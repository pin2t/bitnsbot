package main

import "bytes"
import "context"
import "encoding/json"
import "fmt"
import "net/http"
import "os"
import "strings"
import "sync"
import "time"

import "bitnsbot/logging"

// coreConfig describes how to reach Bitcoin Core's JSON-RPC interface. Either
// user/pass or cookieFile authenticates; the cookie file is what a stock node
// writes into its data directory, so it needs no configuration at all.
type coreConfig struct {
    url        string
    user       string
    pass       string
    cookieFile string
}

// coreClient talks to Bitcoin Core over HTTP JSON-RPC. Unlike btcd there is no
// websocket interface and so no long-lived connection to supervise: every call
// is an independent request, and notifications arrive over ZMQ instead (see
// zmq.go). That removes the reconnection machinery the btcd client needed.
type coreClient struct {
    cfg    coreConfig
    client *http.Client
    mu     sync.Mutex
    auth   string

    blockTxidsCache   *lruCache[string, *coreBlockTxids]
    blockVerboseCache *lruCache[string, *coreVerboseBlock]
}

func newCoreClient(cfg coreConfig) (*coreClient, error) {
    var c = &coreClient{
        cfg: cfg,
        client: &http.Client{
            Transport: &http.Transport{
                MaxIdleConns:        50,
                MaxIdleConnsPerHost: 10,
                MaxConnsPerHost:     20,
                IdleConnTimeout:     60 * time.Second,
                DisableKeepAlives:   false,
            },
        },
        blockTxidsCache:   newLRU[string, *coreBlockTxids](100),
        blockVerboseCache: newLRU[string, *coreVerboseBlock](100),
    }
    if err := c.refreshAuth(); err != nil { return nil, err }
    return c, nil
}

// refreshAuth rebuilds the basic-auth credentials. The cookie file is re-read
// rather than cached forever because Core rewrites it with a fresh password on
// every restart, and the bot outlives node restarts.
func (c *coreClient) refreshAuth() error {
    var user, pass = c.cfg.user, c.cfg.pass
    if c.cfg.cookieFile != "" {
        var data, err = os.ReadFile(c.cfg.cookieFile)
        if err != nil { return fmt.Errorf("read cookie %s: %w", c.cfg.cookieFile, err) }
        var parts = strings.SplitN(strings.TrimSpace(string(data)), ":", 2)
        if len(parts) != 2 { return fmt.Errorf("malformed cookie file %s", c.cfg.cookieFile) }
        user, pass = parts[0], parts[1]
    }
    c.mu.Lock()
    c.auth = basicAuth(user, pass)
    c.mu.Unlock()
    return nil
}

func basicAuth(user, pass string) string {
    var req = &http.Request{Header: http.Header{}}
    req.SetBasicAuth(user, pass)
    return req.Header.Get("Authorization")
}

type coreError struct {
    Code    int    `json:"code"`
    Message string `json:"message"`
}

func (e *coreError) Error() string { return fmt.Sprintf("%s (code %d)", e.Message, e.Code) }

// call performs one JSON-RPC request. Core speaks JSON-RPC 1.0 with positional
// params and reports method errors in the body (with HTTP 500), so a non-200
// status is not on its own a failure — the body is decoded either way.
func (c *coreClient) call(ctx context.Context, method string, params []interface{}, result interface{}) error {
    if params == nil { params = []interface{}{} }
    var body, err = json.Marshal(map[string]interface{}{
        "jsonrpc": "1.0", "id": "bitnsbot", "method": method, "params": params,
    })
    if err != nil { return err }
    logging.Net("core → %s", body)
    var req, reqErr = http.NewRequestWithContext(ctx, http.MethodPost, c.cfg.url, bytes.NewReader(body))
    if reqErr != nil { return reqErr }
    req.Header.Set("Content-Type", "application/json")
    c.mu.Lock()
    req.Header.Set("Authorization", c.auth)
    c.mu.Unlock()
    var resp, doErr = c.client.Do(req)
    if doErr != nil { return doErr }
    defer resp.Body.Close()
    if resp.StatusCode == http.StatusUnauthorized {
        // the node restarted and rotated its cookie: pick up the new one so the
        // next call succeeds rather than failing forever
        if err := c.refreshAuth(); err != nil { return err }
        return fmt.Errorf("unauthorized (credentials reloaded, retry)")
    }
    var decoded struct {
        Result json.RawMessage `json:"result"`
        Error  *coreError      `json:"error"`
    }
    if err := json.NewDecoder(resp.Body).Decode(&decoded); err != nil {
        return fmt.Errorf("%s: %s: %w", method, resp.Status, err)
    }
    logging.Net("core ← %s %s", method, decoded.Result)
    if decoded.Error != nil { return decoded.Error }
    if result == nil { return nil }
    return json.Unmarshal(decoded.Result, result)
}

func (c *coreClient) getBlockCount(ctx context.Context) (int64, error) {
    var count int64
    var err = c.call(ctx, "getblockcount", nil, &count)
    return count, err
}

func (c *coreClient) getBlockHash(ctx context.Context, height int64) (string, error) {
    var hash string
    var err = c.call(ctx, "getblockhash", []interface{}{height}, &hash)
    return hash, err
}

type coreBlockHeader struct {
    Hash   string `json:"hash"`
    Height int64  `json:"height"`
}

// getBlockHeader resolves a block hash to its height, erroring for a hash the
// node has no block for — which is what lets /info tell a block hash from a txid.
func (c *coreClient) getBlockHeader(ctx context.Context, hash string) (*coreBlockHeader, error) {
    var header coreBlockHeader
    var err = c.call(ctx, "getblockheader", []interface{}{hash, true}, &header)
    if err != nil { return nil, err }
    return &header, nil
}

type coreScriptPubKey struct {
    Address string `json:"address"`
    Hex     string `json:"hex"`
    Type    string `json:"type"`
}

type coreVout struct {
    Value        float64          `json:"value"`
    N            uint32           `json:"n"`
    ScriptPubKey coreScriptPubKey `json:"scriptPubKey"`
}

// corePrevOut is the spent output, which Core supplies inline for confirmed
// transactions (getrawtransaction verbosity 2, getblock verbosity 3) — the
// per-input prevout fetching btcd forced on us is not needed for those.
type corePrevOut struct {
    Generated    bool             `json:"generated"`
    Height       int64            `json:"height"`
    Value        float64          `json:"value"`
    ScriptPubKey coreScriptPubKey `json:"scriptPubKey"`
}

type coreVin struct {
    Txid     string       `json:"txid"`
    Vout     uint32       `json:"vout"`
    Coinbase string       `json:"coinbase"`
    PrevOut  *corePrevOut `json:"prevout"`
}

type coreTransaction struct {
    Txid          string     `json:"txid"`
    Hash          string     `json:"hash"`
    Size          int32      `json:"size"`
    Vsize         int32      `json:"vsize"`
    Confirmations uint64     `json:"confirmations"`
    BlockHash     string     `json:"blockhash"`
    Time          int64      `json:"time"`
    Fee           float64    `json:"fee"`
    Vin           []coreVin  `json:"vin"`
    Vout          []coreVout `json:"vout"`
}

// getRawTransaction fetches a transaction. Verbosity 2 additionally carries the
// fee and each input's prevout, but **only for confirmed transactions** — a
// mempool transaction has no undo data, so both are absent there and the fee has
// to come from getMempoolEntry instead. Needs -txindex for transactions outside
// the mempool, exactly as btcd needed it.
func (c *coreClient) getRawTransaction(ctx context.Context, txid string) (*coreTransaction, error) {
    var tx coreTransaction
    var err = c.call(ctx, "getrawtransaction", []interface{}{txid, 2}, &tx)
    if err != nil { return nil, err }
    return &tx, nil
}

func (c *coreClient) decodeRawTransaction(ctx context.Context, txHex string) (*coreTransaction, error) {
    var tx coreTransaction
    var err = c.call(ctx, "decoderawtransaction", []interface{}{txHex}, &tx)
    if err != nil { return nil, err }
    return &tx, nil
}

type coreBlockTxids struct {
    Height     int64    `json:"height"`
    Difficulty float64  `json:"difficulty"`
    Tx         []string `json:"tx"`
}

// getBlockTxids is getblock at verbosity 1: the header fields plus the txids
// only, which is all the confirmation check and the miner collector need.
func (c *coreClient) getBlockTxids(ctx context.Context, hash string) (*coreBlockTxids, error) {
    c.mu.Lock()
    if cached, ok := c.blockTxidsCache.Get(hash); ok {
        c.mu.Unlock()
        return cached, nil
    }
    c.mu.Unlock()

    var blk coreBlockTxids
    var err = c.call(ctx, "getblock", []interface{}{hash, 1}, &blk)
    if err != nil { return nil, err }

    c.mu.Lock()
    c.blockTxidsCache.Put(hash, &blk)
    c.mu.Unlock()
    return &blk, nil
}

type coreVerboseBlock struct {
    Hash       string            `json:"hash"`
    Height     int64             `json:"height"`
    Time       int64             `json:"time"`
    Size       int32             `json:"size"`
    Difficulty float64           `json:"difficulty"`
    Tx         []coreTransaction `json:"tx"`
}

// getBlockVerbose is getblock at verbosity 2. Two differences from btcd worth
// knowing: the full transactions live under "tx" (btcd put them under "rawtx"),
// and every non-coinbase transaction already carries its "fee" — so the block's
// fee distribution needs no prevout fetching at all.
func (c *coreClient) getBlockVerbose(ctx context.Context, hash string) (*coreVerboseBlock, error) {
    c.mu.Lock()
    if cached, ok := c.blockVerboseCache.Get(hash); ok {
        c.mu.Unlock()
        return cached, nil
    }
    c.mu.Unlock()

    var blk coreVerboseBlock
    var err = c.call(ctx, "getblock", []interface{}{hash, 2}, &blk)
    if err != nil { return nil, err }

    c.mu.Lock()
    c.blockVerboseCache.Put(hash, &blk)
    c.mu.Unlock()
    return &blk, nil
}

type coreAddressInfo struct {
    IsValid      bool   `json:"isvalid"`
    Address      string `json:"address"`
    ScriptPubKey string `json:"scriptPubKey"`
    IsScript     bool   `json:"isscript"`
    IsWitness    bool   `json:"iswitness"`
}

// validateAddress also returns the address's scriptPubKey, which is what makes
// local matching of ZMQ-delivered transactions possible without decoding any
// address format in the bot.
func (c *coreClient) validateAddress(ctx context.Context, address string) (*coreAddressInfo, error) {
    var info coreAddressInfo
    var err = c.call(ctx, "validateaddress", []interface{}{address}, &info)
    if err != nil { return nil, err }
    return &info, nil
}

type coreMempoolInfo struct {
    Size  int64 `json:"size"`
    Bytes int64 `json:"bytes"`
    // MempoolMinFee is the node's purge threshold in BTC/kvB — the rate below
    // which it will not even keep a transaction, so no recommendation may sit
    // under it.
    MempoolMinFee float64 `json:"mempoolminfee"`
}

func (c *coreClient) getMempoolInfo(ctx context.Context) (*coreMempoolInfo, error) {
    var info coreMempoolInfo
    var err = c.call(ctx, "getmempoolinfo", nil, &info)
    if err != nil { return nil, err }
    return &info, nil
}

type coreMempoolEntry struct {
    Vsize int32 `json:"vsize"`
    // Weight is what projected blocks are packed by — a block's limit is 4M
    // weight units, and vsize is only weight/4 rounded up.
    Weight int64 `json:"weight"`
    // AncestorSize/Fees.Ancestor cover the whole unconfirmed package, which is
    // what a miner actually maximises: a low-fee parent rides in on a high-fee
    // child (CPFP). For the common case of a transaction with no unconfirmed
    // parents these equal Vsize and Fees.Base.
    AncestorSize int64 `json:"ancestorsize"`
    Fees         struct {
        Base     float64 `json:"base"`
        Ancestor float64 `json:"ancestor"`
    } `json:"fees"`
}

// getMempoolEntry is where an *unconfirmed* transaction's fee comes from, since
// getrawtransaction can't compute one without undo data.
func (c *coreClient) getMempoolEntry(ctx context.Context, txid string) (*coreMempoolEntry, error) {
    var entry coreMempoolEntry
    var err = c.call(ctx, "getmempoolentry", []interface{}{txid}, &entry)
    if err != nil { return nil, err }
    return &entry, nil
}

func (c *coreClient) rawMempoolVerbose(ctx context.Context) (map[string]coreMempoolEntry, error) {
    var mp map[string]coreMempoolEntry
    var err = c.call(ctx, "getrawmempool", []interface{}{true}, &mp)
    if err != nil { return nil, err }
    return mp, nil
}

type coreFeeEstimate struct {
    FeeRate float64  `json:"feerate"`
    Blocks  int64    `json:"blocks"`
    Errors  []string `json:"errors"`
}

// estimateSmartFee returns the fee rate in BTC/kvB for a confirmation target.
// Core reports "not enough data" as an *errors* array with no feerate rather
// than an RPC error, so an unusable estimate is a zero FeeRate, not a failure.
func (c *coreClient) estimateSmartFee(ctx context.Context, blocks int64) (float64, error) {
    var estimate coreFeeEstimate
    var err = c.call(ctx, "estimatesmartfee", []interface{}{blocks}, &estimate)
    if err != nil { return 0, err }
    return estimate.FeeRate, nil
}

type coreScanResult struct {
    Success  bool `json:"success"`
    Unspents []struct {
        Txid         string  `json:"txid"`
        Vout         uint32  `json:"vout"`
        ScriptPubKey string  `json:"scriptPubKey"`
        Amount       float64 `json:"amount"`
        Height       int64   `json:"height"`
    } `json:"unspents"`
}

// scanTxOutSet finds the current unspent outputs of the given addresses by
// scanning the UTXO set. Core has no address index, so this is how the watch
// notifier learns which outpoints a watched address currently owns — many
// addresses can be scanned in one pass, which matters because the scan walks the
// whole UTXO set and takes minutes on mainnet.
func (c *coreClient) scanTxOutSet(ctx context.Context, addresses []string) (*coreScanResult, error) {
    var descriptors = make([]string, 0, len(addresses))
    for _, a := range addresses {
        descriptors = append(descriptors, "addr("+a+")")
    }
    var result coreScanResult
    var err = c.call(ctx, "scantxoutset", []interface{}{"start", descriptors}, &result)
    if err != nil { return nil, err }
    return &result, nil
}

// waitForBlock is Core's long-poll for a new tip. It is not used for the block
// notifications themselves (ZMQ delivers those) but gives tests a way to wait on
// the node without polling.
func (c *coreClient) waitForBlock(ctx context.Context, timeout time.Duration) (*coreBlockHeader, error) {
    var header coreBlockHeader
    var err = c.call(ctx, "waitfornewblock", []interface{}{timeout.Milliseconds()}, &header)
    if err != nil { return nil, err }
    return &header, nil
}

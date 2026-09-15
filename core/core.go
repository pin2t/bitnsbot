// Package core talks to Bitcoin Core over HTTP JSON-RPC. There is one node per
// process, so the connection is the package's own: Init points it at the node,
// and every function below calls through it. Unlike btcd there is no websocket
// interface and so no long-lived connection to supervise: every call is an
// independent request, and notifications arrive over ZMQ instead. That removes
// the reconnection machinery the btcd client needed.
package core

import "bytes"
import "context"
import "errors"
import "encoding/json"
import "fmt"
import "net/http"
import "os"
import "strings"
import "sync"
import "sync/atomic"
import "time"
import "bitnsbot/logging"
import "bitnsbot/lru"

// conn is the node the package talks to. It is swapped whole rather than
// edited, so a call already in flight finishes against the node it started on.
type conn struct {
    url, user, pass, cookie string
    client *http.Client
    mu     sync.Mutex
    auth   string

    blockTxidsCache   *lru.Cache[string, *BlockTxids]
    blockVerboseCache *lru.Cache[string, *VerboseBlock]
}

var current atomic.Pointer[conn]

var errUnconfigured = errors.New("bitcoin core is not configured")

// Init points the package at a node. The cookie is read here, so a bad path
// fails now rather than on the first call.
func Init(url, user, pass, cookie string) error {
    var c = &conn{
        url: url, user: user, pass: pass, cookie: cookie,
        client: &http.Client{
            Transport: &http.Transport{
                MaxIdleConns:        50,
                MaxIdleConnsPerHost: 10,
                MaxConnsPerHost:     20,
                IdleConnTimeout:     60 * time.Second,
                DisableKeepAlives:   false,
            },
        },
        blockTxidsCache:   lru.New[string, *BlockTxids](100),
        blockVerboseCache: lru.New[string, *VerboseBlock](100),
    }
    if err := c.refreshAuth(); err != nil { return err }
    current.Store(c)
    return nil
}

// Enabled reports whether Init has pointed the package at a node. The bot runs
// without one, and everything that needs it checks this first.
func Enabled() bool { return current.Load() != nil }

// Reset forgets the node, so a test leaves the next one with none.
func Reset() { current.Store(nil) }

// refreshAuth rebuilds the basic-auth credentials. The cookie file is re-read
// rather than cached forever because Core rewrites it with a fresh password on
// every restart, and the bot outlives node restarts.
func (c *conn) refreshAuth() error {
    var user, pass = c.user, c.pass
    if c.cookie != "" {
        var data, err = os.ReadFile(c.cookie)
        if err != nil { return fmt.Errorf("read cookie %s: %w", c.cookie, err) }
        var parts = strings.SplitN(strings.TrimSpace(string(data)), ":", 2)
        if len(parts) != 2 { return fmt.Errorf("malformed cookie file %s", c.cookie) }
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

type Error struct {
    Code    int    `json:"code"`
    Message string `json:"message"`
}

func (e *Error) Error() string { return fmt.Sprintf("%s (code %d)", e.Message, e.Code) }

// Call performs one JSON-RPC request against the node Init named. Core speaks
// JSON-RPC 1.0 with positional params and reports method errors in the body
// (with HTTP 500), so a non-200 status is not on its own a failure — the body is
// decoded either way.
func Call(ctx context.Context, method string, params []interface{}, result interface{}) error {
    var c = current.Load()
    if c == nil { return errUnconfigured }
    return c.call(ctx, method, params, result)
}

// call is Call against one node, which is what lets GetBlockTxids and
// GetBlockVerbose use the cache of the node they asked.
//
// the node restarted and rotated its cookie: pick up the new one so the
// next call succeeds rather than failing forever
func (c *conn) call(ctx context.Context, method string, params []interface{}, result interface{}) error {
    if params == nil { params = []interface{}{} }
    var body, err = json.Marshal(map[string]interface{}{
        "jsonrpc": "1.0", "id": "bitnsbot", "method": method, "params": params,
    })
    if err != nil { return err }
    logging.Net("core → %s", body)
    var req, reqErr = http.NewRequestWithContext(ctx, http.MethodPost, c.url, bytes.NewReader(body))
    if reqErr != nil { return reqErr }
    req.Header.Set("Content-Type", "application/json")
    c.mu.Lock()
    req.Header.Set("Authorization", c.auth)
    c.mu.Unlock()
    var resp, doErr = c.client.Do(req)
    if doErr != nil { return doErr }
    defer resp.Body.Close()
    if resp.StatusCode == http.StatusUnauthorized {
        if err := c.refreshAuth(); err != nil { return err }
        return fmt.Errorf("unauthorized (credentials reloaded, retry)")
    }
    var decoded struct {
        Result json.RawMessage `json:"result"`
        Error  *Error          `json:"error"`
    }
    if err := json.NewDecoder(resp.Body).Decode(&decoded); err != nil {
        return fmt.Errorf("%s: %s: %w", method, resp.Status, err)
    }
    logging.Net("core ← %s %s", method, decoded.Result)
    if decoded.Error != nil { return decoded.Error }
    if result == nil { return nil }
    return json.Unmarshal(decoded.Result, result)
}

func GetBlockCount(ctx context.Context) (int64, error) {
    var count int64
    var err = Call(ctx, "getblockcount", nil, &count)
    return count, err
}

func GetBlockHash(ctx context.Context, height int64) (string, error) {
    var hash string
    var err = Call(ctx, "getblockhash", []interface{}{height}, &hash)
    return hash, err
}

type BlockHeader struct {
    Hash   string `json:"hash"`
    Height int64  `json:"height"`
}

// GetBlockHeader resolves a block hash to its height, erroring for a hash the
// node has no block for — which is what lets /info tell a block hash from a txid.
func GetBlockHeader(ctx context.Context, hash string) (*BlockHeader, error) {
    var header BlockHeader
    var err = Call(ctx, "getblockheader", []interface{}{hash, true}, &header)
    if err != nil { return nil, err }
    return &header, nil
}

type ScriptPubKey struct {
    Address string `json:"address"`
    Hex     string `json:"hex"`
    Type    string `json:"type"`
}

type Vout struct {
    Value        float64          `json:"value"`
    N            uint32           `json:"n"`
    ScriptPubKey ScriptPubKey `json:"scriptPubKey"`
}

// PrevOut is the spent output, which Core supplies inline for confirmed
// transactions (getrawtransaction verbosity 2, getblock verbosity 3) — the
// per-input prevout fetching btcd forced on us is not needed for those.
type PrevOut struct {
    Generated    bool             `json:"generated"`
    Height       int64            `json:"height"`
    Value        float64          `json:"value"`
    ScriptPubKey ScriptPubKey `json:"scriptPubKey"`
}

type Vin struct {
    Txid     string       `json:"txid"`
    Vout     uint32       `json:"vout"`
    Coinbase string       `json:"coinbase"`
    PrevOut  *PrevOut `json:"prevout"`
}

type Transaction struct {
    Txid          string     `json:"txid"`
    Hash          string     `json:"hash"`
    Size          int32      `json:"size"`
    Vsize         int32      `json:"vsize"`
    Confirmations uint64     `json:"confirmations"`
    BlockHash     string     `json:"blockhash"`
    Time          int64      `json:"time"`
    Fee           float64    `json:"fee"`
    Vin           []Vin  `json:"vin"`
    Vout          []Vout `json:"vout"`
}

// GetRawTransaction fetches a transaction. Verbosity 2 additionally carries the
// fee and each input's prevout, but **only for confirmed transactions** — a
// mempool transaction has no undo data, so both are absent there and the fee has
// to come from getMempoolEntry instead. Needs -txindex for transactions outside
// the mempool, exactly as btcd needed it.
func GetRawTransaction(ctx context.Context, txid string) (*Transaction, error) {
    var tx Transaction
    var err = Call(ctx, "getrawtransaction", []interface{}{txid, 2}, &tx)
    if err != nil { return nil, err }
    return &tx, nil
}

func DecodeRawTransaction(ctx context.Context, txHex string) (*Transaction, error) {
    var tx Transaction
    var err = Call(ctx, "decoderawtransaction", []interface{}{txHex}, &tx)
    if err != nil { return nil, err }
    return &tx, nil
}

type BlockTxids struct {
    Height     int64    `json:"height"`
    Difficulty float64  `json:"difficulty"`
    Tx         []string `json:"tx"`
}

// GetBlockTxids is getblock at verbosity 1: the header fields plus the txids
// only, which is all the confirmation check and the miner collector need.
func GetBlockTxids(ctx context.Context, hash string) (*BlockTxids, error) {
    var c = current.Load()
    if c == nil { return nil, errUnconfigured }
    c.mu.Lock()
    if cached, ok := c.blockTxidsCache.Get(hash); ok {
        c.mu.Unlock()
        return cached, nil
    }
    c.mu.Unlock()
    var blk BlockTxids
    var err = c.call(ctx, "getblock", []interface{}{hash, 1}, &blk)
    if err != nil { return nil, err }
    c.mu.Lock()
    c.blockTxidsCache.Put(hash, &blk)
    c.mu.Unlock()
    return &blk, nil
}

type VerboseBlock struct {
    Hash       string            `json:"hash"`
    Height     int64             `json:"height"`
    Time       int64             `json:"time"`
    Size       int32             `json:"size"`
    Difficulty float64           `json:"difficulty"`
    Tx         []Transaction     `json:"tx"`
}

// GetBlockVerbose is getblock at verbosity 2. Two differences from btcd worth
// knowing: the full transactions live under "tx" (btcd put them under "rawtx"),
// and every non-coinbase transaction already carries its "fee" — so the block's
// fee distribution needs no prevout fetching at all.
func GetBlockVerbose(ctx context.Context, hash string) (*VerboseBlock, error) {
    var c = current.Load()
    if c == nil { return nil, errUnconfigured }
    c.mu.Lock()
    if cached, ok := c.blockVerboseCache.Get(hash); ok {
        c.mu.Unlock()
        return cached, nil
    }
    c.mu.Unlock()
    var blk VerboseBlock
    var err = c.call(ctx, "getblock", []interface{}{hash, 2}, &blk)
    if err != nil { return nil, err }
    c.mu.Lock()
    c.blockVerboseCache.Put(hash, &blk)
    c.mu.Unlock()
    return &blk, nil
}

type AddressInfo struct {
    IsValid      bool   `json:"isvalid"`
    Address      string `json:"address"`
    ScriptPubKey string `json:"scriptPubKey"`
    IsScript     bool   `json:"isscript"`
    IsWitness    bool   `json:"iswitness"`
}

// ValidateAddress also returns the address's scriptPubKey, which is what makes
// local matching of ZMQ-delivered transactions possible without decoding any
// address format in the bot.
func ValidateAddress(ctx context.Context, address string) (*AddressInfo, error) {
    var info AddressInfo
    var err = Call(ctx, "validateaddress", []interface{}{address}, &info)
    if err != nil { return nil, err }
    return &info, nil
}

type MempoolInfo struct {
    Size  int64 `json:"size"`
    Bytes int64 `json:"bytes"`
    // MempoolMinFee is the node's purge threshold in BTC/kvB — the rate below
    // which it will not even keep a transaction, so no recommendation may sit
    // under it.
    MempoolMinFee float64 `json:"mempoolminfee"`
}

type ChainInfo struct {
    Blocks     int64 `json:"blocks"`
    SizeOnDisk int64 `json:"size_on_disk"`
}

type ChainTxStats struct {
    TxCount int64 `json:"txcount"`
}

func GetChainTxStats(ctx context.Context) (*ChainTxStats, error) {
    var stats ChainTxStats
    var err = Call(ctx, "getchaintxstats", nil, &stats)
    if err != nil { return nil, err }
    return &stats, nil
}

func GetBlockchainInfo(ctx context.Context) (*ChainInfo, error) {
    var info ChainInfo
    var err = Call(ctx, "getblockchaininfo", nil, &info)
    if err != nil { return nil, err }
    return &info, nil
}

// NodeAddress is one entry of the node's address manager. Time is when the
// node was last seen — gossiped, not verified, so this is the node's own view of
// the network rather than a reachability scan.
type NodeAddress struct {
    Time    int64  `json:"time"`
    Network string `json:"network"`
}

// GetNodeAddresses asks for every address the node knows (count 0 means all).
// On mainnet that is tens of thousands of entries and several megabytes, so it
// belongs in a background refresh, never in a request path.
func GetNodeAddresses(ctx context.Context) ([]NodeAddress, error) {
    var addrs []NodeAddress
    var err = Call(ctx, "getnodeaddresses", []interface{}{0}, &addrs)
    if err != nil { return nil, err }
    return addrs, nil
}

func GetMempoolInfo(ctx context.Context) (*MempoolInfo, error) {
    var info MempoolInfo
    var err = Call(ctx, "getmempoolinfo", nil, &info)
    if err != nil { return nil, err }
    return &info, nil
}

type MempoolEntry struct {
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

// GetMempoolEntry is where an *unconfirmed* transaction's fee comes from, since
// getrawtransaction can't compute one without undo data.
func GetMempoolEntry(ctx context.Context, txid string) (*MempoolEntry, error) {
    var entry MempoolEntry
    var err = Call(ctx, "getmempoolentry", []interface{}{txid}, &entry)
    if err != nil { return nil, err }
    return &entry, nil
}

func RawMempoolVerbose(ctx context.Context) (map[string]MempoolEntry, error) {
    var mp map[string]MempoolEntry
    var err = Call(ctx, "getrawmempool", []interface{}{true}, &mp)
    if err != nil { return nil, err }
    return mp, nil
}

type ScanResult struct {
    Success  bool `json:"success"`
    Unspents []struct {
        Txid         string  `json:"txid"`
        Vout         uint32  `json:"vout"`
        ScriptPubKey string  `json:"scriptPubKey"`
        Amount       float64 `json:"amount"`
        Height       int64   `json:"height"`
    } `json:"unspents"`
}

// ScanTxOutSet finds the current unspent outputs of the given addresses by
// scanning the UTXO set. Core has no address index, so this is how the watch
// notifier learns which outpoints a watched address currently owns — many
// addresses can be scanned in one pass, which matters because the scan walks the
// whole UTXO set and takes minutes on mainnet.
func ScanTxOutSet(ctx context.Context, addresses []string) (*ScanResult, error) {
    var descriptors = make([]string, 0, len(addresses))
    for _, a := range addresses {
        descriptors = append(descriptors, "addr("+a+")")
    }
    var result ScanResult
    var err = Call(ctx, "scantxoutset", []interface{}{"start", descriptors}, &result)
    if err != nil { return nil, err }
    return &result, nil
}

// WaitForBlock is Core's long-poll for a new tip. It is not used for the block
// notifications themselves (ZMQ delivers those) but gives tests a way to wait on
// the node without polling.
func WaitForBlock(ctx context.Context, timeout time.Duration) (*BlockHeader, error) {
    var header BlockHeader
    var err = Call(ctx, "waitfornewblock", []interface{}{timeout.Milliseconds()}, &header)
    if err != nil { return nil, err }
    return &header, nil
}

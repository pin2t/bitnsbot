package main

import "context"
import "crypto/tls"
import "crypto/x509"
import "encoding/base64"
import "fmt"
import "net/http"
import "os"
import "strings"
import "sync"
import "sync/atomic"
import "time"

import "github.com/gorilla/websocket"
import "github.com/sourcegraph/jsonrpc2"
import "bitnsbot/logging"
import jsonrpc2ws "github.com/sourcegraph/jsonrpc2/websocket"

type btcdConfig struct {
    url         string
    user        string
    pass        string
    certFile    string
    insecureTLS bool
}

// btcdClient is a long-lived handle whose underlying jsonrpc2 connection is
// atomically swapped by supervise() on reconnect, so callers keep using the same
// *btcdClient (and every `btcd.foo()` call site stays unchanged) while the dead
// connection is replaced beneath them. cfg/handler are retained so a reconnect
// can redial identically; stop ends the supervise goroutine.
type btcdClient struct {
    conn     atomic.Pointer[jsonrpc2.Conn]
    cfg      btcdConfig
    handler  jsonrpc2.Handler
    stop     chan struct{}
    stopOnce sync.Once
}

// dialConn establishes a single jsonrpc2 connection to btcd over websocket.
func dialConn(ctx context.Context, cfg btcdConfig, handler jsonrpc2.Handler) (*jsonrpc2.Conn, error) {
    var header = http.Header{}
    header.Set("Authorization", "Basic "+base64.StdEncoding.EncodeToString([]byte(cfg.user+":"+cfg.pass)))
    var dialer = websocket.Dialer{HandshakeTimeout: 15 * time.Second}
    if strings.HasPrefix(cfg.url, "wss://") {
        var tlsConfig, err = btcdTLSConfig(cfg)
        if err != nil { return nil, err }
        dialer.TLSClientConfig = tlsConfig
    }
    var wsConn, _, err = dialer.DialContext(ctx, cfg.url, header)
    if err != nil { return nil, err }
    var stream = jsonrpc2ws.NewObjectStream(wsConn)
    var opts []jsonrpc2.ConnOpt
    if *verbose >= 2 {
        opts = append(opts,
            jsonrpc2.OnSend(func(req *jsonrpc2.Request, resp *jsonrpc2.Response) { logging.Net("btcd → %s", btcdMsg(req, resp)) }),
            jsonrpc2.OnRecv(func(req *jsonrpc2.Request, resp *jsonrpc2.Response) { logging.Net("btcd ← %s", btcdMsg(req, resp)) }),
        )
    }
    return jsonrpc2.NewConn(ctx, stream, handler, opts...), nil
}

func dialBtcd(ctx context.Context, cfg btcdConfig, handler jsonrpc2.Handler) (*btcdClient, error) {
    var conn, err = dialConn(ctx, cfg, handler)
    if err != nil { return nil, err }
    var c = &btcdClient{cfg: cfg, handler: handler, stop: make(chan struct{})}
    c.conn.Store(conn)
    return c, nil
}

// btcdPingInterval is how often supervise health-checks the connection. A
// package var so tests can shrink it.
var btcdPingInterval = 10 * time.Second

// supervise pings btcd every btcdPingInterval and, when a ping fails, reconnects
// — swapping in a fresh connection and reapplying the stateful subscriptions
// (transaction filter + block notification) via reapply. Started from main()
// after the initial dial; the goroutine exits when close() is called.
func (c *btcdClient) supervise(reapply func()) {
    go func() {
        var ticker = time.NewTicker(btcdPingInterval)
        defer ticker.Stop()
        for {
            select {
            case <-c.stop:
                return
            case <-ticker.C:
                var ctx, cancel = context.WithTimeout(context.Background(), 5*time.Second)
                var err = c.ping(ctx)
                cancel()
                if err == nil { continue }
                logging.Warn("btcd ping failed: %v — reconnecting", err)
                if c.reconnect() {
                    reapply()
                }
            }
        }
    }()
}

// ping is a lightweight round-trip that fails if the connection is dead.
func (c *btcdClient) ping(ctx context.Context) error {
    var count int64
    return c.conn.Load().Call(ctx, "getblockcount", []interface{}{}, &count)
}

// reconnect dials a fresh connection and swaps it in for the dead one, closing
// the old one. Returns false (retried on the next tick) if the dial failed or
// close() fired mid-dial.
func (c *btcdClient) reconnect() bool {
    var ctx, cancel = context.WithTimeout(context.Background(), 15*time.Second)
    defer cancel()
    var conn, err = dialConn(ctx, c.cfg, c.handler)
    if err != nil {
        logging.Err("btcd reconnect failed: %v", err)
        return false
    }
    select {
    case <-c.stop:
        conn.Close()
        return false
    default:
    }
    var old = c.conn.Swap(conn)
    if old != nil { old.Close() }
    logging.Status("reconnected to btcd at %s", c.cfg.url)
    return true
}

// btcdMsg formats a logged jsonrpc2 message. OnRecv passes both the original
// request and the response, so a response is preferred when present; a bare
// request (OnSend, or a received notification) falls through to method+params.
func btcdMsg(req *jsonrpc2.Request, resp *jsonrpc2.Response) string {
    if resp != nil {
        if resp.Error != nil { return "error " + resp.Error.Error() }
        if resp.Result != nil { return string(*resp.Result) }
        return ""
    }
    if req != nil {
        var params = ""
        if req.Params != nil { params = string(*req.Params) }
        return strings.TrimSpace(req.Method + " " + params)
    }
    return ""
}

func btcdTLSConfig(cfg btcdConfig) (*tls.Config, error) {
    if cfg.insecureTLS {
        return &tls.Config{InsecureSkipVerify: true}, nil
    }
    if cfg.certFile == "" {
        return &tls.Config{}, nil
    }
    var pem, err = os.ReadFile(cfg.certFile)
    if err != nil { return nil, err }
    var pool = x509.NewCertPool()
    if !pool.AppendCertsFromPEM(pem) {
        return nil, fmt.Errorf("no certificates found in %s", cfg.certFile)
    }
    return &tls.Config{RootCAs: pool}, nil
}

func (c *btcdClient) close() error {
    c.stopOnce.Do(func() { close(c.stop) })
    return c.conn.Load().Close()
}

func (c *btcdClient) getBlockCount(ctx context.Context) (int64, error) {
    var count int64
    var err = c.conn.Load().Call(ctx, "getblockcount", []interface{}{}, &count)
    return count, err
}

// estimateFee returns the fee, in BTC per kilobyte, needed for a transaction to
// confirm within numBlocks blocks. btcd's estimator errors ("not enough blocks
// have been observed") until the node has seen enough mempool activity.
func (c *btcdClient) estimateFee(ctx context.Context, numBlocks int) (float64, error) {
    var btcPerKB float64
    var err = c.conn.Load().Call(ctx, "estimatefee", []interface{}{numBlocks}, &btcPerKB)
    return btcPerKB, err
}

type btcdVout struct {
    Value        float64          `json:"value"`
    ScriptPubKey btcdScriptPubKey `json:"scriptPubKey"`
}

type btcdTransaction struct {
    Txid          string     `json:"txid"`
    Hash          string     `json:"hash"`
    Confirmations uint64     `json:"confirmations"`
    BlockHash     string     `json:"blockhash"`
    Time          int64      `json:"time"`
    Size          int32      `json:"size"`
    Vsize         int32      `json:"vsize"`
    Vin           []btcdVin  `json:"vin"`
    Vout          []btcdVout `json:"vout"`
}

func (c *btcdClient) getRawTransaction(ctx context.Context, txid string) (*btcdTransaction, error) {
    var tx btcdTransaction
    var err = c.conn.Load().Call(ctx, "getrawtransaction", []interface{}{txid, 1}, &tx)
    if err != nil { return nil, err }
    return &tx, nil
}

type btcdMempoolInfo struct {
    Size  int64 `json:"size"`  // number of transactions in the mempool
    Bytes int64 `json:"bytes"` // total serialized size of the mempool
}

func (c *btcdClient) getMempoolInfo(ctx context.Context) (*btcdMempoolInfo, error) {
    var info btcdMempoolInfo
    var err = c.conn.Load().Call(ctx, "getmempoolinfo", []interface{}{}, &info)
    if err != nil { return nil, err }
    return &info, nil
}

type btcdMempoolEntry struct {
    Fee  float64 `json:"fee"`
    Time int64   `json:"time"`
}

// rawMempoolVerbose fetches every mempool entry (txid → {fee, time}). btcd has no
// getmempoolentry, so this whole-mempool fetch is the only way to read a single
// entry's fields (mempoolTime) or aggregate across the mempool (/mempool totals).
func (c *btcdClient) rawMempoolVerbose(ctx context.Context) (map[string]btcdMempoolEntry, error) {
    var mp map[string]btcdMempoolEntry
    var err = c.conn.Load().Call(ctx, "getrawmempool", []interface{}{true}, &mp)
    return mp, err
}

// mempoolTime returns when this node first accepted txid into its mempool —
// worth doing only for an unconfirmed transaction, whose getrawtransaction
// carries no time of its own.
func (c *btcdClient) mempoolTime(ctx context.Context, txid string) (int64, bool) {
    var mp, err = c.rawMempoolVerbose(ctx)
    if err != nil { return 0, false }
    var e, ok = mp[txid]
    return e.Time, ok
}

func (c *btcdClient) getBlockHash(ctx context.Context, height int64) (string, error) {
    var hash string
    var err = c.conn.Load().Call(ctx, "getblockhash", []interface{}{height}, &hash)
    return hash, err
}

type btcdBlockHeader struct {
    Hash   string `json:"hash"`
    Height int64  `json:"height"`
}

// getBlockHeader resolves a block hash to its height. It is an O(1) index lookup
// returning just the header, which is what makes it usable as the "is this a
// block hash?" probe /info needs — see blockHeight there.
func (c *btcdClient) getBlockHeader(ctx context.Context, hash string) (*btcdBlockHeader, error) {
    var header btcdBlockHeader
    var err = c.conn.Load().Call(ctx, "getblockheader", []interface{}{hash, true}, &header)
    if err != nil { return nil, err }
    return &header, nil
}

type btcdVin struct {
    Txid     string `json:"txid"`
    Vout     uint32 `json:"vout"`
    Coinbase string `json:"coinbase"`
}

type btcdBlockTx struct {
    Txid string     `json:"txid"`
    Size int32      `json:"size"`
    Vin  []btcdVin  `json:"vin"`
    Vout []btcdVout `json:"vout"`
}

type btcdVerboseBlock struct {
    Hash       string  `json:"hash"`
    Height     int64   `json:"height"`
    Time       int64   `json:"time"`
    Size       int32   `json:"size"`
    Difficulty float64 `json:"difficulty"`
    // btcd puts the full transactions under "rawtx" at verbosity 2; "tx" holds
    // only txids (verbosity 1). Bitcoin Core uses "tx" for both — this is a
    // btcd-specific quirk, caught against a real regtest node.
    Tx []btcdBlockTx `json:"rawtx"`
}

// getBlockVerbose fetches a block with full transaction details (getblock
// verbosity 2), listing every tx's inputs and outputs. Inputs still reference
// their prevouts by txid:vout with no value, so computing fees requires fetching
// those prevout transactions separately.
func (c *btcdClient) getBlockVerbose(ctx context.Context, hash string) (*btcdVerboseBlock, error) {
    var blk btcdVerboseBlock
    var err = c.conn.Load().Call(ctx, "getblock", []interface{}{hash, 2}, &blk)
    if err != nil { return nil, err }
    return &blk, nil
}

type btcdBlockTxids struct {
    Height     int64    `json:"height"`
    Difficulty float64  `json:"difficulty"`
    Tx         []string `json:"tx"`
}

// getBlockTxids fetches a block at verbosity 1, where "tx" carries only the
// txids — far cheaper than getBlockVerbose (verbosity 2, full transactions) when
// all that's wanted is which transactions the block contains.
func (c *btcdClient) getBlockTxids(ctx context.Context, hash string) (*btcdBlockTxids, error) {
    var blk btcdBlockTxids
    var err = c.conn.Load().Call(ctx, "getblock", []interface{}{hash, 1}, &blk)
    if err != nil { return nil, err }
    return &blk, nil
}

type btcdAddressInfo struct {
    IsValid   bool   `json:"isvalid"`
    Address   string `json:"address"`
    IsScript  bool   `json:"isscript"`
    IsWitness bool   `json:"iswitness"`
}

func (c *btcdClient) validateAddress(ctx context.Context, address string) (*btcdAddressInfo, error) {
    var info btcdAddressInfo
    var err = c.conn.Load().Call(ctx, "validateaddress", []interface{}{address}, &info)
    if err != nil { return nil, err }
    return &info, nil
}

type btcdPrevOut struct {
    Addresses []string `json:"addresses"`
    Value     float64  `json:"value"`
}

type btcdAddrVin struct {
    Coinbase string       `json:"coinbase"`
    Txid     string       `json:"txid"`
    Vout     uint32       `json:"vout"`
    PrevOut  *btcdPrevOut `json:"prevOut"`
}

type btcdAddrTx struct {
    Txid string        `json:"txid"`
    Time int64         `json:"time"`
    Vin  []btcdAddrVin `json:"vin"`
    Vout []btcdVout    `json:"vout"`
}

// searchAddressTxs returns a page of the address's transactions (verbose, oldest
// first), each input carrying its previous-output value and addresses (vinextra),
// so callers can sum sent amounts and fees. Needs btcd's --addrindex (and
// --txindex for the prevouts). An address with no history returns an RPC error
// ("No information available about address"), which is also how paging past the
// end reports itself.
func (c *btcdClient) searchAddressTxs(ctx context.Context, address string, skip, count int) ([]btcdAddrTx, error) {
    var txs []btcdAddrTx
    var err = c.conn.Load().Call(ctx, "searchrawtransactions", []interface{}{address, 1, skip, count, 1, false}, &txs)
    if err != nil { return nil, err }
    return txs, nil
}

type btcdScriptPubKey struct {
    Address   string   `json:"address"`
    Addresses []string `json:"addresses"`
}

type btcdDecodedVout struct {
    Value        float64          `json:"value"`
    ScriptPubKey btcdScriptPubKey `json:"scriptPubKey"`
}

type btcdDecodedTx struct {
    Txid string            `json:"txid"`
    Vout []btcdDecodedVout `json:"vout"`
}

func (c *btcdClient) decodeRawTransaction(ctx context.Context, txHex string) (*btcdDecodedTx, error) {
    var tx btcdDecodedTx
    var err = c.conn.Load().Call(ctx, "decoderawtransaction", []interface{}{txHex}, &tx)
    if err != nil { return nil, err }
    return &tx, nil
}

type btcdOutpoint struct {
    Hash  string `json:"hash"`
    Index uint32 `json:"index"`
}

func (c *btcdClient) loadTxFilter(ctx context.Context, reload bool, addresses []string, outpoints []btcdOutpoint) error {
    if addresses == nil {
        addresses = []string{}
    }
    if outpoints == nil {
        outpoints = []btcdOutpoint{}
    }
    return c.conn.Load().Call(ctx, "loadtxfilter", []interface{}{reload, addresses, outpoints}, nil)
}

func (c *btcdClient) notifyBlocks(ctx context.Context) error {
    return c.conn.Load().Call(ctx, "notifyblocks", []interface{}{}, nil)
}

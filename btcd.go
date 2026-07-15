package main

import "context"
import "crypto/tls"
import "crypto/x509"
import "encoding/base64"
import "fmt"
import "net/http"
import "os"
import "strings"
import "time"

import "github.com/gorilla/websocket"
import "github.com/sourcegraph/jsonrpc2"
import jsonrpc2ws "github.com/sourcegraph/jsonrpc2/websocket"

type btcdConfig struct {
    url         string
    user        string
    pass        string
    certFile    string
    insecureTLS bool
}

type btcdClient struct {
    conn *jsonrpc2.Conn
}

func dialBtcd(ctx context.Context, cfg btcdConfig, handler jsonrpc2.Handler) (*btcdClient, error) {
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
            jsonrpc2.OnSend(func(req *jsonrpc2.Request, resp *jsonrpc2.Response) { logNet("btcd → %s", btcdMsg(req, resp)) }),
            jsonrpc2.OnRecv(func(req *jsonrpc2.Request, resp *jsonrpc2.Response) { logNet("btcd ← %s", btcdMsg(req, resp)) }),
        )
    }
    var conn = jsonrpc2.NewConn(ctx, stream, handler, opts...)
    return &btcdClient{conn: conn}, nil
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
    return c.conn.Close()
}

func (c *btcdClient) getBlockCount(ctx context.Context) (int64, error) {
    var count int64
    var err = c.conn.Call(ctx, "getblockcount", []interface{}{}, &count)
    return count, err
}

// estimateFee returns the fee, in BTC per kilobyte, needed for a transaction to
// confirm within numBlocks blocks. btcd's estimator errors ("not enough blocks
// have been observed") until the node has seen enough mempool activity.
func (c *btcdClient) estimateFee(ctx context.Context, numBlocks int) (float64, error) {
    var btcPerKB float64
    var err = c.conn.Call(ctx, "estimatefee", []interface{}{numBlocks}, &btcPerKB)
    return btcPerKB, err
}

type btcdVout struct {
    Value float64 `json:"value"`
}

type btcdTransaction struct {
    Txid          string     `json:"txid"`
    Hash          string     `json:"hash"`
    Confirmations uint64     `json:"confirmations"`
    BlockHash     string     `json:"blockhash"`
    Time          int64      `json:"time"`
    Vout          []btcdVout `json:"vout"`
}

func (c *btcdClient) getRawTransaction(ctx context.Context, txid string) (*btcdTransaction, error) {
    var tx btcdTransaction
    var err = c.conn.Call(ctx, "getrawtransaction", []interface{}{txid, 1}, &tx)
    if err != nil { return nil, err }
    return &tx, nil
}

func (c *btcdClient) getBlockHash(ctx context.Context, height int64) (string, error) {
    var hash string
    var err = c.conn.Call(ctx, "getblockhash", []interface{}{height}, &hash)
    return hash, err
}

type btcdBlockHeader struct {
    Hash          string  `json:"hash"`
    Confirmations int64   `json:"confirmations"`
    Height        int32   `json:"height"`
    Version       int32   `json:"version"`
    MerkleRoot    string  `json:"merkleroot"`
    Time          int64   `json:"time"`
    Nonce         uint64  `json:"nonce"`
    Bits          string  `json:"bits"`
    Difficulty    float64 `json:"difficulty"`
    PreviousHash  string  `json:"previousblockhash"`
    NextHash      string  `json:"nextblockhash"`
}

func (c *btcdClient) getBlockHeader(ctx context.Context, hash string) (*btcdBlockHeader, error) {
    var header btcdBlockHeader
    var err = c.conn.Call(ctx, "getblockheader", []interface{}{hash, true}, &header)
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
    Vin  []btcdVin  `json:"vin"`
    Vout []btcdVout `json:"vout"`
}

type btcdVerboseBlock struct {
    Hash string `json:"hash"`
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
    var err = c.conn.Call(ctx, "getblock", []interface{}{hash, 2}, &blk)
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
    var err = c.conn.Call(ctx, "validateaddress", []interface{}{address}, &info)
    if err != nil { return nil, err }
    return &info, nil
}

type btcdAddressTx struct {
    Txid string `json:"txid"`
    Time int64  `json:"time"`
}

func (c *btcdClient) searchRawTransactions(ctx context.Context, address string, count int) ([]btcdAddressTx, error) {
    var txs []btcdAddressTx
    var err = c.conn.Call(ctx, "searchrawtransactions", []interface{}{address, 1, 0, count, 0, true}, &txs)
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
    var err = c.conn.Call(ctx, "decoderawtransaction", []interface{}{txHex}, &tx)
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
    return c.conn.Call(ctx, "loadtxfilter", []interface{}{reload, addresses, outpoints}, nil)
}

func (c *btcdClient) notifyBlocks(ctx context.Context) error {
    return c.conn.Call(ctx, "notifyblocks", []interface{}{}, nil)
}

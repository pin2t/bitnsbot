package main

import "bytes"
import "context"
import "encoding/hex"
import "encoding/json"
import "fmt"
import "net/http"
import "os"
import "strings"

import "bitnsbot/logging"

// rpc is a minimal Bitcoin Core JSON-RPC client — only what resolving an
// address's history needs. The bot's own client (core.go) is package main's and
// cannot be imported here; this covers four methods where that one covers
// twenty, so it is a smaller thing rather than a copy.
type rpc struct {
    url    string
    client *http.Client
    auth   string
}

func newRPC(url, user, pass, cookieFile string) (*rpc, error) {
    if cookieFile != "" {
        var data, err = os.ReadFile(cookieFile)
        if err != nil { return nil, fmt.Errorf("read cookie: %w", err) }
        var parts = strings.SplitN(strings.TrimSpace(string(data)), ":", 2)
        if len(parts) != 2 { return nil, fmt.Errorf("%s is not a Core cookie file", cookieFile) }
        user, pass = parts[0], parts[1]
    }
    var c = &rpc{url: url, client: &http.Client{}}
    var req = &http.Request{Header: http.Header{}}
    req.SetBasicAuth(user, pass)
    c.auth = req.Header.Get("Authorization")
    return c, nil
}

// call speaks JSON-RPC 1.0 with positional params. Core reports method errors in
// the body with HTTP 500, so the status is not checked — the body is decoded
// either way.
func (c *rpc) call(ctx context.Context, method string, params []interface{}, result interface{}) error {
    if params == nil { params = []interface{}{} }
    var body, err = json.Marshal(map[string]interface{}{
        "jsonrpc": "1.0", "id": "addrindex", "method": method, "params": params,
    })
    if err != nil { return err }
    var req, reqErr = http.NewRequestWithContext(ctx, http.MethodPost, c.url, bytes.NewReader(body))
    if reqErr != nil { return reqErr }
    req.Header.Set("Content-Type", "application/json")
    req.Header.Set("Authorization", c.auth)
    var resp, doErr = c.client.Do(req)
    if doErr != nil { return doErr }
    defer resp.Body.Close()
    var decoded struct {
        Result json.RawMessage `json:"result"`
        Error  *struct {
            Message string `json:"message"`
        } `json:"error"`
    }
    if err := json.NewDecoder(resp.Body).Decode(&decoded); err != nil {
        return fmt.Errorf("%s: %w", method, err)
    }
    if decoded.Error != nil { return fmt.Errorf("%s: %s", method, decoded.Error.Message) }
    if result == nil { return nil }
    return json.Unmarshal(decoded.Result, result)
}

// scriptOf turns an address into the scriptPubKey the index is keyed by, which
// is why no address format is ever decoded here: the node does it.
func (c *rpc) scriptOf(ctx context.Context, addr string) (string, error) {
    var info struct {
        IsValid      bool   `json:"isvalid"`
        ScriptPubKey string `json:"scriptPubKey"`
    }
    if err := c.call(ctx, "validateaddress", []interface{}{addr}, &info); err != nil { return "", err }
    if !info.IsValid { return "", fmt.Errorf("%s is not a Bitcoin address", addr) }
    return info.ScriptPubKey, nil
}

// txidsAt returns one block's transaction ids in order, which is what a touch's
// TxIndex points into.
func (c *rpc) txidsAt(ctx context.Context, height uint32) ([]string, error) {
    var hash string
    if err := c.call(ctx, "getblockhash", []interface{}{height}, &hash); err != nil { return nil, err }
    var blk struct {
        Tx []string `json:"tx"`
    }
    if err := c.call(ctx, "getblock", []interface{}{hash, 1}, &blk); err != nil { return nil, err }
    return blk.Tx, nil
}

// tx is one transaction reduced to what an address listing needs: when it
// confirmed, and every input and output carrying an address.
type tx struct {
    Txid string `json:"txid"`
    Time int64  `json:"time"`
    Vin  []struct {
        PrevOut *struct {
            Value        float64 `json:"value"`
            ScriptPubKey struct {
                Address string `json:"address"`
            } `json:"scriptPubKey"`
        } `json:"prevout"`
    } `json:"vin"`
    Vout []struct {
        Value        float64 `json:"value"`
        ScriptPubKey struct {
            Address string `json:"address"`
        } `json:"scriptPubKey"`
    } `json:"vout"`
}

// transaction reads a confirmed transaction at verbosity 2, where Core supplies
// each input's prevout inline — so the spending side needs no extra calls.
func (c *rpc) transaction(ctx context.Context, txid string) (*tx, error) {
    var t tx
    if err := c.call(ctx, "getrawtransaction", []interface{}{txid, 2}, &t); err != nil { return nil, err }
    return &t, nil
}

// moved reports what one transaction did to this address: what it paid in, what
// it spent out, and the net of the two. Amounts are whole satoshi; Core reports
// BTC as a JSON number, and this is the one place it is converted.
func (t *tx) moved(addr string) (received, sent int64) {
    for _, v := range t.Vout {
        if v.ScriptPubKey.Address == addr { received += toSat(v.Value) }
    }
    for _, v := range t.Vin {
        if v.PrevOut != nil && v.PrevOut.ScriptPubKey.Address == addr { sent += toSat(v.PrevOut.Value) }
    }
    return received, sent
}

func toSat(btc float64) int64 {
    if btc < 0 { return -int64(-btc*1e8 + 0.5) }
    return int64(btc*1e8 + 0.5)
}

// addressOf turns a scriptPubKey back into the address it pays, which the node
// does — this tool decodes no address format of its own, the same rule the bot
// follows. A nonstandard script has no address, and comes back empty.
func (c *rpc) addressOf(ctx context.Context, script []byte) (string, error) {
    var out struct {
        Address string `json:"address"`
    }
    if err := c.call(ctx, "decodescript", []interface{}{hex.EncodeToString(script)}, &out); err != nil {
        return "", err
    }
    return out.Address, nil
}

// addressesOf resolves a batch of scriptPubKeys to the addresses they pay, in
// one JSON-RPC call. Core accepts a batch as an array of requests and answers
// with an array of results — matched back by id, since a batch reply is not
// promised in order. The returned slice is aligned to scripts, with "" where the
// script is nonstandard and has no address.
func (c *rpc) addressesOf(ctx context.Context, scripts [][]byte) ([]string, error) {
    var reqs = make([]map[string]interface{}, 0, len(scripts))
    for i, s := range scripts {
        reqs = append(reqs, map[string]interface{}{
            "jsonrpc": "1.0", "id": i, "method": "decodescript",
            "params": []interface{}{hex.EncodeToString(s)},
        })
    }
    var body, err = json.Marshal(reqs)
    if err != nil { return nil, err }
    var req, reqErr = http.NewRequestWithContext(ctx, http.MethodPost, c.url, bytes.NewReader(body))
    if reqErr != nil { return nil, reqErr }
    req.Header.Set("Content-Type", "application/json")
    req.Header.Set("Authorization", c.auth)
    var resp, doErr = c.client.Do(req)
    if doErr != nil { return nil, doErr }
    defer resp.Body.Close()
    var decoded []struct {
        ID     int `json:"id"`
        Result struct {
            Address string `json:"address"`
        } `json:"result"`
        Error *struct {
            Message string `json:"message"`
        } `json:"error"`
    }
    if err := json.NewDecoder(resp.Body).Decode(&decoded); err != nil {
        return nil, fmt.Errorf("decodescript batch: %w", err)
    }
    var out = make([]string, len(scripts))
    for _, d := range decoded {
        if d.ID < 0 || d.ID >= len(out) {
            return nil, fmt.Errorf("decodescript batch: reply id %d out of range", d.ID)
        }
        // one bad script must not lose the rest of the batch; it simply has no
        // address, which is the same answer a nonstandard script gives
        if d.Error != nil {
            logging.Warn("actbuild: decodescript: %s", d.Error.Message)
            continue
        }
        out[d.ID] = d.Result.Address
    }
    return out, nil
}

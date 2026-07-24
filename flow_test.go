package main

import "encoding/json"
import "fmt"
import "net/http"
import "net/http/httptest"
import "path/filepath"
import "strings"
import "testing"
import "time"

import "go.etcd.io/bbolt"

// infoBlockHash is a real mainnet block hash: 64 hex characters, exactly the
// same shape as a txid.
const infoBlockHash = "00000000000000000000524afad3a4cc1e4e190e1272de721de6cdb4e889f6aa"

func TestInfoFlow(t *testing.T) {
    var sent []string
    var lastMode string
    server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
        var body struct {
            Text      string `json:"text"`
            ParseMode string `json:"parse_mode"`
        }
        json.NewDecoder(r.Body).Decode(&body)
        sent = append(sent, body.Text)
        lastMode = body.ParseMode
        json.NewEncoder(w).Encode(map[string]any{"ok": true, "result": true})
    }))
    defer server.Close()
    bot := newBot("TESTTOKEN", server.URL)
    update(bot, Update{Message: &Message{Chat: Chat{ID: 1}, Text: "/info"}})
    if len(sent) != 1 || sent[0] != "Please send Bitcoin address or transaction or block number or block hash" {
        t.Fatalf("unexpected first reply: %#v", sent)
    }
    if !pendingInfoChats[1] {
        t.Fatalf("expected chat 1 to be pending")
    }
    core = nil
    update(bot, Update{Message: &Message{Chat: Chat{ID: 1}, Text: "hello world"}})
    if len(sent) != 2 || sent[1] != "Bitcoin node connection is not configured." {
        t.Fatalf("unexpected second reply: %#v", sent)
    }
    if pendingInfoChats[1] {
        t.Fatalf("expected chat 1 pending flag cleared")
    }
    update(bot, Update{Message: &Message{Chat: Chat{ID: 3}, Text: "/start"}})
    if len(sent) != 3 || !strings.Contains(sent[2], "<b>/info</b>") || !strings.Contains(sent[2], "<b>/watches</b>") {
        t.Fatalf("unexpected /start reply: %#v", sent)
    }
    update(bot, Update{Message: &Message{Chat: Chat{ID: 4}, Text: "just chatting, no pending info"}})
    if len(sent) != 3 {
        t.Fatalf("expected no reply for plain text without pending state, got: %#v", sent)
    }
    var recentTxid = "aaaa47a9143a23e80cc59e81588d21558b394005580b285961957cb3bed5b3e0"
    var recentTime = time.Now().Add(-48 * time.Hour).Unix()
    var btcdServer = newFakeCoreServer(t, func(method string, params []interface{}) (interface{}, error) {
        var p = params
        _ = p
        switch method {
        case "getblockhash":
            var height, _ = p[0].(float64)
            if height == 200 {
                return "00000000000000recentblockhash", nil
            }
            return "0000000000000000000blockhash", nil
        case "getblockheader":
            var hash, _ = p[0].(string)
            var blockTime int64 = 1700000000
            var height = 100
            var difficulty = 1.5
            switch hash {
            case "00000000000000recentblockhash":
                blockTime = recentTime
                height = 200
                difficulty = 1e9
            case "0000000000000000000blockhash", infoBlockHash:
            default:
                // a real node errors on a hash it doesn't have — which is exactly
                // what lets /info tell a 64-hex block hash from a 64-hex txid
                return nil, fmt.Errorf("Block not found")
            }
            return map[string]any{
                "hash":          hash,
                "confirmations": 10,
                "height":        height,
                "time":          blockTime,
                "difficulty":    difficulty,
            }, nil
        case "getblock":
            var reqHash, _ = p[0].(string)
            var height, blockTime, difficulty = 100, int64(1700000000), 1.5
            if reqHash == "00000000000000recentblockhash" {
                height, blockTime, difficulty = 200, recentTime, 1e9
            }
            return map[string]any{
                "hash": reqHash, "height": height, "time": blockTime, "size": 285, "difficulty": difficulty,
                "tx": []map[string]any{
                    {"txid": "coinbasetx", "size": 204, "vin": []map[string]any{{"coinbase": "03aabbcc"}}, "vout": []map[string]any{{"value": 50.0}}},
                },
            }, nil
        case "getrawtransaction":
            var reqTxid, _ = p[0].(string)
            if reqTxid == "prevtx" { // the input's prevout (value 1.5015 → fee 0.0015)
                return map[string]any{"vout": []map[string]any{{"value": 1.5015, "scriptPubKey": map[string]any{"address": "1A1zP1eP5QGefi2DMPTfTL5SLmv7DivfNa"}}}}, nil
            }
            var txTime int64 = 1700000000
            if reqTxid == recentTxid {
                txTime = recentTime
            }
            return map[string]any{
                "txid":          reqTxid,
                "confirmations": 6,
                "blockhash":     "0000000000000000000blockhash",
                "time":          txTime,
                "size":          225,
                "vin":           []map[string]any{{"txid": "prevtx", "vout": 0}},
                "vout":          []map[string]any{{"value": 1.5, "scriptPubKey": map[string]any{"address": "bc1qw508d6qejxtdg4y5r3zarvary0c5xw7kv8f3t4"}}},
            }, nil
        case "validateaddress":
            var addr, _ = p[0].(string)
            if addr == "invalidaddr" {
                return map[string]any{"isvalid": false}, nil
            }
            return map[string]any{"isvalid": true, "address": addr, "iswitness": addr == "addresswithhistory"}, nil
        case "searchrawtransactions":
            var addr, _ = p[0].(string)
            if addr == "addresswithhistory" {
                if skip, _ := p[2].(float64); skip > 0 {
                    return nil, fmt.Errorf("No information available about address")
                }
                return []map[string]any{
                    { // received 1.0 BTC on 2015-01-01
                        "txid": "aa", "time": 1420070400,
                        "vin":  []map[string]any{{"prevOut": map[string]any{"addresses": []string{"other"}, "value": 1.0001}}},
                        "vout": []map[string]any{{"value": 1.0, "scriptPubKey": map[string]any{"addresses": []string{"addresswithhistory"}}}},
                    },
                    { // spent 1.0 on 2016-01-01: 0.9 to dest + 0.0999 change back, fee 0.0001
                        "txid": "bb", "time": 1451606400,
                        "vin":  []map[string]any{{"prevOut": map[string]any{"addresses": []string{"addresswithhistory"}, "value": 1.0}}},
                        "vout": []map[string]any{
                            {"value": 0.9, "scriptPubKey": map[string]any{"addresses": []string{"dest"}}},
                            {"value": 0.0999, "scriptPubKey": map[string]any{"addresses": []string{"addresswithhistory"}}},
                        },
                    },
                }, nil
            }
            return nil, fmt.Errorf("Address index must be enabled (--addrindex)")
        }
        return nil, fmt.Errorf("unexpected method %s", method)
    })
    defer btcdServer.Close()
    core = newFakeCoreClient(t, btcdServer)
    defer func() { core = nil }()
    update(bot, Update{Message: &Message{Chat: Chat{ID: 5}, Text: "/info 100"}})
    if len(sent) != 4 {
        t.Fatalf("expected block reply, got %#v", sent)
    }
    for _, want := range []string{
        "Block #100", "Hash:          000000...ckhash", "Time:          14 november 2023 22:13",
        "Size:          285 bytes", "Transactions:  1", "Miner:         Unknown", "Difficulty:    1.5",
        "Fees:          none (coinbase only)", "minimum:       204 bytes", "Reward:        50 BTC",
        "Reward + fees: 50 BTC",
    } {
        if !strings.Contains(sent[3], want) {
            t.Fatalf("block reply missing %q: %q", want, sent[3])
        }
    }
    if lastMode != "HTML" {
        t.Fatalf("expected HTML parse mode, got: %q", lastMode)
    }
    var txid = "f21b47a9143a23e80cc59e81588d21558b394005580b285961957cb3bed5b3e0"
    update(bot, Update{Message: &Message{Chat: Chat{ID: 6}, Text: "/info " + txid}})
    if len(sent) != 5 {
        t.Fatalf("expected transaction reply, got %#v", sent)
    }
    for _, want := range []string{
        "Transaction <code>f21b47a9143a23e80cc59e81588d21558b394005580b285961957cb3bed5b3e0<code>",
        "confirmed (6 confirmations)", "Confirmed:", "14 november 2023 22:13",
        "Amount:", "1.5 BTC", "Fee:", "150 000 sats", "Size:", "225 bytes",
        "Inputs:", "1A1zP1...DivfNa", "Outputs:", "bc1qw5...v8f3t4",
    } {
        if !strings.Contains(sent[4], want) {
            t.Fatalf("transaction reply missing %q: %q", want, sent[4])
        }
    }
    // a block hash is the same 64-hex shape as a txid, and must produce exactly
    // the message its height produces — not "couldn't find transaction"
    update(bot, Update{Message: &Message{Chat: Chat{ID: 9}, Text: "/info " + infoBlockHash}})
    if len(sent) != 6 {
        t.Fatalf("expected a block reply for the block hash, got %#v", sent)
    }
    if sent[5] != sent[3] {
        t.Fatalf("/info <block hash> replied:\n%s\nbut /info <height> replied:\n%s", sent[5], sent[3])
    }
    update(bot, Update{Message: &Message{Chat: Chat{ID: 7}, Text: "/info 1A1zP1eP5QGefi2DMPTfTL5SLmv7DivfNa"}})
    if len(sent) != 7 || !strings.Contains(sent[6], "Address 1A1zP1...DivfNa") ||
        !strings.Contains(sent[6], "standard (P2PKH)") || !strings.Contains(sent[6], "Activity: unavailable") {
        t.Fatalf("unexpected address (index disabled) reply: %#v", sent)
    }
    update(bot, Update{Message: &Message{Chat: Chat{ID: 8}, Text: "/info addresswithhistory"}})
    if len(sent) != 8 {
        t.Fatalf("expected address reply, got %#v", sent)
    }
    // The history behind these stats now comes from the bot's own address index
    // rather than btcd's searchrawtransactions. With nothing indexed the reply
    // must say so plainly instead of reporting an empty history as fact —
    // addressStats itself is covered by TestAddressStats.
    for _, want := range []string{"segwit (bech32)", "Activity:", "still building"} {
        if !strings.Contains(sent[7], want) {
            t.Fatalf("address reply missing %q: %q", want, sent[7])
        }
    }
    update(bot, Update{Message: &Message{Chat: Chat{ID: 12}, Text: "/info invalidaddr"}})
    if len(sent) != 9 || !strings.Contains(sent[8], "doesn't look like a valid Bitcoin address") {
        t.Fatalf("unexpected invalid address reply: %#v", sent)
    }
    update(bot, Update{Message: &Message{Chat: Chat{ID: 10}, Text: "/info " + recentTxid}})
    if len(sent) != 10 || !strings.Contains(sent[9], "Confirmed:") || !strings.Contains(sent[9], "2 days ago") {
        t.Fatalf("expected relative confirmation time for recent transaction, got: %#v", sent)
    }
    update(bot, Update{Message: &Message{Chat: Chat{ID: 11}, Text: "/info 200"}})
    if len(sent) != 11 || !strings.Contains(sent[10], "Block #200") || !strings.Contains(sent[10], "Time:          2 days ago") {
        t.Fatalf("expected relative time format for recent block, got: %#v", sent)
    }
    if !strings.Contains(sent[10], "Difficulty:    1 G") {
        t.Fatalf("expected human readable difficulty, got: %#v", sent[10])
    }
}

func TestWatchFlow(t *testing.T) {
    var sent []string
    server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
        var body struct {
            Text string `json:"text"`
        }
        json.NewDecoder(r.Body).Decode(&body)
        sent = append(sent, body.Text)
        json.NewEncoder(w).Encode(map[string]any{"ok": true, "result": true})
    }))
    defer server.Close()
    bot := newBot("TESTTOKEN", server.URL)
    var err error
    err = openDB(filepath.Join(t.TempDir(), "watches.db"))
    if err != nil {
        t.Fatalf("openDB: %v", err)
    }
    defer closeDB()
    stopNotify()
    defer stopNotify()
    update(bot, Update{Message: &Message{Chat: Chat{ID: 1}, Text: "/watch"}})
    if len(sent) != 1 || !strings.Contains(sent[0], "alias") {
        t.Fatalf("unexpected first reply: %#v", sent)
    }
    if !pendingWatchChats[1] {
        t.Fatalf("expected chat 1 to be pending")
    }
    update(bot, Update{Message: &Message{Chat: Chat{ID: 1}, Text: "1A1zP1eP5QGefi2DMPTfTL5SLmv7DivfNa"}})
    if len(sent) != 2 || sent[1] != "Watching address: 1A1zP1eP5QGefi2DMPTfTL5SLmv7DivfNa" {
        t.Fatalf("unexpected second reply: %#v", sent)
    }
    if pendingWatchChats[1] {
        t.Fatalf("expected chat 1 pending flag cleared")
    }
    var txid = "f21b47a9143a23e80cc59e81588d21558b394005580b285961957cb3bed5b3e0"
    update(bot, Update{Message: &Message{Chat: Chat{ID: 2}, Text: "/watch " + txid}})
    if len(sent) != 3 || sent[2] != "Watching transaction: "+txid {
        t.Fatalf("unexpected third reply: %#v", sent)
    }
    // an alias as the second parameter (one message)
    var aliasAddr = "1BvBMSEYstWetqTFn5Au4m4GFg7xJaNVN2"
    update(bot, Update{Message: &Message{Chat: Chat{ID: 5}, Text: "/watch " + aliasAddr + " Savings"}})
    if sent[len(sent)-1] != "Watching address: "+aliasAddr+" (Savings)" {
        t.Fatalf("unexpected alias reply: %#v", sent)
    }
    // and through the bare-/watch follow-up — target and alias in the same message
    update(bot, Update{Message: &Message{Chat: Chat{ID: 6}, Text: "/watch"}})
    update(bot, Update{Message: &Message{Chat: Chat{ID: 6}, Text: aliasAddr + " Cold storage"}})
    if sent[len(sent)-1] != "Watching address: "+aliasAddr+" (Cold storage)" {
        t.Fatalf("unexpected pending-alias reply: %#v", sent)
    }
    // /watches lists the alias next to the id
    update(bot, Update{Message: &Message{Chat: Chat{ID: 5}, Text: "/watches"}})
    if !strings.Contains(sent[len(sent)-1], "<code>"+aliasAddr+"</code> (Savings)") {
        t.Fatalf("expected alias in /watches listing: %q", sent[len(sent)-1])
    }
}

func TestFeesFlow(t *testing.T) {
    var sent []string
    var lastMode string
    var server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
        var body struct {
            Text      string `json:"text"`
            ParseMode string `json:"parse_mode"`
        }
        json.NewDecoder(r.Body).Decode(&body)
        sent = append(sent, body.Text)
        lastMode = body.ParseMode
        json.NewEncoder(w).Encode(map[string]any{"ok": true, "result": true})
    }))
    defer server.Close()
    var bot = newBot("TESTTOKEN", server.URL)
    // not configured → fixed message
    core = nil
    update(bot, Update{Message: &Message{Chat: Chat{ID: 1}, Text: "/fees"}})
    if len(sent) != 1 || sent[0] != "Bitcoin node connection is not configured." {
        t.Fatalf("unexpected not-configured reply: %#v", sent)
    }
    // configured: BTC/kvB values convert to sat/vB by ×1e5 (0.0001 → 10 sat/vB)
    var rates = map[float64]float64{2: 0.0001, 6: 0.00007, 12: 0.00003}
    var btcdServer = newFakeCoreServer(t, func(method string, params []interface{}) (interface{}, error) {
        if method != "estimatesmartfee" {
            return nil, fmt.Errorf("unexpected method %s", method)
        }
        var blocks, _ = params[0].(float64)
        return map[string]any{"feerate": rates[blocks], "blocks": blocks}, nil
    })
    defer btcdServer.Close()
    core = newFakeCoreClient(t, btcdServer)
    defer func() { core = nil }()
    update(bot, Update{Message: &Message{Chat: Chat{ID: 1}, Text: "/fees"}})
    if len(sent) != 2 {
        t.Fatalf("expected a fees reply, got %#v", sent)
    }
    for _, want := range []string{"Estimated network fees", "<pre>", "Fast (10-20 min):", "10 sat/vB", "Medium (~1h):", "7 sat/vB", "Slow (2h+):", "3 sat/vB"} {
        if !strings.Contains(sent[1], want) {
            t.Fatalf("fees reply missing %q: %q", want, sent[1])
        }
    }
    if lastMode != "HTML" {
        t.Fatalf("expected HTML parse mode, got %q", lastMode)
    }
}

func TestFeesUnavailable(t *testing.T) {
    var sent []string
    var server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
        var body struct {
            Text string `json:"text"`
        }
        json.NewDecoder(r.Body).Decode(&body)
        sent = append(sent, body.Text)
        json.NewEncoder(w).Encode(map[string]any{"ok": true, "result": true})
    }))
    defer server.Close()
    var bot = newBot("TESTTOKEN", server.URL)
    var btcdServer = newFakeCoreServer(t, func(method string, params []interface{}) (interface{}, error) {
        return nil, fmt.Errorf("not enough blocks have been observed")
    })
    defer btcdServer.Close()
    core = newFakeCoreClient(t, btcdServer)
    defer func() { core = nil }()
    update(bot, Update{Message: &Message{Chat: Chat{ID: 1}, Text: "/fees"}})
    if len(sent) != 1 || !strings.Contains(sent[0], "aren't available") {
        t.Fatalf("unexpected unavailable reply: %#v", sent)
    }
}

func TestCompactAddrs(t *testing.T) {
    if got := compactAddrs(nil); got != "none" {
        t.Fatalf("empty = %q", got)
    }
    if got := compactAddrs([]string{"a", "b", "c"}); got != "a, b, c" { // exactly 3 shown in full
        t.Fatalf("three = %q", got)
    }
    if got := compactAddrs([]string{"a", "b", "c", "d", "e"}); got != "a, b, c, ..." { // >3 → first 3 + ...
        t.Fatalf("more = %q", got)
    }
    if got := compactAddrs([]string{"1A1zP1eP5QGefi2DMPTfTL5SLmv7DivfNa"}); got != "1A1zP1...DivfNa" { // shortened
        t.Fatalf("short = %q", got)
    }
}

func TestMempoolFlow(t *testing.T) {
    var sent []string
    var server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
        var body struct {
            Text string `json:"text"`
        }
        json.NewDecoder(r.Body).Decode(&body)
        sent = append(sent, body.Text)
        json.NewEncoder(w).Encode(map[string]any{"ok": true, "result": true})
    }))
    defer server.Close()
    var bot = newBot("TESTTOKEN", server.URL)
    core = nil
    update(bot, Update{Message: &Message{Chat: Chat{ID: 1}, Text: "/mempool"}})
    if len(sent) != 1 || sent[0] != "Bitcoin node connection is not configured." {
        t.Fatalf("unexpected not-configured reply: %#v", sent)
    }
    var btcdServer = newFakeCoreServer(t, func(method string, params []interface{}) (interface{}, error) {
        var p = params
        _ = p
        switch method {
        case "getmempoolinfo":
            return map[string]any{"size": 6700, "bytes": 3500000}, nil // → "6.7 k" txs, "3.5 M"
        case "getrawmempool":
            // Core nests the fee under fees.base; btcd used a flat "fee"
            return map[string]any{
                "tx1": map[string]any{"fees": map[string]any{"base": 0.0001}},
                "tx2": map[string]any{"fees": map[string]any{"base": 0.0002}},
            }, nil // fees sum 0.0003 = 30000 sats
        case "getrawtransaction":
            switch id, _ := p[0].(string); id {
            case "tx1":
                return map[string]any{"vout": []map[string]any{{"value": 1.0}}}, nil
            case "tx2":
                return map[string]any{"vout": []map[string]any{{"value": 2.0}, {"value": 0.5}}}, nil // amounts sum 3.5 = 350000000 sats
            }
            return nil, fmt.Errorf("no such tx")
        }
        return nil, fmt.Errorf("unexpected method %s", method)
    })
    defer btcdServer.Close()
    core = newFakeCoreClient(t, btcdServer)
    defer func() { core = nil }()
    update(bot, Update{Message: &Message{Chat: Chat{ID: 1}, Text: "/mempool"}})
    if len(sent) != 2 {
        t.Fatalf("expected a mempool reply, got %#v", sent)
    }
    for _, want := range []string{
        "Mempool", "Size:", "3.5 M", "Transactions: 6.7 k",
        "Total amount:", "3.50 BTC", "Total fees:", "0.0003 BTC", // amounts 3.5 BTC, fees 0.0003 BTC
    } {
        if !strings.Contains(sent[1], want) {
            t.Fatalf("mempool reply missing %q: %q", want, sent[1])
        }
    }
    // a mempool over the limit degrades to size + count only
    var saved = mempoolSummaryLimit
    mempoolSummaryLimit = 1
    defer func() { mempoolSummaryLimit = saved }()
    update(bot, Update{Message: &Message{Chat: Chat{ID: 1}, Text: "/mempool"}})
    if !strings.Contains(sent[2], "Totals:") || strings.Contains(sent[2], "Total amount") {
        t.Fatalf("expected totals skipped for oversized mempool: %q", sent[2])
    }
}

func TestMempoolFlowRate(t *testing.T) {
    var savedInterval, savedLimit = flowInterval, mempoolSummaryLimit
    flowInterval = 10 * time.Second // known divisor
    mempoolSummaryLimit = 1         // skip totals — this test is about the flow line
    flowMu.Lock()
    flowHaveCount, flowRateOK, flowChangeOK = false, false, false
    flowMu.Unlock()
    defer func() {
        flowInterval, mempoolSummaryLimit = savedInterval, savedLimit
        flowMu.Lock()
        flowHaveCount, flowRateOK, flowChangeOK = false, false, false
        flowMu.Unlock()
    }()
    updateFlow(1000) // baseline — no rate yet
    flowMu.Lock()
    var ok1 = flowRateOK
    flowMu.Unlock()
    if ok1 {
        t.Fatal("expected no rate after one sample")
    }
    updateFlow(1025) // rate (1025-1000)/10 = 2.5, no change yet
    flowMu.Lock()
    var r2, cok2 = flowRate, flowChangeOK
    flowMu.Unlock()
    if r2 != 2.5 || cok2 {
        t.Fatalf("after two samples: rate=%v changeOK=%v (want 2.5, false)", r2, cok2)
    }
    updateFlow(1055) // rate (1055-1025)/10 = 3.0, change 3.0-2.5 = 0.5
    flowMu.Lock()
    var r3, ch3, cok3 = flowRate, flowChange, flowChangeOK
    flowMu.Unlock()
    if r3 != 3.0 || !cok3 || ch3 != 0.5 {
        t.Fatalf("after three samples: rate=%v change=%v (want 3.0, 0.5)", r3, ch3)
    }
    updateFlow(900) // count DROPPED (likely a mined block) → rate/change unchanged, baseline re-set
    flowMu.Lock()
    var rDrop, chDrop = flowRate, flowChange
    flowMu.Unlock()
    if rDrop != 3.0 || chDrop != 0.5 {
        t.Fatalf("after a decrease: rate=%v change=%v (want unchanged 3.0, 0.5)", rDrop, chDrop)
    }
    updateFlow(950) // increase from the re-set baseline: (950-900)/10 = 5.0, change 5.0-3.0 = 2.0
    flowMu.Lock()
    var rUp, chUp = flowRate, flowChange
    flowMu.Unlock()
    if rUp != 5.0 || chUp != 2.0 {
        t.Fatalf("after re-increase: rate=%v change=%v (want 5.0, 2.0)", rUp, chUp)
    }
    // and it renders in the /mempool reply
    var sent []string
    var tg = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
        var body struct {
            Text string `json:"text"`
        }
        json.NewDecoder(r.Body).Decode(&body)
        sent = append(sent, body.Text)
        json.NewEncoder(w).Encode(map[string]any{"ok": true, "result": true})
    }))
    defer tg.Close()
    var b = newBot("TESTTOKEN", tg.URL)
    var srv = newFakeCoreServer(t, func(method string, params []interface{}) (interface{}, error) {
        if method == "getmempoolinfo" {
            return map[string]any{"size": 5000, "bytes": 2000000}, nil
        }
        return nil, fmt.Errorf("unexpected method %s", method)
    })
    defer srv.Close()
    core = newFakeCoreClient(t, srv)
    defer func() { core = nil }()
    update(b, Update{Message: &Message{Chat: Chat{ID: 1}, Text: "/mempool"}})
    if len(sent) != 1 || !strings.Contains(sent[0], "Flow rate:") || !strings.Contains(sent[0], "5.0 tx/sec (+2.0)") {
        t.Fatalf("expected flow rate line in reply: %#v", sent)
    }
}

func TestPeriodText(t *testing.T) {
    var cases = []struct {
        d    time.Duration
        want string
    }{
        {365 * 24 * time.Hour, "1 y"},
        {(3*365 + 2) * 24 * time.Hour, "3 y 2 d"},
        {(2*30 + 1) * 24 * time.Hour, "2 m 1 d"},
        {5*time.Hour + 10*time.Minute, "5 h 10 min"},
        {45 * time.Minute, "45 min"},
        {30 * time.Second, "0 min"},
    }
    for _, c := range cases {
        if got := periodText(c.d); got != c.want {
            t.Errorf("periodText(%v) = %q, want %q", c.d, got, c.want)
        }
    }
}

func TestTimeCompact(t *testing.T) {
    var now = time.Now()
    var cases = []struct {
        t    time.Time
        want string
    }{
        {now.Add(-30 * time.Second), "just now"},
        {now.Add(-5 * time.Minute), "5 min ago"},
        {now.Add(-3 * time.Hour), "3 h ago"},
        {now.Add(-5 * 24 * time.Hour), "5 d ago"},
        {now.Add(-70 * 24 * time.Hour), "2 m ago"},
    }
    for _, c := range cases {
        if got := timeCompact(c.t.Unix()); got != c.want {
            t.Errorf("timeCompact(%v) = %q, want %q", c.t, got, c.want)
        }
    }
    if got := timeCompact(time.Date(2015, 1, 1, 0, 0, 0, 0, time.UTC).Unix()); got != "1 january 2015" {
        t.Errorf("timeCompact(2015-01-01) = %q, want %q", got, "1 january 2015")
    }
}

func TestMinersFlow(t *testing.T) {
    var sent []string
    var server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
        var body struct {
            Text string `json:"text"`
        }
        json.NewDecoder(r.Body).Decode(&body)
        sent = append(sent, body.Text)
        json.NewEncoder(w).Encode(map[string]any{"ok": true, "result": true})
    }))
    defer server.Close()
    var bot = newBot("TESTTOKEN", server.URL)
    if err := openDB(filepath.Join(t.TempDir(), "miners.db")); err != nil {
        t.Fatalf("openDB: %v", err)
    }
    defer closeDB()
    update(bot, Update{Message: &Message{Chat: Chat{ID: 1}, Text: "/miners"}})
    if len(sent) != 1 || !strings.Contains(sent[0], "still collecting") {
        t.Fatalf("unexpected empty-stats reply: %#v", sent)
    }
    // a processed window of blocks 0..9 with three attributed pools; LastWork
    // 6e23 is a ~10 GW network, so a 60% share draws ~6 GW
    var pools = []struct {
        name     string
        blocks   int
        reward   float64
        fees     float64
        lastWork float64
    }{
        {"ViaBTC", 3, 9.75, 0.1, 6.0e23},
        {"AntPool", 6, 19.5, 0.4, 6.0e23},
        {"F2Pool", 1, 3.25, 0.02, 5.6e23},
    }
    if err := db.Update(func(tx *bbolt.Tx) error {
        var b = tx.Bucket([]byte("miners-stat"))
        for _, p := range pools {
            var rec = fmt.Sprintf(`{"Blocks":%d,"Reward":%g,"Fees":%g,"Work":%g,"LastWork":%g}`,
                p.blocks, p.reward, p.fees, p.lastWork*float64(p.blocks), p.lastWork)
            if err := b.Put([]byte(p.name), []byte(rec)); err != nil { return err }
        }
        return tx.Bucket([]byte("miners-block")).Put([]byte("cursor"), []byte(`{"Start":0,"Last":9}`))
    }); err != nil {
        t.Fatalf("seed stats: %v", err)
    }
    update(bot, Update{Message: &Message{Chat: Chat{ID: 1}, Text: "/miners"}})
    if len(sent) != 2 {
        t.Fatalf("expected a miners reply, got %#v", sent)
    }
    var want = "Top miners by blocks mined:\n\n" +
        "1. AntPool. 6 blocks mined, reward 19.5 BTC, fees 0.4 BTC, consumption 6 GW\n" +
        "2. ViaBTC. 3 blocks mined, reward 9.75 BTC, fees 0.1 BTC, consumption 3 GW\n" +
        "3. F2Pool. 1 block mined, reward 3.25 BTC, fees 0.02 BTC, consumption 0.9 GW"
    if sent[1] != want {
        t.Fatalf("miners reply:\n%s\nwant:\n%s", sent[1], want)
    }
}

package main

import "encoding/json"
import "fmt"
import "net/http"
import "net/http/httptest"
import "path/filepath"
import "strings"
import "testing"
import "time"

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
    if len(sent) != 1 || sent[0] != "Please send Bitcoin address or transaction or block number" {
        t.Fatalf("unexpected first reply: %#v", sent)
    }
    if !pendingInfoChats[1] {
        t.Fatalf("expected chat 1 to be pending")
    }
    btcd = nil
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
    var btcdServer = newFakeBtcdServer(t, func(method string, params json.RawMessage) (interface{}, error) {
        var p []interface{}
        json.Unmarshal(params, &p)
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
            if hash == "00000000000000recentblockhash" {
                blockTime = recentTime
                height = 200
                difficulty = 1e9
            }
            return map[string]any{
                "hash":          hash,
                "confirmations": 10,
                "height":        height,
                "time":          blockTime,
                "difficulty":    difficulty,
            }, nil
        case "getrawtransaction":
            var reqTxid, _ = p[0].(string)
            var txTime int64 = 1700000000
            if reqTxid == recentTxid {
                txTime = recentTime
            }
            return map[string]any{
                "txid":          reqTxid,
                "confirmations": 6,
                "blockhash":     "0000000000000000000blockhash",
                "time":          txTime,
                "vout":          []map[string]any{{"value": 1.5}},
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
                return []map[string]any{{"txid": "abc123", "time": 1700000000}}, nil
            }
            return nil, fmt.Errorf("Address index disabled")
        }
        return nil, fmt.Errorf("unexpected method %s", method)
    })
    defer btcdServer.Close()
    btcd = dialFakeBtcd(t, btcdServer, &recordingHandler{})
    defer btcd.close()
    update(bot, Update{Message: &Message{Chat: Chat{ID: 5}, Text: "/info 100"}})
    var wantBlock = "Block #100\n\n<pre>Hash:          000000...ckhash\nTime:          14 november 2023 22:13\nConfirmations: 10\nDifficulty:    1.5</pre>"
    if len(sent) != 4 || sent[3] != wantBlock {
        t.Fatalf("unexpected block reply: %#v", sent)
    }
    if lastMode != "HTML" {
        t.Fatalf("expected HTML parse mode, got: %q", lastMode)
    }
    var txid = "f21b47a9143a23e80cc59e81588d21558b394005580b285961957cb3bed5b3e0"
    update(bot, Update{Message: &Message{Chat: Chat{ID: 6}, Text: "/info " + txid}})
    var wantTx = "Transaction f21b47...d5b3e0\n\n<pre>Status: confirmed (6 confirmations)\nBlock:  000000...ckhash\nTime:   14 november 2023 22:13\nAmount: 150 000 000 satoshi</pre>"
    if len(sent) != 5 || sent[4] != wantTx {
        t.Fatalf("unexpected transaction reply: %#v", sent)
    }
    update(bot, Update{Message: &Message{Chat: Chat{ID: 7}, Text: "/info 1A1zP1eP5QGefi2DMPTfTL5SLmv7DivfNa"}})
    var wantAddr = "Address 1A1zP1...DivfNa\n\n<pre>Type:            standard (P2PKH)\nRecent activity: unavailable (address index not enabled)</pre>"
    if len(sent) != 6 || sent[5] != wantAddr {
        t.Fatalf("unexpected address (no history) reply: %#v", sent)
    }
    update(bot, Update{Message: &Message{Chat: Chat{ID: 8}, Text: "/info addresswithhistory"}})
    if len(sent) != 7 || !strings.Contains(sent[6], "segwit") || !strings.Contains(sent[6], "1 transaction(s) found") {
        t.Fatalf("unexpected address (with history) reply: %#v", sent)
    }
    update(bot, Update{Message: &Message{Chat: Chat{ID: 9}, Text: "/info invalidaddr"}})
    if len(sent) != 8 || !strings.Contains(sent[7], "doesn't look like a valid Bitcoin address") {
        t.Fatalf("unexpected invalid address reply: %#v", sent)
    }
    update(bot, Update{Message: &Message{Chat: Chat{ID: 10}, Text: "/info " + recentTxid}})
    if len(sent) != 9 || !strings.Contains(sent[8], "Time:   2 days ago") {
        t.Fatalf("expected relative time format for recent transaction, got: %#v", sent)
    }
    update(bot, Update{Message: &Message{Chat: Chat{ID: 11}, Text: "/info 200"}})
    if len(sent) != 10 || !strings.Contains(sent[9], "Block #200") || !strings.Contains(sent[9], "Time:          2 days ago") {
        t.Fatalf("expected relative time format for recent block, got: %#v", sent)
    }
    if !strings.Contains(sent[9], "Difficulty:    1 G") {
        t.Fatalf("expected human readable difficulty, got: %#v", sent[9])
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
    store, err = openWatchStore(filepath.Join(t.TempDir(), "watches.db"))
    if err != nil {
        t.Fatalf("openWatchStore: %v", err)
    }
    defer store.close()
    stopNotify()
    defer stopNotify()
    update(bot, Update{Message: &Message{Chat: Chat{ID: 1}, Text: "/watch"}})
    if len(sent) != 1 || sent[0] != "Please send what you'd like to watch in a separate message." {
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
}

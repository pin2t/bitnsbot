package main

import "encoding/json"
import "fmt"
import "net/http"
import "net/http/httptest"
import "path/filepath"
import "strings"
import "testing"

func TestInfoFlow(t *testing.T) {
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
    update(bot, Update{Message: &Message{Chat: Chat{ID: 1}, Text: "/info"}})
    if len(sent) != 1 || sent[0] != "Please send the info text in a separate message." {
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
    if len(sent) != 3 || sent[2] != "Hello! I'm bitnsbot. I'll notify you about Bitcoin network events." {
        t.Fatalf("unexpected third reply: %#v", sent)
    }
    update(bot, Update{Message: &Message{Chat: Chat{ID: 4}, Text: "just chatting, no pending info"}})
    if len(sent) != 3 {
        t.Fatalf("expected no reply for plain text without pending state, got: %#v", sent)
    }
    var btcdServer = newFakeBtcdServer(t, func(method string, params json.RawMessage) (interface{}, error) {
        var p []interface{}
        json.Unmarshal(params, &p)
        switch method {
        case "getblockhash":
            return "0000000000000000000blockhash", nil
        case "getblockheader":
            return map[string]any{
                "hash":          "0000000000000000000blockhash",
                "confirmations": 10,
                "height":        100,
                "time":          1700000000,
                "difficulty":    1.5,
            }, nil
        case "getrawtransaction":
            return map[string]any{
                "txid":          "f21b47a9143a23e80cc59e81588d21558b394005580b285961957cb3bed5b3e0",
                "confirmations": 6,
                "blockhash":     "0000000000000000000blockhash",
                "time":          1700000000,
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
    if len(sent) != 4 || !strings.Contains(sent[3], "Block #100") {
        t.Fatalf("unexpected block reply: %#v", sent)
    }
    var txid = "f21b47a9143a23e80cc59e81588d21558b394005580b285961957cb3bed5b3e0"
    update(bot, Update{Message: &Message{Chat: Chat{ID: 6}, Text: "/info " + txid}})
    if len(sent) != 5 || !strings.Contains(sent[4], "Transaction "+txid) || !strings.Contains(sent[4], "1.50000000 BTC") {
        t.Fatalf("unexpected transaction reply: %#v", sent)
    }
    update(bot, Update{Message: &Message{Chat: Chat{ID: 7}, Text: "/info 1A1zP1eP5QGefi2DMPTfTL5SLmv7DivfNa"}})
    if len(sent) != 6 || !strings.Contains(sent[5], "unavailable") {
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

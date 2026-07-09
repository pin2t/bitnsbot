package main

import "context"
import "encoding/json"
import "net/http"
import "net/http/httptest"
import "path/filepath"
import "strings"
import "sync"
import "testing"
import "time"

import "github.com/gorilla/websocket"
import "github.com/sourcegraph/jsonrpc2"

// TestWatchNotification drives the full address-watch notification path: a
// /watch on an address loads it into btcd's filter, btcd pushes a
// relevanttxaccepted notification, and the bot decodes the tx and messages the
// watching chat. The fake btcd server pushes the notification right after the
// loadtxfilter it receives.
func TestWatchNotification(t *testing.T) {
    var sentMu sync.Mutex
    var sent []string
    var tg = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
        var body struct {
            Text string `json:"text"`
        }
        json.NewDecoder(r.Body).Decode(&body)
        sentMu.Lock()
        sent = append(sent, body.Text)
        sentMu.Unlock()
        json.NewEncoder(w).Encode(map[string]any{"ok": true, "result": true})
    }))
    defer tg.Close()
    var b = newBot("TESTTOKEN", tg.URL)
    var watchedAddr = "1A1zP1eP5QGefi2DMPTfTL5SLmv7DivfNa"
    var txid = "f21b47a9143a23e80cc59e81588d21558b394005580b285961957cb3bed5b3e0"
    var upgrader websocket.Upgrader
    var btcdSrv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
        var conn, err = upgrader.Upgrade(w, r, nil)
        if err != nil { return }
        defer conn.Close()
        for {
            var req struct {
                ID     json.RawMessage `json:"id"`
                Method string          `json:"method"`
                Params json.RawMessage `json:"params"`
            }
            if err := conn.ReadJSON(&req); err != nil { return }
            switch req.Method {
            case "loadtxfilter":
                conn.WriteJSON(map[string]any{"jsonrpc": "2.0", "id": req.ID, "result": nil})
                conn.WriteJSON(map[string]any{
                    "jsonrpc": "2.0",
                    "method":  "relevanttxaccepted",
                    "params":  []string{"deadbeefrawtxhex"},
                })
            case "decoderawtransaction":
                conn.WriteJSON(map[string]any{"jsonrpc": "2.0", "id": req.ID, "result": map[string]any{
                    "txid": txid,
                    "vout": []map[string]any{
                        {"value": 2.5, "scriptPubKey": map[string]any{"addresses": []string{watchedAddr}}},
                    },
                }})
            default:
                conn.WriteJSON(map[string]any{"jsonrpc": "2.0", "id": req.ID, "result": nil})
            }
        }
    }))
    defer btcdSrv.Close()
    var url = "ws://" + strings.TrimPrefix(btcdSrv.URL, "http://") + "/ws"
    var dialErr error
    btcd, dialErr = dialBtcd(context.Background(), btcdConfig{url: url, user: "u", pass: "p"}, jsonrpc2.AsyncHandler(btcdNotifier{bot: b}))
    if dialErr != nil {
        t.Fatalf("dialBtcd: %v", dialErr)
    }
    defer func() { btcd.close(); btcd = nil }()
    store, _ = openWatchStore(filepath.Join(t.TempDir(), "watches.db"))
    defer store.close()
    watchersByAddr = make(map[string][]int64)
    watch(b, 42, watchedAddr)
    var deadline = time.Now().Add(3 * time.Second)
    for time.Now().Before(deadline) {
        sentMu.Lock()
        var n = len(sent)
        sentMu.Unlock()
        if n >= 2 { break }
        time.Sleep(10 * time.Millisecond)
    }
    sentMu.Lock()
    defer sentMu.Unlock()
    var found string
    for _, m := range sent {
        if strings.Contains(m, "New transaction on watched address") {
            found = m
        }
    }
    if found == "" {
        t.Fatalf("expected a watch notification, got: %#v", sent)
    }
    if !strings.Contains(found, short(watchedAddr)) || !strings.Contains(found, short(txid)) {
        t.Fatalf("notification missing address/txid: %q", found)
    }
    if !strings.Contains(found, "250 000 000 satoshi") {
        t.Fatalf("expected 250 000 000 satoshi in notification, got: %q", found)
    }
}

func TestUnwatchFlow(t *testing.T) {
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
    var b = newBot("TESTTOKEN", server.URL)
    btcd = nil
    watchersByAddr = make(map[string][]int64)
    store, _ = openWatchStore(filepath.Join(t.TempDir(), "watches.db"))
    defer store.close()
    var addr = "1A1zP1eP5QGefi2DMPTfTL5SLmv7DivfNa"
    update(b, Update{Message: &Message{Chat: Chat{ID: 1}, Text: "/watch " + addr}})
    update(b, Update{Message: &Message{Chat: Chat{ID: 2}, Text: "/watch " + addr}})
    if len(watchersByAddr[addr]) != 2 {
        t.Fatalf("expected 2 watchers, got %#v", watchersByAddr[addr])
    }
    // chat 2 unwatches; only chat 1 must remain, both in the routing map and the store
    update(b, Update{Message: &Message{Chat: Chat{ID: 2}, Text: "/unwatch " + addr}})
    if sent[len(sent)-1] != "Stopped watching "+addr+"." {
        t.Fatalf("unexpected unwatch reply: %#v", sent)
    }
    if got := watchersByAddr[addr]; len(got) != 1 || got[0] != 1 {
        t.Fatalf("expected only chat 1 to remain, got %#v", got)
    }
    var records, _ = store.list()
    if len(records) != 1 || records[0].ChatID != 1 {
        t.Fatalf("expected only chat 1's record in store, got %#v", records)
    }
    // unwatching something not watched removes nothing
    update(b, Update{Message: &Message{Chat: Chat{ID: 1}, Text: "/unwatch bogusaddr"}})
    if sent[len(sent)-1] != "You're not watching bogusaddr." {
        t.Fatalf("unexpected not-watching reply: %q", sent[len(sent)-1])
    }
    // bare /unwatch marks the chat pending and consumes the next plain message
    update(b, Update{Message: &Message{Chat: Chat{ID: 1}, Text: "/unwatch"}})
    if !pendingUnwatchChats[1] {
        t.Fatalf("expected chat 1 pending for unwatch")
    }
    update(b, Update{Message: &Message{Chat: Chat{ID: 1}, Text: addr}})
    if sent[len(sent)-1] != "Stopped watching "+addr+"." {
        t.Fatalf("expected follow-up to remove chat 1's watch, got: %q", sent[len(sent)-1])
    }
    if pendingUnwatchChats[1] {
        t.Fatalf("expected pending cleared")
    }
    if len(watchersByAddr[addr]) != 0 {
        t.Fatalf("expected no watchers left, got %#v", watchersByAddr[addr])
    }
}

func TestWatchesFlow(t *testing.T) {
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
    var b = newBot("TESTTOKEN", server.URL)
    store, _ = openWatchStore(filepath.Join(t.TempDir(), "watches.db"))
    defer store.close()
    // nothing watched yet
    update(b, Update{Message: &Message{Chat: Chat{ID: 1}, Text: "/watches"}})
    if sent[len(sent)-1] != "You're not watching anything yet." {
        t.Fatalf("unexpected empty reply: %q", sent[len(sent)-1])
    }
    var addr = "1A1zP1eP5QGefi2DMPTfTL5SLmv7DivfNa"
    var txid = "f21b47a9143a23e80cc59e81588d21558b394005580b285961957cb3bed5b3e0"
    store.add(1, watchTypeAddress, addr)
    store.add(1, watchTypeTransaction, txid)
    store.add(2, watchTypeAddress, "someoneElsesAddress")
    update(b, Update{Message: &Message{Chat: Chat{ID: 1}, Text: "/watches"}})
    var msg = sent[len(sent)-1]
    // full ids present (not shortened) and tap-to-copy wrapped
    if !strings.Contains(msg, "<code>"+addr+"</code>") {
        t.Fatalf("expected full address in <code>, got: %q", msg)
    }
    if !strings.Contains(msg, "<code>"+txid+"</code>") {
        t.Fatalf("expected full txid in <code>, got: %q", msg)
    }
    if !strings.Contains(msg, "Addresses:") || !strings.Contains(msg, "Transactions:") {
        t.Fatalf("expected grouped sections, got: %q", msg)
    }
    // scoping: another chat's watch must not appear
    if strings.Contains(msg, "someoneElsesAddress") {
        t.Fatalf("must not list another chat's watch: %q", msg)
    }
    // a watch id containing HTML metacharacters must be escaped
    store.add(3, watchTypeAddress, "a<b>c")
    update(b, Update{Message: &Message{Chat: Chat{ID: 3}, Text: "/watches"}})
    if last := sent[len(sent)-1]; !strings.Contains(last, "a&lt;b&gt;c") {
        t.Fatalf("expected HTML-escaped watch id, got: %q", last)
    }
}

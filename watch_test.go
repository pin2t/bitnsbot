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

import "bitnsbot/txwatches"
import "bitnsbot/watches"

func countWatchers(watchID string) int {
    notifyMu.Lock()
    defer notifyMu.Unlock()
    var n int
    for k, _ := range notifies {
        if k.id == watchID { n++ }
    }
    return n
}

func watcherChats(watchID string) []int64 {
    notifyMu.Lock()
    defer notifyMu.Unlock()
    var ids []int64
    for k, _ := range notifies {
        if k.id == watchID { ids = append(ids, k.chat) }
    }
    return ids
}

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
            case "getrawtransaction":
                var p []interface{}
                json.Unmarshal(req.Params, &p)
                if reqTxid, _ := p[0].(string); reqTxid == "prevtx" { // prevout: value 2.5001 → fee 0.0001 BTC = 10000 sat
                    conn.WriteJSON(map[string]any{"jsonrpc": "2.0", "id": req.ID, "result": map[string]any{
                        "vout": []map[string]any{{"value": 2.5001, "scriptPubKey": map[string]any{"address": "1InputAddr"}}},
                    }})
                } else {
                    conn.WriteJSON(map[string]any{"jsonrpc": "2.0", "id": req.ID, "result": map[string]any{
                        "txid": txid, "confirmations": 0, "size": 110, "vsize": 100,
                        "vin":  []map[string]any{{"txid": "prevtx", "vout": 0}},
                        "vout": []map[string]any{{"value": 2.5, "scriptPubKey": map[string]any{"addresses": []string{watchedAddr}}}},
                    }})
                }
            case "estimatefee":
                var p []interface{}
                json.Unmarshal(req.Params, &p)
                var rate = 0.0002 // 6-block target → 20 sat/vB
                if blocks, _ := p[0].(float64); blocks == 2 {
                    rate = 0.0005 // 2-block target → 50 sat/vB
                }
                conn.WriteJSON(map[string]any{"jsonrpc": "2.0", "id": req.ID, "result": rate})
            default:
                conn.WriteJSON(map[string]any{"jsonrpc": "2.0", "id": req.ID, "result": nil})
            }
        }
    }))
    defer btcdSrv.Close()
    var url = "ws://" + strings.TrimPrefix(btcdSrv.URL, "http://") + "/ws"
    var dialErr error
    btcd, dialErr = dialBtcd(context.Background(), btcdConfig{url: url, user: "u", pass: "p"}, notifier{})
    if dialErr != nil {
        t.Fatalf("dialBtcd: %v", dialErr)
    }
    defer func() { btcd.close(); btcd = nil }()
    openDB(filepath.Join(t.TempDir(), "watches.db"))
    defer closeDB()
    stopNotify()
    defer stopNotify()
    watchCmd(b, 42, watchedAddr+" John") // alias "John"
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
    if !strings.Contains(found, "watched address "+short(watchedAddr)+" (John)") {
        t.Fatalf("notification missing alias: %q", found)
    }
    if !strings.Contains(found, "250 000 000 sats") {
        t.Fatalf("expected 250 000 000 sats in notification, got: %q", found)
    }
    // fee 0.0001 BTC = 10000 sat over 100 vB = 100 sat/vB; 100 >= 50 (2-block) → ~10-20 min
    for _, want := range []string{"Fee:", "10 000 sats", "Fee rate:", "100 sat/vB", "Confirms:", "~10-20 min"} {
        if !strings.Contains(found, want) {
            t.Fatalf("notification missing %q: %q", want, found)
        }
    }
    // the mempool notification also registers a one-shot confirmation watch, so
    // the chat gets a second message once this transaction is mined
    var registered bool
    for _, c := range txwatches.Confirms([]string{txid}) {
        if c.ChatID == 42 && c.Addr == watchedAddr {
            registered = true
        }
    }
    if !registered {
        t.Fatalf("expected a confirmation watch registered for the watched-address tx")
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
    stopNotify()
    defer stopNotify()
    openDB(filepath.Join(t.TempDir(), "watches.db"))
    defer closeDB()
    var addr = "1A1zP1eP5QGefi2DMPTfTL5SLmv7DivfNa"
    update(b, Update{Message: &Message{Chat: Chat{ID: 1}, Text: "/watch " + addr}})
    update(b, Update{Message: &Message{Chat: Chat{ID: 2}, Text: "/watch " + addr}})
    if countWatchers(addr) != 2 {
        t.Fatalf("expected 2 watchers, got %d", countWatchers(addr))
    }
    // chat 2 unwatches; only chat 1 must remain, both as a live watcher and in the store
    update(b, Update{Message: &Message{Chat: Chat{ID: 2}, Text: "/unwatch " + addr}})
    if sent[len(sent)-1] != "Stopped watching "+addr+"." {
        t.Fatalf("unexpected unwatch reply: %#v", sent)
    }
    if got := watcherChats(addr); len(got) != 1 || got[0] != 1 {
        t.Fatalf("expected only chat 1 to remain, got %#v", got)
    }
    var records, _ = watches.List()
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
    if countWatchers(addr) != 0 {
        t.Fatalf("expected no watchers left, got %d", countWatchers(addr))
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
    openDB(filepath.Join(t.TempDir(), "watches.db"))
    defer closeDB()
    stopNotify()
    defer stopNotify()
    // nothing watched yet
    update(b, Update{Message: &Message{Chat: Chat{ID: 1}, Text: "/watches"}})
    if sent[len(sent)-1] != "You're not watching anything yet." {
        t.Fatalf("unexpected empty reply: %q", sent[len(sent)-1])
    }
    var addr = "1A1zP1eP5QGefi2DMPTfTL5SLmv7DivfNa"
    var txid = "f21b47a9143a23e80cc59e81588d21558b394005580b285961957cb3bed5b3e0"
    watches.Add(1, addr, "")
    txwatches.Add(txid, 1, "")
    watches.Add(2, "someoneElsesAddress", "")
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
    watches.Add(3, "a<b>c", "")
    update(b, Update{Message: &Message{Chat: Chat{ID: 3}, Text: "/watches"}})
    if last := sent[len(sent)-1]; !strings.Contains(last, "a&lt;b&gt;c") {
        t.Fatalf("expected HTML-escaped watch id, got: %q", last)
    }
}

func TestTxConfirmation(t *testing.T) {
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
    var txid = "f21b47a9143a23e80cc59e81588d21558b394005580b285961957cb3bed5b3e0"
    var srv = newFakeBtcdServer(t, func(method string, params json.RawMessage) (interface{}, error) {
        switch method {
        case "notifyblocks":
            return nil, nil
        case "getblock":
            var p []interface{}
            json.Unmarshal(params, &p)
            if len(p) > 1 && p[1].(float64) == 1 {
                return map[string]any{"height": 100, "tx": []string{txid}}, nil
            }
            return map[string]any{"hash": "0000000000000000abc", "height": 100, "time": 1700000000, "size": 300,
                "rawtx": []map[string]any{{"txid": "cb", "size": 100, "vin": []map[string]any{{"coinbase": "03"}}, "vout": []map[string]any{{"value": 50.0}}}}}, nil
        }
        return nil, nil
    })
    defer srv.Close()
    btcd = dialFakeBtcd(t, srv, notifier{bot: b})
    defer func() { btcd.close(); btcd = nil }()
    openDB(filepath.Join(t.TempDir(), "watches.db"))
    defer closeDB()
    stopNotify()
    defer stopNotify()
    txwatches.Add(txid, 7, "Alice")
    // notifyblocks makes the fake push a blockconnected, which the notifier turns
    // into a checkConfirmations that finds txid in the block and messages chat 7.
    if err := btcd.notifyBlocks(context.Background()); err != nil {
        t.Fatalf("notifyBlocks: %v", err)
    }
    var found string
    var deadline = time.Now().Add(3 * time.Second)
    for time.Now().Before(deadline) {
        sentMu.Lock()
        for _, m := range sent {
            if strings.Contains(m, "was confirmed") { found = m }
        }
        sentMu.Unlock()
        if found != "" { break }
        time.Sleep(10 * time.Millisecond)
    }
    if found == "" {
        t.Fatalf("expected a confirmation notification, got: %#v", sent)
    }
    for _, want := range []string{"Watched transaction " + short(txid), "(Alice)", "confirmed in block #100 after"} {
        if !strings.Contains(found, want) {
            t.Fatalf("confirmation missing %q: %q", want, found)
        }
    }
}

func TestDurationText(t *testing.T) {
    var cases = []struct {
        d    time.Duration
        want string
    }{
        {30 * time.Second, "30 sec"},
        {5 * time.Minute, "5 min"},
        {90 * time.Minute, "1 h 30 min"},
        {2 * time.Hour, "2 h"},
    }
    for _, c := range cases {
        if got := durationText(c.d); got != c.want {
            t.Errorf("durationText(%v) = %q, want %q", c.d, got, c.want)
        }
    }
}

func TestAddrConfirmation(t *testing.T) {
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
    var addr = "1A1zP1eP5QGefi2DMPTfTL5SLmv7DivfNa"
    var txid = "f21b47a9143a23e80cc59e81588d21558b394005580b285961957cb3bed5b3e0"
    var srv = newFakeBtcdServer(t, func(method string, params json.RawMessage) (interface{}, error) {
        if method == "getblock" {
            return map[string]any{"height": 200, "tx": []string{txid}}, nil
        }
        return nil, nil
    })
    defer srv.Close()
    btcd = dialFakeBtcd(t, srv, &recordingHandler{})
    defer func() { btcd.close(); btcd = nil }()
    stopNotify()
    defer stopNotify()
    txwatches.AddAddrConfirm(txid, 7, addr, "John")
    checkConfirmations(b, "hash200")
    sentMu.Lock()
    defer sentMu.Unlock()
    var found string
    for _, m := range sent {
        if strings.Contains(m, "was confirmed") {
            found = m
        }
    }
    if found == "" {
        t.Fatalf("expected a confirmation message, got %#v", sent)
    }
    for _, want := range []string{"Transaction " + short(txid), "on watched address " + short(addr), "(John)", "confirmed in block #200 after"} {
        if !strings.Contains(found, want) {
            t.Fatalf("address confirmation missing %q: %q", want, found)
        }
    }
}

func TestAddrConfirmDedup(t *testing.T) {
    txwatches.Reset()
    defer txwatches.Reset()
    txwatches.AddAddrConfirm("txabc", 5, "addrX", "Alias")
    txwatches.AddAddrConfirm("txabc", 5, "addrX", "Alias")
    txwatches.Add("txabc", 5, "")
    var n = len(txwatches.Confirms([]string{"txabc"}))
    // the two identical address confirmations dedup to one; the direct watch (addr "") stays distinct
    if n != 2 {
        t.Fatalf("expected 2 entries (deduped addr-confirm + distinct direct watch), got %d", n)
    }
}

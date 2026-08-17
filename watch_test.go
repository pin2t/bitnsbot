package main

import "encoding/json"
import "fmt"
import "net/http"
import "net/http/httptest"
import "path/filepath"
import "strings"
import "sync"
import "testing"
import "time"

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

// TestWatchNotification drives the full address-watch notification path. Under
// Core there is no server-side filter and no server push: ZMQ delivers every
// mempool transaction and the bot matches locally, so the test hands the raw
// transaction to broadcast exactly as zmq.go does on a rawtx frame.
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
    var srv = newFakeCoreServer(t, func(method string, params []interface{}) (interface{}, error) {
        switch method {
        case "validateaddress":
            return map[string]any{"isvalid": true, "address": watchedAddr, "scriptPubKey": "76a914aa88ac"}, nil
        case "scantxoutset":
            return map[string]any{"success": true, "unspents": []map[string]any{}}, nil
        case "decoderawtransaction":
            return map[string]any{
                "txid": txid,
                "vout": []map[string]any{{"value": 2.5, "n": 0, "scriptPubKey": map[string]any{"address": watchedAddr, "hex": "76a914aa88ac"}}},
            }, nil
        case "getrawtransaction":
            if reqTxid, _ := params[0].(string); reqTxid == "prevtx" {
                return map[string]any{"vout": []map[string]any{{"value": 2.5001, "n": 0, "scriptPubKey": map[string]any{"address": "1InputAddr"}}}}, nil
            }
            return map[string]any{
                "txid": txid, "confirmations": 0, "size": 110, "vsize": 100,
                "vin":  []map[string]any{{"txid": "prevtx", "vout": 0}},
                "vout": []map[string]any{{"value": 2.5, "n": 0, "scriptPubKey": map[string]any{"address": watchedAddr, "hex": "76a914aa88ac"}}},
            }, nil
        case "getmempoolentry":
            // fee 0.0001 BTC = 10000 sat over 100 vB = 100 sat/vB
            return map[string]any{"vsize": 100, "fees": map[string]any{"base": 0.0001}}, nil
        case "estimatesmartfee":
            var rate = 0.0002 // 6-block target → 20 sat/vB
            if blocks, _ := params[0].(float64); blocks == 2 {
                rate = 0.0005 // 2-block target → 50 sat/vB
            }
            return map[string]any{"feerate": rate, "blocks": params[0]}, nil
        }
        return nil, nil
    })
    core = newFakeCoreConn(t, srv)
    defer func() { core = nil }()
    // Populate cachedFees so confEstimate can map fee rates to ETA without
    // calling Core's estimatesmartfee.
    feesMu.Lock()
    cachedFees = recommendedFees{fastest: 50, halfHour: 30, hour: 20, economy: 10, minimum: 1}
    cachedFeesOK = true
    cachedFeesCount = 1000
    feesMu.Unlock()
    openDB(filepath.Join(t.TempDir(), "watches.db"))
    defer closeDB()
    stopNotify()
    resetWatched()
    defer stopNotify()
    watchCmd(b, 42, watchedAddr+" John") // alias "John"
    broadcast("deadbeefrawtxhex")
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
        if strings.Contains(m, "receiving") || strings.Contains(m, "received") {
            found = m
        }
    }
    if found == "" {
        t.Fatalf("expected a watch notification, got: %#v", sent)
    }
    if !strings.Contains(found, short(watchedAddr)) || !strings.Contains(found, txid) {
        t.Fatalf("notification missing address/txid: %q", found)
    }
    if !strings.Contains(found, short(watchedAddr)+" (John)") {
        t.Fatalf("notification missing alias: %q", found)
    }
    if !strings.Contains(found, "2.5 BTC") {
        t.Fatalf("expected 2.5 BTC in notification, got: %q", found)
    }
    // fee 0.0001 BTC = 10000 sat over 100 vB = 100 sat/vB; 100 >= 50 (2-block) → ~10-20 min
    for _, want := range []string{"ETA ~10-20 min"} {
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
    core = nil
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
    if sent[len(sent)-1] != "Stopped watching "+addr {
        t.Fatalf("unexpected unwatch reply: %#v", sent)
    }
    if got := watcherChats(addr); len(got) != 1 || got[0] != 1 {
        t.Fatalf("expected only chat 1 to remain, got %#v", got)
    }
    var records, _ = watches.List()
    if len(records) != 1 || records[0].Chat != 1 {
        t.Fatalf("expected only chat 1's record in store, got %#v", records)
    }
    // unwatching something not watched removes nothing
    update(b, Update{Message: &Message{Chat: Chat{ID: 1}, Text: "/unwatch bogusaddr"}})
    if sent[len(sent)-1] != "You're not watching bogusaddr" {
        t.Fatalf("unexpected not-watching reply: %q", sent[len(sent)-1])
    }
    // bare /unwatch marks the chat pending and consumes the next plain message
    update(b, Update{Message: &Message{Chat: Chat{ID: 1}, Text: "/unwatch"}})
    if !pendingUnwatchChats[1] {
        t.Fatalf("expected chat 1 pending for unwatch")
    }
    update(b, Update{Message: &Message{Chat: Chat{ID: 1}, Text: addr}})
    if sent[len(sent)-1] != "Stopped watching "+addr {
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
    if sent[len(sent)-1] != "You're not watching anything yet" {
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

// TestWatchLimit seeds a chat up to maxSubscriptionsPerChat and checks that a new
// /watch is rejected with the polite limit message instead of being stored.
func TestWatchLimit(t *testing.T) {
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
    core = nil
    stopNotify()
    defer stopNotify()
    txwatches.Reset()
    defer txwatches.Reset()
    openDB(filepath.Join(t.TempDir(), "watches.db"))
    defer closeDB()
    // seed the chat exactly at the limit
    for i := 0; i < maxSubscriptionsPerChat; i++ {
        if err := watches.Add(42, fmt.Sprintf("addr%d", i), ""); err != nil {
            t.Fatalf("seed watch %d: %v", i, err)
        }
    }
    var addr = "1A1zP1eP5QGefi2DMPTfTL5SLmv7DivfNa"
    watchCmd(b, 42, addr)
    if last := sent[len(sent)-1]; !strings.Contains(last, "limit") {
        t.Fatalf("expected a limit rejection, got %q", last)
    }
    // the rejected watch must not have been stored
    var records, _ = watches.List()
    for _, r := range records {
        if r.Chat == 42 && r.Address == addr {
            t.Fatalf("the rejected watch was stored anyway")
        }
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
    var srv = newFakeCoreServer(t, func(method string, params []interface{}) (interface{}, error) {
        var p = params
        _ = p
        switch method {
        case "notifyblocks":
            return nil, nil
        case "getblock":
            if len(p) > 1 && p[1].(float64) == 1 {
                return map[string]any{"height": 100, "tx": []string{txid}}, nil
            }
            return map[string]any{"hash": "0000000000000000abc", "height": 100, "time": 1700000000, "size": 300,
                "tx": []map[string]any{{"txid": "cb", "size": 100, "vin": []map[string]any{{"coinbase": "03"}}, "vout": []map[string]any{{"value": 50.0}}}}}, nil
        }
        return nil, nil
    })
    defer srv.Close()
    core = newFakeCoreConn(t, srv)
    defer func() { core = nil }()
    openDB(filepath.Join(t.TempDir(), "watches.db"))
    defer closeDB()
    stopNotify()
    defer stopNotify()
    txwatches.Add(txid, 7, "Alice")
    // a hashblock frame is what zmq.go turns into a checkConfirmations that finds
    // txid in the new block and messages chat 7
    go processConfirms(b, "0000000000000000abc")
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
    for _, want := range []string{"Transaction " + short(txid), "(Alice)", "was confirmed in block #100 after"} {
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
        if got := durationText(c.d, 0); got != c.want {
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
    var srv = newFakeCoreServer(t, func(method string, params []interface{}) (interface{}, error) {
        if method == "getblock" {
            return map[string]any{"height": 200, "tx": []string{txid}}, nil
        }
        return nil, nil
    })
    defer srv.Close()
    core = newFakeCoreConn(t, srv)
    defer func() { core = nil }()
    stopNotify()
    defer stopNotify()
    txwatches.AddAddrConfirm(txid, 7, addr, "John", txwatches.Summary{})
    processConfirms(b, "hash200")
    sentMu.Lock()
    defer sentMu.Unlock()
    var found string
    for _, m := range sent {
        if strings.Contains(m, "Confirmed") {
            found = m
        }
    }
    if found == "" {
        t.Fatalf("expected a confirmation message, got %#v", sent)
    }
    for _, want := range []string{txid, short(addr), "(John)", "Confirmed in block #200 after"} {
        if !strings.Contains(found, want) {
            t.Fatalf("address confirmation missing %q: %q", want, found)
        }
    }
}

func TestAddrConfirmDedup(t *testing.T) {
    txwatches.Reset()
    defer txwatches.Reset()
    txwatches.AddAddrConfirm("txabc", 5, "addrX", "Alias", txwatches.Summary{})
    txwatches.AddAddrConfirm("txabc", 5, "addrX", "Alias", txwatches.Summary{})
    txwatches.Add("txabc", 5, "")
    var n = len(txwatches.Confirms([]string{"txabc"}))
    // the two identical address confirmations dedup to one; the direct watch (addr "") stays distinct
    if n != 2 {
        t.Fatalf("expected 2 entries (deduped addr-confirm + distinct direct watch), got %d", n)
    }
}

// spendCoreServer fakes Core for a transaction *spending* watchedAddr: the
// decoded transaction pays a stranger (plus change back to watchedAddr when
// change is true), and the prevout its input references belongs to watchedAddr —
// which is the only way the sending side is visible, since a decoded transaction
// names receiving addresses only.
func spendCoreServer(t *testing.T, watchedAddr, txid string, change bool) *httptest.Server {
    var inValue = 1.0001
    var outs = []map[string]any{
        {"value": 1.0, "n": 0, "scriptPubKey": map[string]any{"address": "1RecipientAddr", "hex": "76a914bb88ac"}},
    }
    if change {
        inValue = 2.5
        outs = append(outs, map[string]any{"value": 1.4999, "n": 1, "scriptPubKey": map[string]any{"address": watchedAddr, "hex": "76a914aa88ac"}})
    }
    return newFakeCoreServer(t, func(method string, params []interface{}) (interface{}, error) {
        switch method {
        case "validateaddress":
            return map[string]any{"isvalid": true, "address": watchedAddr, "scriptPubKey": "76a914aa88ac"}, nil
        case "scantxoutset":
            return map[string]any{"success": true, "unspents": []map[string]any{}}, nil
        case "decoderawtransaction":
            return map[string]any{"txid": txid, "vout": outs}, nil
        case "getrawtransaction":
            if reqTxid, _ := params[0].(string); reqTxid == "prevtx" {
                return map[string]any{"vout": []map[string]any{{"value": inValue, "n": 0, "scriptPubKey": map[string]any{"address": watchedAddr, "hex": "76a914aa88ac"}}}}, nil
            }
            return map[string]any{
                "txid": txid, "confirmations": 0, "size": 110, "vsize": 100,
                "vin":  []map[string]any{{"txid": "prevtx", "vout": 0}},
                "vout": outs,
            }, nil
        case "getmempoolentry":
            return map[string]any{"vsize": 100, "fees": map[string]any{"base": 0.0001}}, nil
        case "estimatesmartfee":
            var rate = 0.0002
            if blocks, _ := params[0].(float64); blocks == 2 { rate = 0.0005 }
            return map[string]any{"feerate": rate, "blocks": params[0]}, nil
        }
        return nil, nil
    })
}

// awaitNotification drives a watch on watchedAddr against the given fake core and
// returns the address notification the chat received.
// capturedMu/captured hold what the fake Telegram server received, so a test can
// assert on messages sent after awaitNotification returns.
var capturedMu sync.Mutex
var captured []string

func sentMessages(t *testing.T) []string {
    capturedMu.Lock()
    defer capturedMu.Unlock()
    return append([]string(nil), captured...)
}

func awaitNotification(t *testing.T, btcdSrv *httptest.Server, watchedAddr string) string {
    capturedMu.Lock()
    captured = nil
    capturedMu.Unlock()
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
        capturedMu.Lock()
        captured = append(captured, body.Text)
        capturedMu.Unlock()
        json.NewEncoder(w).Encode(map[string]any{"ok": true, "result": true})
    }))
    var b = newBot("TESTTOKEN", tg.URL)
    core = newFakeCoreConn(t, btcdSrv)
    // Populate cachedFees so confEstimate can map fee rates to ETA without
    // calling Core's estimatesmartfee.
    feesMu.Lock()
    cachedFees = recommendedFees{fastest: 50, halfHour: 30, hour: 20, economy: 10, minimum: 1}
    cachedFeesOK = true
    cachedFeesCount = 1000
    feesMu.Unlock()
    openDB(filepath.Join(t.TempDir(), "watches.db"))
    stopNotify()
    resetWatched()
    // cleanup runs after the caller's assertions, so stopNotify (which resets the
    // confirmation map) can't erase what the test is about to check
    t.Cleanup(tg.Close)
    t.Cleanup(func() { core = nil })
    t.Cleanup(func() { closeDB() })
    t.Cleanup(stopNotify)
    t.Cleanup(resetWatched)
    watchCmd(b, 42, watchedAddr+" John")
    // ZMQ delivers the raw transaction; this is what zmq.go does with a rawtx frame
    broadcast("deadbeefrawtxhex")
    var deadline = time.Now().Add(3 * time.Second)
    for time.Now().Before(deadline) {
        sentMu.Lock()
        var found string
        for _, m := range sent {
            if strings.Contains(m, "is sending") { found = m }
        }
        sentMu.Unlock()
        if found != "" { return found }
        time.Sleep(10 * time.Millisecond)
    }
    sentMu.Lock()
    defer sentMu.Unlock()
    t.Fatalf("expected a watch notification, got: %#v", sent)
    return ""
}

// Spending the whole balance with nothing back: the address only appears in the
// inputs, so the notification is an outgoing one with no change/net lines.
func TestSpendNotification(t *testing.T) {
    var watchedAddr = "1A1zP1eP5QGefi2DMPTfTL5SLmv7DivfNa"
    var txid = "f21b47a9143a23e80cc59e81588d21558b394005580b285961957cb3bed5b3e0"
    var srv = spendCoreServer(t, watchedAddr, txid, false)
    defer srv.Close()
    var got = awaitNotification(t, srv, watchedAddr)
    if !strings.Contains(got, short(watchedAddr)+" (John)") {
        t.Fatalf("expected an outgoing-transaction notification: %q", got)
    }
    if strings.Contains(got, "New transaction on") {
        t.Fatalf("a spend must not be reported as an incoming transaction: %q", got)
    }
    // the whole 1.0001 BTC input left the address
    if !strings.Contains(got, "Sending:") || !strings.Contains(got, "1 BTC") {
        t.Fatalf("expected the sent amount: %q", got)
    }
    for _, unwanted := range []string{"Change back:", "Net:", "Amount:"} {
        if strings.Contains(got, unwanted) {
            t.Fatalf("unexpected %q with no change output: %q", unwanted, got)
        }
    }
    for _, want := range []string{"Fee:", "10 000 sats (100 sat/vB)", "ETA:", "~10-20 min"} {
        if !strings.Contains(got, want) {
            t.Fatalf("notification missing %q: %q", want, got)
        }
    }
    // an outgoing transaction is worth a confirmation follow-up too
    var registered bool
    for _, c := range txwatches.Confirms([]string{txid}) {
        if c.ChatID == 42 && c.Addr == watchedAddr { registered = true }
    }
    if !registered {
        t.Fatal("expected a confirmation watch registered for the outgoing tx")
    }
}

// The usual spend: part goes to the recipient and the rest returns as change, so
// the address is in both the inputs and the outputs and the report is a net move.
func TestSpendWithChangeNotification(t *testing.T) {
    var watchedAddr = "1A1zP1eP5QGefi2DMPTfTL5SLmv7DivfNa"
    var txid = "f21b47a9143a23e80cc59e81588d21558b394005580b285961957cb3bed5b3e0"
    var srv = spendCoreServer(t, watchedAddr, txid, true)
    defer srv.Close()
    var got = awaitNotification(t, srv, watchedAddr)
    if !strings.Contains(got, "sending") {
        t.Fatalf("expected an outgoing-transaction notification: %q", got)
    }
    // 2.5 spent, 1.4999 back as change, so the address is down 1.0001 BTC
    for _, want := range []string{"Sending:", "2.5 BTC", "Change back:", "1.4999 BTC", "Net:", "-1.0001 BTC"} {
        if !strings.Contains(got, want) {
            t.Fatalf("notification missing %q: %q", want, got)
        }
    }
}

// Spend detection needs to know which outpoints a watched address owns, because
// a spend names the outpoint and never the address. Core answers that directly
// with scantxoutset — no history replay, unlike the btcd path — and seeding runs
// in the background, so this waits for it.
func TestSeedOutpoints(t *testing.T) {
    var addr = "1A1zP1eP5QGefi2DMPTfTL5SLmv7DivfNa"
    var script = "76a914aabbccddeeff88ac"
    var srv = newFakeCoreServer(t, func(method string, params []interface{}) (interface{}, error) {
        switch method {
        case "validateaddress":
            return map[string]any{"isvalid": true, "address": addr, "scriptPubKey": script}, nil
        case "scantxoutset":
            // the scan must be asked for every address in one pass
            if action, _ := params[0].(string); action != "start" {
                t.Fatalf("scantxoutset action = %v, want start", params[0])
            }
            return map[string]any{"success": true, "unspents": []map[string]any{
                {"txid": "tx2", "vout": 0, "scriptPubKey": script, "amount": 1.0, "height": 100},
                {"txid": "tx4", "vout": 1, "scriptPubKey": script, "amount": 0.5, "height": 200},
                {"txid": "other", "vout": 0, "scriptPubKey": "76a914ffff88ac", "amount": 9.0, "height": 300},
            }}, nil
        }
        return nil, nil
    })
    core = newFakeCoreConn(t, srv)
    resetWatched()
    t.Cleanup(func() { core = nil; resetWatched() })
    seedOutpoints([]string{addr})
    var deadline = time.Now().Add(5 * time.Second)
    for time.Now().Before(deadline) {
        watchedMu.Lock()
        var n = len(watchedOutpoints)
        watchedMu.Unlock()
        if n >= 2 { break }
        time.Sleep(10 * time.Millisecond)
    }
    watchedMu.Lock()
    defer watchedMu.Unlock()
    if watchedScripts[script] != addr {
        t.Fatalf("the address's scriptPubKey was not registered for matching: %v", watchedScripts)
    }
    var want = map[outpoint]string{{"tx2", 0}: addr, {"tx4", 1}: addr}
    for op, owner := range want {
        if watchedOutpoints[op] != owner {
            t.Fatalf("outpoint %v not seeded (got %q): %v", op, watchedOutpoints[op], watchedOutpoints)
        }
    }
    // an unspent belonging to a different script must not be attributed to us
    if _, ok := watchedOutpoints[outpoint{"other", 0}]; ok {
        t.Fatal("an unrelated address's outpoint was seeded")
    }
}


// Core publishes a transaction on rawtx twice — when it enters the mempool and
// again when it is mined — so broadcasting the same transaction twice must
// produce exactly one message. Before this was fixed a watched payment produced
// three messages: the mempool one, the confirmation, and a second "New
// transaction" from the mined republish.
func TestBroadcastDedups(t *testing.T) {
    var watchedAddr = "1A1zP1eP5QGefi2DMPTfTL5SLmv7DivfNa"
    var txid = "f21b47a9143a23e80cc59e81588d21558b394005580b285961957cb3bed5b3e0"
    var srv = spendCoreServer(t, watchedAddr, txid, false)
    defer srv.Close()
    var got = awaitNotification(t, srv, watchedAddr)
    if !strings.Contains(got, "is sending") {
        t.Fatalf("expected a first notification, got %q", got)
    }
    // the mined republish of the very same transaction
    var before = len(sentMessages(t))
    broadcast("deadbeefrawtxhex")
    time.Sleep(300 * time.Millisecond)
    if after := len(sentMessages(t)); after != before {
        t.Fatalf("a repeat broadcast sent %d extra message(s); it must send none", after-before)
    }
    // a *different* transaction on the same address still notifies
    if alreadyNotified("some-other-txid") {
        t.Fatal("an unseen txid must not be reported as already notified")
    }
}

// A transaction first seen already mined — a coinbase paying a watched address,
// or one that arrived while the bot was down — has no earlier sighting to dedup
// against, so it must still notify.
func TestBroadcastNotifiesFirstSeenMined(t *testing.T) {
    stopNotify()
    defer stopNotify()
    if alreadyNotified("mined-first-sighting") {
        t.Fatal("a transaction never seen before must notify, even when already confirmed")
    }
    if !alreadyNotified("mined-first-sighting") {
        t.Fatal("the second sighting of the same transaction must be suppressed")
    }
}

// The confirmation message names the block it was mined in, so that block should
// be one tap away like the txid and address are.
func TestConfirmationLinksBlock(t *testing.T) {
    var txid = "f21b47a9143a23e80cc59e81588d21558b394005580b285961957cb3bed5b3e0"
    var addr = "1A1zP1eP5QGefi2DMPTfTL5SLmv7DivfNa"
    var markupMu sync.Mutex
    var markups []map[string]any
    var tg = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
        var body map[string]any
        json.NewDecoder(r.Body).Decode(&body)
        markupMu.Lock()
        markups = append(markups, body)
        markupMu.Unlock()
        json.NewEncoder(w).Encode(map[string]any{"ok": true, "result": true})
    }))
    defer tg.Close()
    var b = newBot("TESTTOKEN", tg.URL)
    var srv = newFakeCoreServer(t, func(method string, params []interface{}) (interface{}, error) {
        if method == "getblock" {
            return map[string]any{"height": 959126, "tx": []string{txid}}, nil
        }
        return nil, nil
    })
    core = newFakeCoreConn(t, srv)
    stopNotify()
    t.Cleanup(func() { core = nil })
    t.Cleanup(stopNotify)
    txwatches.AddAddrConfirm(txid, 42, addr, "", txwatches.Summary{})
    processConfirms(b, "0000000000000000abc")
    var deadline = time.Now().Add(3 * time.Second)
    for time.Now().Before(deadline) {
        markupMu.Lock()
        var n = len(markups)
        markupMu.Unlock()
        if n > 0 { break }
        time.Sleep(10 * time.Millisecond)
    }
    markupMu.Lock()
    defer markupMu.Unlock()
    if len(markups) == 0 { t.Fatal("no confirmation message was sent") }
    var body = markups[len(markups)-1]
    if text, _ := body["text"].(string); !strings.Contains(text, "block #959126") {
        t.Fatalf("confirmation text = %q", body["text"])
    }
    var markup, ok = body["reply_markup"].(map[string]any)
    if !ok { t.Fatal("the confirmation message carries no buttons") }
    var found []string
    for _, row := range markup["inline_keyboard"].([]any) {
        for _, btn := range row.([]any) {
            found = append(found, btn.(map[string]any)["callback_data"].(string))
        }
    }
    var want = map[string]bool{txid: false, addr: false, "959126": false}
    for _, data := range found {
        if _, expected := want[data]; expected { want[data] = true }
    }
    for id, seen := range want {
        if !seen {
            t.Errorf("no button for %q — buttons were %v", id, found)
        }
    }
}

package main

import "encoding/json"
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
    var btcdSrv = newFakeCoreServer(t, func(method string, params []interface{}) (interface{}, error) {
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
    core = newFakeCoreClient(t, btcdSrv)
    defer func() { core = nil }()
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
    core = newFakeCoreClient(t, srv)
    defer func() { core = nil }()
    openDB(filepath.Join(t.TempDir(), "watches.db"))
    defer closeDB()
    stopNotify()
    defer stopNotify()
    txwatches.Add(txid, 7, "Alice")
    // a hashblock frame is what zmq.go turns into a checkConfirmations that finds
    // txid in the new block and messages chat 7
    go checkConfirmations(b, "0000000000000000abc")
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
    var srv = newFakeCoreServer(t, func(method string, params []interface{}) (interface{}, error) {
        if method == "getblock" {
            return map[string]any{"height": 200, "tx": []string{txid}}, nil
        }
        return nil, nil
    })
    defer srv.Close()
    core = newFakeCoreClient(t, srv)
    defer func() { core = nil }()
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
func awaitNotification(t *testing.T, btcdSrv *httptest.Server, watchedAddr string) string {
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
    var b = newBot("TESTTOKEN", tg.URL)
    core = newFakeCoreClient(t, btcdSrv)
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
            if strings.Contains(m, "watched address") { found = m }
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
    if !strings.Contains(got, "Outgoing transaction from watched address "+short(watchedAddr)+" (John)") {
        t.Fatalf("expected an outgoing-transaction notification: %q", got)
    }
    if strings.Contains(got, "New transaction on") {
        t.Fatalf("a spend must not be reported as an incoming transaction: %q", got)
    }
    // the whole 1.0001 BTC input left the address
    if !strings.Contains(got, "Sent:") || !strings.Contains(got, "100 010 000 sats") {
        t.Fatalf("expected the sent amount: %q", got)
    }
    for _, unwanted := range []string{"Change back:", "Net:", "Amount:"} {
        if strings.Contains(got, unwanted) {
            t.Fatalf("unexpected %q with no change output: %q", unwanted, got)
        }
    }
    for _, want := range []string{"Fee:", "10 000 sats", "Fee rate:", "100 sat/vB", "Confirms:", "~10-20 min"} {
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
    if !strings.Contains(got, "Outgoing transaction from watched address") {
        t.Fatalf("expected an outgoing-transaction notification: %q", got)
    }
    // 2.5 spent, 1.4999 back as change, so the address is down 1.0001 BTC
    for _, want := range []string{"Sent:", "250 000 000 sats", "Change back:", "149 990 000 sats", "Net:", "-100 010 000 sats"} {
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
    core = newFakeCoreClient(t, srv)
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


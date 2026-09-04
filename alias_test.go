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

// Renaming has to reach three places at once: the stored record, the notifier
// goroutine (which holds its alias by value, so it is restarted) and any
// confirmation already pending. This drives the whole path — watch, rename,
// then a transaction — and reads the alias off the message that comes out.
func TestSetAliasRenamesALiveWatch(t *testing.T) {
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
            return map[string]any{"vsize": 100, "fees": map[string]any{"base": 0.0001}}, nil
        }
        return nil, nil
    })
    core = newFakeCoreConn(t, srv)
    defer func() { core = nil }()
    openDB(filepath.Join(t.TempDir(), "watches.db"))
    defer closeDB()
    stopNotify()
    resetWatched()
    defer stopNotify()

    // the app's two steps: the bell files the watch, then the dialog names it
    if err := addWatch(b, 42, watchedAddr, ""); err != nil { t.Fatalf("add watch: %v", err) }
    var renamed, err = setAlias(b, 42, watchedAddr, "John")
    if err != nil || !renamed { t.Fatalf("setAlias = %v, %v; want it to rename the watch", renamed, err) }
    var list, lerr = watches.List()
    if lerr != nil { t.Fatalf("list: %v", lerr) }
    if len(list) != 1 || list[0].Alias != "John" {
        t.Fatalf("stored watches = %#v, want one named John", list)
    }

    broadcast("deadbeefrawtxhex")
    var deadline = time.Now().Add(3 * time.Second)
    var found string
    for time.Now().Before(deadline) && found == "" {
        sentMu.Lock()
        for _, m := range sent {
            if strings.Contains(m, "receiving") || strings.Contains(m, "received") { found = m }
        }
        sentMu.Unlock()
        time.Sleep(10 * time.Millisecond)
    }
    if found == "" {
        t.Fatalf("no notification arrived after the rename: %#v", sent)
    }
    // the notifier holds its alias by value, so this is what the restart buys
    if !strings.Contains(found, short(watchedAddr)+" (John)") {
        t.Errorf("the notification does not carry the new alias: %q", found)
    }
    // and the confirmation the mempool sighting queued carries it too
    var confirmed = txwatches.Confirms([]string{txid})
    if len(confirmed) != 1 || confirmed[0].Alias != "John" {
        t.Errorf("pending confirmation = %#v, want it renamed to John", confirmed)
    }
}

// Renaming is chat-scoped like every other watch operation, and a watch nobody
// holds is not renamed into existence.
func TestSetAliasIsScopedToTheChat(t *testing.T) {
    var b = newBot("TESTTOKEN", "http://127.0.0.1:1")
    openDB(filepath.Join(t.TempDir(), "watches.db"))
    defer closeDB()
    stopNotify()
    resetWatched()
    defer stopNotify()
    var addr = "1A1zP1eP5QGefi2DMPTfTL5SLmv7DivfNa"
    if err := addWatch(b, 42, addr, "Mine"); err != nil { t.Fatalf("add watch: %v", err) }
    if renamed, err := setAlias(b, 99, addr, "Theirs"); err != nil || renamed {
        t.Errorf("setAlias for another chat = %v, %v; want it to rename nothing", renamed, err)
    }
    var list, _ = watches.List()
    if len(list) != 1 || list[0].Alias != "Mine" {
        t.Errorf("watches = %#v; another chat renamed one", list)
    }
    if renamed, err := setAlias(b, 42, "1BoatSLRHtKNngkdXEeobR76b53LETtpyT", "Ghost"); err != nil || renamed {
        t.Errorf("renaming an unwatched address = %v, %v; want nothing renamed", renamed, err)
    }
}

// A transaction watch is renamed in the in-memory list the same way, and shows
// up renamed in /watches.
func TestSetAliasOnATransactionWatch(t *testing.T) {
    var b = newBot("TESTTOKEN", "http://127.0.0.1:1")
    openDB(filepath.Join(t.TempDir(), "watches.db"))
    defer closeDB()
    stopNotify()
    defer stopNotify()
    var txid = "f21b47a9143a23e80cc59e81588d21558b394005580b285961957cb3bed5b3e0"
    if err := addWatch(b, 42, txid, ""); err != nil { t.Fatalf("add watch: %v", err) }
    if renamed, err := setAlias(b, 42, txid, "Payout"); err != nil || !renamed {
        t.Fatalf("setAlias = %v, %v; want it renamed", renamed, err)
    }
    var entries = txwatches.For(42)
    if len(entries) != 1 || entries[0].Alias != "Payout" {
        t.Errorf("transaction watches = %#v, want one named Payout", entries)
    }
}

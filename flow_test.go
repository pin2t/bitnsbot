package main

import "encoding/json"
import "net/http"
import "net/http/httptest"
import "path/filepath"
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
    update(bot, Update{Message: &Message{Chat: Chat{ID: 1}, Text: "hello world"}})
    if len(sent) != 2 || sent[1] != "Info: hello world" {
        t.Fatalf("unexpected second reply: %#v", sent)
    }
    if pendingInfoChats[1] {
        t.Fatalf("expected chat 1 pending flag cleared")
    }
    update(bot, Update{Message: &Message{Chat: Chat{ID: 2}, Text: "/info direct arg"}})
    if len(sent) != 3 || sent[2] != "Info: direct arg" {
        t.Fatalf("unexpected third reply: %#v", sent)
    }
    update(bot, Update{Message: &Message{Chat: Chat{ID: 3}, Text: "/start"}})
    if len(sent) != 4 || sent[3] != "Hello! I'm bitnsbot. I'll notify you about Bitcoin network events." {
        t.Fatalf("unexpected fourth reply: %#v", sent)
    }
    update(bot, Update{Message: &Message{Chat: Chat{ID: 4}, Text: "just chatting, no pending info"}})
    if len(sent) != 4 {
        t.Fatalf("expected no reply for plain text without pending state, got: %#v", sent)
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

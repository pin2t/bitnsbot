package main

import "encoding/json"
import "net/http"
import "net/http/httptest"
import "strings"
import "testing"

// Buttons carry the *full* id as callback_data and the shortened id as the
// label, so what the user taps matches what the message shows.
func TestButtonRows(t *testing.T) {
    var txid = "f21b47a9143a23e80cc59e81588d21558b394005580b285961957cb3bed5b3e0"
    var addr = "1A1zP1eP5QGefi2DMPTfTL5SLmv7DivfNa"
    var rows = buttonRows([]string{txid, addr})
    if len(rows) != 1 || len(rows[0]) != 2 {
        t.Fatalf("expected one row of two buttons, got %v", rows)
    }
    if rows[0][0]["callback_data"] != txid {
        t.Fatalf("callback_data = %q, want the full txid", rows[0][0]["callback_data"])
    }
    if rows[0][0]["text"] != short(txid) {
        t.Fatalf("label = %q, want the shortened id %q", rows[0][0]["text"], short(txid))
    }
    // a txid is exactly 64 characters — Telegram's callback_data limit — so it
    // has to travel with no prefix at all
    if len(txid) != 64 {
        t.Fatalf("fixture txid is %d chars, the limit case is 64", len(txid))
    }
}

func TestButtonRowsFilters(t *testing.T) {
    var addr = "1A1zP1eP5QGefi2DMPTfTL5SLmv7DivfNa"
    // duplicates collapse, and the placeholders that stand in for unnamed
    // outputs are not ids at all
    var rows = buttonRows([]string{addr, addr, "", "(non-standard)", strings.Repeat("x", 65)})
    if len(rows) != 1 || len(rows[0]) != 1 {
        t.Fatalf("expected a single button, got %v", rows)
    }
    if rows[0][0]["callback_data"] != addr {
        t.Fatalf("kept the wrong entry: %v", rows[0][0])
    }
    if got := buttonRows(nil); got != nil {
        t.Fatalf("no ids should mean no keyboard, got %v", got)
    }
}

// A long input/output list must not bury the message under a keyboard.
func TestButtonRowsCaps(t *testing.T) {
    var ids []string
    for i := 0; i < 30; i++ {
        ids = append(ids, strings.Repeat("a", 20)+string(rune('a'+i)))
    }
    var rows = buttonRows(ids)
    var total int
    for _, r := range rows {
        total += len(r)
        if len(r) > 2 {
            t.Fatalf("row wider than two buttons: %v", r)
        }
    }
    if total != maxButtons {
        t.Fatalf("emitted %d buttons, want the cap of %d", total, maxButtons)
    }
}

// Tapping a button must acknowledge the query (Telegram spins the button until
// it is answered) and then run the lookup for that id.
func TestCallbackAnswersAndLooksUp(t *testing.T) {
    var mu = make(chan string, 8)
    var tg = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
        var body map[string]any
        json.NewDecoder(r.Body).Decode(&body)
        mu <- r.URL.Path
        json.NewEncoder(w).Encode(map[string]any{"ok": true, "result": true})
    }))
    defer tg.Close()
    var b = newBot("TESTTOKEN", tg.URL)
    core = nil // the lookup then replies "not configured", which is still a reply
    update(b, Update{CallbackQuery: &CallbackQuery{
        ID:      "q1",
        Data:    "1A1zP1eP5QGefi2DMPTfTL5SLmv7DivfNa",
        Message: &Message{Chat: Chat{ID: 42}},
    }})
    var calls []string
    for len(mu) > 0 {
        calls = append(calls, <-mu)
    }
    var answered, replied bool
    for _, c := range calls {
        if strings.HasSuffix(c, "answerCallbackQuery") { answered = true }
        if strings.HasSuffix(c, "sendMessage") { replied = true }
    }
    if !answered {
        t.Fatalf("the callback query was never answered: %v", calls)
    }
    if !replied {
        t.Fatalf("tapping a button produced no reply: %v", calls)
    }
}

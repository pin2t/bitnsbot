package main

import "encoding/json"
import "net/http"
import "net/http/httptest"
import "strconv"
import "strings"
import "testing"
import "bitnsbot/core"

// Every id becomes a button, in order, carried in full — a txid is exactly 64
// characters, Telegram's callback_data limit, so it has to travel as it is.
func TestButtonIDs(t *testing.T) {
    var txid = "f21b47a9143a23e80cc59e81588d21558b394005580b285961957cb3bed5b3e0"
    var addr = "1A1zP1eP5QGefi2DMPTfTL5SLmv7DivfNa"
    var got = buttonIDs([]string{txid, addr})
    if len(got) != 2 || got[0] != txid || got[1] != addr {
        t.Fatalf("buttonIDs = %v, want the txid then the address", got)
    }
    if len(txid) != 64 {
        t.Fatalf("fixture txid is %d chars, the limit case is 64", len(txid))
    }
}

// duplicates collapse, and the placeholders that stand in for unnamed
// outputs are not ids at all
func TestButtonIDsFilters(t *testing.T) {
    var addr = "1A1zP1eP5QGefi2DMPTfTL5SLmv7DivfNa"
    var got = buttonIDs([]string{addr, addr, "", "(non-standard)", strings.Repeat("x", 65)})
    if len(got) != 1 || got[0] != addr {
        t.Fatalf("expected the address alone, got %v", got)
    }
    if got := buttonIDs(nil); got != nil {
        t.Fatalf("no ids should mean no buttons, got %v", got)
    }
}

// A long input/output list must not bury the message under a keyboard.
func TestButtonIDsCaps(t *testing.T) {
    var ids []string
    for i := 0; i < 30; i++ {
        ids = append(ids, strings.Repeat("a", 20)+string(rune('a'+i)))
    }
    if got := buttonIDs(ids); len(got) != maxButtons {
        t.Fatalf("kept %d ids, want the cap of %d", len(got), maxButtons)
    }
}

// A reply goes out with the ids as a keyboard — the selection made here, the
// layout Telegram's — so the buttons a message carries are the ones it shows.
func TestSendCarriesButtons(t *testing.T) {
    var markup []string
    var tg = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
        var body struct{ ReplyMarkup json.RawMessage `json:"reply_markup"` }
        json.NewDecoder(r.Body).Decode(&body)
        markup = append(markup, string(body.ReplyMarkup))
        json.NewEncoder(w).Encode(map[string]any{"ok": true, "result": true})
    }))
    defer tg.Close()
    useTelegram(t, tg.URL)
    var addr = "1A1zP1eP5QGefi2DMPTfTL5SLmv7DivfNa"
    send("tg", 1, "hi", []string{addr, addr, "(non-standard)", "100"})
    var want = `{"inline_keyboard":[[{"callback_data":"` + addr + `","text":"1A1zP1...DivfNa"},{"callback_data":"100","text":"100"}]]}`
    if len(markup) != 1 || markup[0] != want {
        t.Fatalf("reply_markup\n got %q\nwant %q", markup, want)
    }
}

// Tapping a button runs the lookup for that id in the chat it was tapped in.
//
// the lookup then replies "not configured", which is still a reply
func TestCallbackLooksUp(t *testing.T) {
    var sent []string
    var tg = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
        var body struct {
            ChatID int64  `json:"chat_id"`
            Text   string `json:"text"`
        }
        json.NewDecoder(r.Body).Decode(&body)
        sent = append(sent, r.URL.Path+" "+strconv.FormatInt(body.ChatID, 10)+" "+body.Text)
        json.NewEncoder(w).Encode(map[string]any{"ok": true, "result": true})
    }))
    defer tg.Close()
    useTelegram(t, tg.URL)
    core.Reset()
    callback("tg", 42, "alice", "1A1zP1eP5QGefi2DMPTfTL5SLmv7DivfNa", "")
    if len(sent) != 1 || sent[0] != "/botTESTTOKEN/sendMessage 42 Bitcoin node connection is not configured" {
        t.Fatalf("tapping a button replied %q", sent)
    }
}

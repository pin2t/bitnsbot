package main

import "encoding/json"
import "net/http"
import "net/http/httptest"
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

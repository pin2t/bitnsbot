package main

import "bytes"
import "log"
import "net/http"
import "net/http/httptest"
import "strings"
import "testing"

func TestMessageLogging(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"ok":true,"result":true}`))
	}))
	defer server.Close()
	bot := NewBot("TESTTOKEN", server.URL)
	var buf bytes.Buffer
	log.SetOutput(&buf)
	defer log.SetOutput(nil)
	handleUpdate(bot, Update{Message: &Message{
		Chat: Chat{ID: 42},
		From: &User{Username: "alice"},
		Text: "/start",
	}})
	if !strings.Contains(buf.String(), "message from alice (chat 42): /start") {
		t.Fatalf("expected log line for username case, got: %q", buf.String())
	}
	buf.Reset()
	handleUpdate(bot, Update{Message: &Message{
		Chat: Chat{ID: 7},
		From: &User{FirstName: "Bob"},
		Text: "hi",
	}})
	if !strings.Contains(buf.String(), "message from Bob (chat 7): hi") {
		t.Fatalf("expected log line for first-name fallback, got: %q", buf.String())
	}
	buf.Reset()
	handleUpdate(bot, Update{Message: &Message{Chat: Chat{ID: 9}, Text: "anon"}})
	if !strings.Contains(buf.String(), "message from unknown (chat 9): anon") {
		t.Fatalf("expected log line for nil From, got: %q", buf.String())
	}
}

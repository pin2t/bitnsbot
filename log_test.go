package main

import "bytes"
import "log"
import "net/http"
import "net/http/httptest"
import "os"
import "strings"
import "testing"
import "bitnsbot/logging"

func TestMessageLogging(t *testing.T) {
    server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
        w.Write([]byte(`{"ok":true,"result":true}`))
    }))
    defer server.Close()
    bot := newBot("TESTTOKEN", server.URL)
    logging.SetVerbose(1)
    defer logging.SetVerbose(0)
    var buf bytes.Buffer
    log.SetOutput(&buf)
    defer log.SetOutput(os.Stderr)
    update(bot, Update{Message: &Message{
        Chat: Chat{ID: 42},
        From: &User{Username: "alice"},
        Text: "/start",
    }})
    if !strings.Contains(buf.String(), "message from alice (chat 42): /start") {
        t.Fatalf("expected log line for username case, got: %q", buf.String())
    }
    buf.Reset()
    update(bot, Update{Message: &Message{
        Chat: Chat{ID: 7},
        From: &User{FirstName: "Bob"},
        Text: "hi",
    }})
    if !strings.Contains(buf.String(), "message from Bob (chat 7): hi") {
        t.Fatalf("expected log line for first-name fallback, got: %q", buf.String())
    }
    buf.Reset()
    update(bot, Update{Message: &Message{Chat: Chat{ID: 9}, Text: "anon"}})
    if !strings.Contains(buf.String(), "message from unknown (chat 9): anon") {
        t.Fatalf("expected log line for nil From, got: %q", buf.String())
    }
}

func TestLoggingLevels(t *testing.T) {
    var buf bytes.Buffer
    log.SetOutput(&buf)
    defer log.SetOutput(os.Stderr)
    defer logging.SetVerbose(0)
    var cases = []struct {
        v      int
        shown  []string
        hidden []string
    }{
        {0, []string{"[ERR]", "[WARN]", "status-line"}, []string{"[INFO]", "[NET]", "[DB]"}},
        {1, []string{"[ERR]", "[WARN]", "[INFO]"}, []string{"[NET]", "[DB]"}},
        {2, []string{"[ERR]", "[WARN]", "[INFO]", "[NET]", "[DB]"}, nil},
    }
    for _, c := range cases {
        logging.SetVerbose(c.v)
        buf.Reset()
        logging.Err("e")
        logging.Warn("w")
        logging.Status("status-line")
        logging.Info("i")
        logging.Net("n")
        logging.Db("d")
        var out = buf.String()
        for _, want := range c.shown {
            if !strings.Contains(out, want) {
                t.Errorf("verbosity %d: expected %q in output, got:\n%s", c.v, want, out)
            }
        }
        for _, notWant := range c.hidden {
            if strings.Contains(out, notWant) {
                t.Errorf("verbosity %d: did not expect %q in output, got:\n%s", c.v, notWant, out)
            }
        }
    }
}

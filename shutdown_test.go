package main

import "net"
import "net/http"
import "path/filepath"
import "strings"
import "testing"
import "time"
import "bitnsbot/core"
import "bitnsbot/telegram"
import "bitnsbot/watches"

// TestShutdownDrainsHandlersBeforeClosingStore verifies the ordering the
// feature exists for: the webhook server is stopped first and waits for
// in-flight handlers to finish, so a handler using the store never sees it
// closed out from under it — and once shutdown returns, the store is closed.
// The handler is the Telegram webhook's own, reached by a real update.
//
// the store must still be open while a handler runs
//
// must wait for the in-flight handler, then close the store
func TestShutdownDrainsHandlersBeforeClosingStore(t *testing.T) {
    var err error
    err = openDB(filepath.Join(t.TempDir(), "watches.db"))
    if err != nil {
        t.Fatalf("openDB: %v", err)
    }
    core.Reset()
    var started = make(chan struct{})
    var finished = make(chan error, 1)
    var ln, lnErr = net.Listen("tcp", "127.0.0.1:0")
    if lnErr != nil {
        t.Fatalf("listen: %v", lnErr)
    }
    var addr = ln.Addr().String()
    ln.Close()
    var slow = func(platform string, chat int64, name, text, lang string, body []byte) {
        close(started)
        time.Sleep(200 * time.Millisecond)
        var _, listErr = watches.List()
        finished <- listErr
    }
    if err := telegram.Start(telegram.Options{Token: "TESTTOKEN", Listen: addr, WebhookPath: "/bot", Message: slow}); err != nil {
        t.Fatalf("start: %v", err)
    }
    go http.Post("http://"+addr+"/bot", "application/json", strings.NewReader(`{"message":{"chat":{"id":1},"text":"/watches"}}`))
    <-started
    shutdown()
    if handlerErr := <-finished; handlerErr != nil {
        t.Fatalf("store was closed while a handler was still running: %v", handlerErr)
    }
    if _, err := watches.List(); err == nil {
        t.Fatalf("expected the store to be closed after shutdown")
    }
}

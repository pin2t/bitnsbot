package main

import "net"
import "net/http"
import "path/filepath"
import "testing"
import "time"

import "bitnsbot/watches"

// TestShutdownDrainsHandlersBeforeClosingStore verifies the ordering the
// feature exists for: the webhook server is stopped first and waits for
// in-flight handlers to finish, so a handler using the store never sees it
// closed out from under it — and once shutdown returns, the store is closed.
func TestShutdownDrainsHandlersBeforeClosingStore(t *testing.T) {
    var err error
    err = openDB(filepath.Join(t.TempDir(), "watches.db"))
    if err != nil {
        t.Fatalf("openDB: %v", err)
    }
    btcd = nil
    var started = make(chan struct{})
    var finished = make(chan error, 1)
    var mux = http.NewServeMux()
    mux.HandleFunc("/slow", func(w http.ResponseWriter, r *http.Request) {
        close(started)
        time.Sleep(200 * time.Millisecond)
        var _, listErr = watches.List() // the store must still be open while a handler runs
        finished <- listErr
        w.WriteHeader(http.StatusOK)
    })
    var ln, lnErr = net.Listen("tcp", "127.0.0.1:0")
    if lnErr != nil {
        t.Fatalf("listen: %v", lnErr)
    }
    var srv = &http.Server{Handler: mux}
    go srv.Serve(ln)
    go http.Get("http://" + ln.Addr().String() + "/slow")
    <-started
    shutdown(srv) // must wait for the in-flight handler, then close the store
    if handlerErr := <-finished; handlerErr != nil {
        t.Fatalf("store was closed while a handler was still running: %v", handlerErr)
    }
    if _, err := watches.List(); err == nil {
        t.Fatalf("expected the store to be closed after shutdown")
    }
}

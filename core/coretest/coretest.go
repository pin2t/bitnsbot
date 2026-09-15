// Package coretest stands in for Bitcoin Core in tests. Core speaks plain HTTP
// request/response with no websocket and no server-pushed notifications, so
// there is no upgrade handshake to fake and no connection to keep alive:
// notifications arrive over ZMQ, and tests drive those by calling the handlers
// directly.
package coretest

import "encoding/json"
import "net/http"
import "net/http/httptest"
import "testing"
import "bitnsbot/core"

// Responder answers one JSON-RPC call: the method and its positional params in,
// the result or the node's error out.
type Responder func(method string, params []interface{}) (interface{}, error)

// Server is a node answering through respond. It checks basic auth as
// testuser/testpass and reports a method error in the body with HTTP 500, the
// way Core does, so the client is held to reading the body either way.
func Server(t testing.TB, respond Responder) *httptest.Server {
    var server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
        if user, pass, ok := r.BasicAuth(); !ok || user != "testuser" || pass != "testpass" {
            http.Error(w, "unauthorized", http.StatusUnauthorized)
            return
        }
        var req struct {
            Method string        `json:"method"`
            Params []interface{} `json:"params"`
        }
        if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
            http.Error(w, "bad request", http.StatusBadRequest)
            return
        }
        var result, callErr = respond(req.Method, req.Params)
        var resp = map[string]any{"id": "bitnsbot"}
        if callErr != nil {
            resp["error"] = map[string]any{"code": -1, "message": callErr.Error()}
            w.WriteHeader(http.StatusInternalServerError)
        } else {
            resp["result"] = result
            resp["error"] = nil
        }
        json.NewEncoder(w).Encode(resp)
    }))
    t.Cleanup(server.Close)
    return server
}

// Use points the core package at server for the rest of the test, and leaves no
// node configured once it is over.
func Use(t testing.TB, server *httptest.Server) {
    t.Helper()
    if err := core.Init(server.URL, "testuser", "testpass", ""); err != nil { t.Fatalf("core: %v", err) }
    t.Cleanup(core.Reset)
}

// Start is Server and Use together, which is what nearly every test wants.
func Start(t testing.TB, respond Responder) *httptest.Server {
    t.Helper()
    var server = Server(t, respond)
    Use(t, server)
    return server
}

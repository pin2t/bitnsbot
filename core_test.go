package main

import "context"
import "errors"
import "encoding/json"
import "net/http"
import "net/http/httptest"
import "testing"

// newFakeCoreServer stands in for Bitcoin Core's JSON-RPC endpoint. It is far
// simpler than the btcd fake it replaces: Core speaks plain HTTP request/response
// with no websocket and no server-pushed notifications, so there is no upgrade
// handshake to fake and no connection to keep alive. Notifications arrive over
// ZMQ instead, and tests drive those by calling the handlers directly.
func newFakeCoreServer(t *testing.T, respond func(method string, params []interface{}) (interface{}, error)) *httptest.Server {
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
            // Core reports method errors in the body, with HTTP 500
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

func newFakeCoreConn(t *testing.T, server *httptest.Server) *coreConn {
    var client, err = newCoreConn(server.URL, "testuser", "testpass", "")
    if err != nil {
        t.Fatalf("newCoreClient: %v", err)
    }
    return client
}

func TestCoreGetBlockCount(t *testing.T) {
    var server = newFakeCoreServer(t, func(method string, params []interface{}) (interface{}, error) {
        if method != "getblockcount" {
            t.Fatalf("unexpected method: %s", method)
        }
        return 958955, nil
    })
    var client = newFakeCoreConn(t, server)
    var count, err = client.getBlockCount(context.Background())
    if err != nil {
        t.Fatalf("getBlockCount: %v", err)
    }
    if count != 958955 {
        t.Fatalf("count = %d, want 958955", count)
    }
}

// A method error comes back in the body with HTTP 500, not as a transport
// failure, so the client has to read the body either way.
func TestCoreMethodError(t *testing.T) {
    var server = newFakeCoreServer(t, func(method string, params []interface{}) (interface{}, error) {
        return nil, errors.New("Block not found")
    })
    var client = newFakeCoreConn(t, server)
    var _, err = client.getBlockHeader(context.Background(), "deadbeef")
    if err == nil {
        t.Fatal("expected an error for an unknown block hash")
    }
    if got := err.Error(); got == "" {
        t.Fatal("expected the node's message to survive")
    }
}

// Verbosity 2 gives prevouts and a fee for a confirmed transaction, which is what
// lets txInputs skip fetching prevouts entirely.
func TestCoreGetRawTransactionPrevouts(t *testing.T) {
    var server = newFakeCoreServer(t, func(method string, params []interface{}) (interface{}, error) {
        if method != "getrawtransaction" {
            t.Fatalf("unexpected method: %s", method)
        }
        if len(params) < 2 {
            t.Fatalf("expected a verbosity argument, got %v", params)
        }
        if v, _ := params[1].(float64); v != 2 {
            t.Fatalf("verbosity = %v, want 2 (fee and prevouts)", params[1])
        }
        return map[string]any{
            "txid": "abc", "size": 225, "vsize": 200, "confirmations": 6, "fee": 0.0001,
            "vin": []map[string]any{{
                "txid": "prev", "vout": 0,
                "prevout": map[string]any{"value": 1.5, "scriptPubKey": map[string]any{"address": "bc1qsender"}},
            }},
            "vout": []map[string]any{{"value": 1.4999, "n": 0, "scriptPubKey": map[string]any{"address": "bc1qdest", "hex": "0014aa"}}},
        }, nil
    })
    core = newFakeCoreConn(t, server)
    t.Cleanup(func() { core = nil })
    var tx, err = core.getRawTransaction(context.Background(), "abc")
    if err != nil {
        t.Fatalf("getRawTransaction: %v", err)
    }
    if tx.Vin[0].PrevOut == nil {
        t.Fatal("expected an inline prevout at verbosity 2")
    }
    var fee, addrs, spent, ok = txInputs(context.Background(), tx)
    if !ok {
        t.Fatal("txInputs failed on a transaction carrying its own prevouts")
    }
    if fee != 0.0001 {
        t.Fatalf("fee = %v, want the node's own 0.0001", fee)
    }
    if len(addrs) != 1 || addrs[0] != "bc1qsender" {
        t.Fatalf("input addresses = %v, want [bc1qsender]", addrs)
    }
    if spent["bc1qsender"] != 1.5 {
        t.Fatalf("spent = %v, want 1.5 from bc1qsender", spent)
    }
}

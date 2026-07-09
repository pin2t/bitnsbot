package main

import "context"
import "encoding/base64"
import "encoding/json"
import "net/http"
import "net/http/httptest"
import "strings"
import "sync"
import "testing"
import "time"

import "github.com/gorilla/websocket"
import "github.com/sourcegraph/jsonrpc2"

type recordingHandler struct {
    mu    sync.Mutex
    calls []*jsonrpc2.Request
}

func (h *recordingHandler) Handle(ctx context.Context, conn *jsonrpc2.Conn, req *jsonrpc2.Request) {
    h.mu.Lock()
    h.calls = append(h.calls, req)
    h.mu.Unlock()
}

func (h *recordingHandler) snapshot() []*jsonrpc2.Request {
    h.mu.Lock()
    defer h.mu.Unlock()
    var out = make([]*jsonrpc2.Request, len(h.calls))
    copy(out, h.calls)
    return out
}

func newFakeBtcdServer(t *testing.T, respond func(method string, params json.RawMessage) (interface{}, error)) *httptest.Server {
    var upgrader websocket.Upgrader
    var wantAuth = "Basic " + base64.StdEncoding.EncodeToString([]byte("testuser:testpass"))
    return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
        if r.Header.Get("Authorization") != wantAuth {
            http.Error(w, "unauthorized", http.StatusUnauthorized)
            return
        }
        var conn, err = upgrader.Upgrade(w, r, nil)
        if err != nil {
            return
        }
        defer conn.Close()
        for {
            var req struct {
                ID     json.RawMessage `json:"id"`
                Method string          `json:"method"`
                Params json.RawMessage `json:"params"`
            }
            if err := conn.ReadJSON(&req); err != nil {
                return
            }
            var result, callErr = respond(req.Method, req.Params)
            var resp = map[string]any{"jsonrpc": "2.0", "id": req.ID}
            if callErr != nil {
                resp["error"] = map[string]any{"code": -1, "message": callErr.Error()}
            } else {
                resp["result"] = result
            }
            conn.WriteJSON(resp)
            if req.Method == "notifyblocks" {
                conn.WriteJSON(map[string]any{
                    "jsonrpc": "2.0",
                    "method":  "blockconnected",
                    "params":  []interface{}{"0000000000000000abc", 100, 1700000000},
                })
            }
        }
    }))
}

func dialFakeBtcd(t *testing.T, server *httptest.Server, handler jsonrpc2.Handler) *btcdClient {
    var url = "ws://" + strings.TrimPrefix(server.URL, "http://") + "/ws"
    var client, err = dialBtcd(context.Background(), btcdConfig{url: url, user: "testuser", pass: "testpass"}, handler)
    if err != nil {
        t.Fatalf("dialBtcd: %v", err)
    }
    return client
}

func TestBtcdGetBlockCount(t *testing.T) {
    var server = newFakeBtcdServer(t, func(method string, params json.RawMessage) (interface{}, error) {
        if method != "getblockcount" {
            t.Fatalf("unexpected method: %s", method)
        }
        return 42, nil
    })
    defer server.Close()
    var client = dialFakeBtcd(t, server, &recordingHandler{})
    defer client.close()
    var count, err = client.getBlockCount(context.Background())
    if err != nil {
        t.Fatalf("getBlockCount: %v", err)
    }
    if count != 42 {
        t.Fatalf("expected 42, got %d", count)
    }
}

func TestBtcdGetRawTransaction(t *testing.T) {
    var gotParams json.RawMessage
    var server = newFakeBtcdServer(t, func(method string, params json.RawMessage) (interface{}, error) {
        if method != "getrawtransaction" {
            t.Fatalf("unexpected method: %s", method)
        }
        gotParams = params
        return map[string]any{
            "txid":          "abc123",
            "hash":          "abc123",
            "confirmations": 6,
            "blockhash":     "def456",
            "time":          1700000000,
        }, nil
    })
    defer server.Close()
    var client = dialFakeBtcd(t, server, &recordingHandler{})
    defer client.close()
    var tx, err = client.getRawTransaction(context.Background(), "abc123")
    if err != nil {
        t.Fatalf("getRawTransaction: %v", err)
    }
    if tx.Txid != "abc123" || tx.Confirmations != 6 || tx.BlockHash != "def456" {
        t.Fatalf("unexpected tx: %#v", tx)
    }
    var got []interface{}
    if err := json.Unmarshal(gotParams, &got); err != nil {
        t.Fatalf("unmarshal params: %v", err)
    }
    if len(got) != 2 || got[0] != "abc123" || got[1] != float64(1) {
        t.Fatalf("unexpected params: %#v", got)
    }
}

func TestBtcdLoadTxFilter(t *testing.T) {
    var gotParams json.RawMessage
    var server = newFakeBtcdServer(t, func(method string, params json.RawMessage) (interface{}, error) {
        if method != "loadtxfilter" {
            t.Fatalf("unexpected method: %s", method)
        }
        gotParams = params
        return nil, nil
    })
    defer server.Close()
    var client = dialFakeBtcd(t, server, &recordingHandler{})
    defer client.close()
    var err = client.loadTxFilter(context.Background(), true, []string{"1A1zP1eP5QGefi2DMPTfTL5SLmv7DivfNa"}, nil)
    if err != nil {
        t.Fatalf("loadTxFilter: %v", err)
    }
    var got []interface{}
    if err := json.Unmarshal(gotParams, &got); err != nil {
        t.Fatalf("unmarshal params: %v", err)
    }
    if len(got) != 3 || got[0] != true {
        t.Fatalf("unexpected params: %#v", got)
    }
    var addrs, _ = got[1].([]interface{})
    if len(addrs) != 1 || addrs[0] != "1A1zP1eP5QGefi2DMPTfTL5SLmv7DivfNa" {
        t.Fatalf("unexpected addresses param: %#v", got[1])
    }
    var outpoints, _ = got[2].([]interface{})
    if len(outpoints) != 0 {
        t.Fatalf("expected empty outpoints, got %#v", got[2])
    }
}

func TestBtcdNotifyBlocksDispatchesNotification(t *testing.T) {
    var server = newFakeBtcdServer(t, func(method string, params json.RawMessage) (interface{}, error) {
        return nil, nil
    })
    defer server.Close()
    var handler = &recordingHandler{}
    var client = dialFakeBtcd(t, server, handler)
    defer client.close()
    if err := client.notifyBlocks(context.Background()); err != nil {
        t.Fatalf("notifyBlocks: %v", err)
    }
    var deadline = time.Now().Add(2 * time.Second)
    for len(handler.snapshot()) == 0 && time.Now().Before(deadline) {
        time.Sleep(10 * time.Millisecond)
    }
    var calls = handler.snapshot()
    if len(calls) != 1 || calls[0].Method != "blockconnected" {
        t.Fatalf("expected one blockconnected notification, got: %#v", calls)
    }
}

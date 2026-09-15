package core_test

import "context"
import "errors"
import "testing"
import "bitnsbot/core"
import "bitnsbot/core/coretest"

func TestCoreGetBlockCount(t *testing.T) {
    coretest.Start(t, func(method string, params []interface{}) (interface{}, error) {
        if method != "getblockcount" {
            t.Fatalf("unexpected method: %s", method)
        }
        return 958955, nil
    })
    var count, err = core.GetBlockCount(context.Background())
    if err != nil {
        t.Fatalf("GetBlockCount: %v", err)
    }
    if count != 958955 {
        t.Fatalf("count = %d, want 958955", count)
    }
}

// A method error comes back in the body with HTTP 500, not as a transport
// failure, so the client has to read the body either way.
func TestCoreMethodError(t *testing.T) {
    coretest.Start(t, func(method string, params []interface{}) (interface{}, error) {
        return nil, errors.New("Block not found")
    })
    var _, err = core.GetBlockHeader(context.Background(), "deadbeef")
    if err == nil {
        t.Fatal("expected an error for an unknown block hash")
    }
    if got := err.Error(); got != "Block not found (code -1)" {
        t.Fatalf("error = %q, want the node's own message", got)
    }
}

// With no node configured every call fails rather than panicking, so a caller
// that forgot to check Enabled gets an error it can log.
func TestCoreUnconfigured(t *testing.T) {
    core.Reset()
    if core.Enabled() { t.Fatal("Enabled after Reset") }
    if _, err := core.GetBlockCount(context.Background()); err == nil {
        t.Fatal("a call with no node configured succeeded")
    }
    if _, err := core.GetBlockTxids(context.Background(), "deadbeef"); err == nil {
        t.Fatal("a cached call with no node configured succeeded")
    }
}

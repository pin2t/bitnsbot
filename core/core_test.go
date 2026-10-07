package core_test

import "context"
import "errors"
import "sync/atomic"
import "testing"
import "time"
import "bitnsbot/core"
import "bitnsbot/core/coretest"

func TestCoreGetBlockCount(t *testing.T) {
    coretest.Start(t, func(method string, params []any) (any, error) {
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
    coretest.Start(t, func(method string, params []any) (any, error) {
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

// pinging answers every ping through fail and anything else with a block
// count, counting the pings so a test can wait for them.
func pinging(t *testing.T, fail func(n int) bool) *atomic.Int64 {
    var pings atomic.Int64
    core.SetPing(10 * time.Millisecond)
    t.Cleanup(func() { core.SetPing(5 * time.Second) })
    coretest.Start(t, func(method string, params []any) (any, error) {
        if method != "ping" { return 1, nil }
        if fail(int(pings.Add(1))) { return nil, errors.New("node is down") }
        return nil, nil
    })
    return &pings
}

// waitFor polls cond for up to a second, the watchdog running at 10ms.
func waitFor(t *testing.T, what string, cond func() bool) {
    t.Helper()
    var deadline = time.Now().Add(time.Second)
    for !cond() {
        if time.Now().After(deadline) { t.Fatalf("timed out waiting for %s", what) }
        time.Sleep(5 * time.Millisecond)
    }
}

// Two failed pings in a row make the watchdog dial the node again, and the new
// connection replaces the old one once it answers a ping of its own.
func TestCoreReconnectsAfterTwoFailedPings(t *testing.T) {
    var pings = pinging(t, func(n int) bool { return n <= 2 })
    var before = core.Current()
    waitFor(t, "a reconnect", func() bool { return core.Current() != before })
    if n := pings.Load(); n < 3 {
        t.Errorf("swapped after %d pings; want the new connection's own ping to have answered", n)
    }
    if _, err := core.GetBlockCount(context.Background()); err != nil {
        t.Errorf("a call through the new connection failed: %v", err)
    }
}

// One failed ping is not enough to reconnect: a ping that answers in between
// starts the count again.
func TestCoreOneFailedPingKeepsTheConnection(t *testing.T) {
    var pings = pinging(t, func(n int) bool { return n%2 == 1 })
    var before = core.Current()
    waitFor(t, "several pings", func() bool { return pings.Load() >= 8 })
    if core.Current() != before {
        t.Error("reconnected though no two pings in a row failed")
    }
}

// A new connection whose own ping fails is thrown away, and the old one stays.
func TestCoreKeepsTheConnectionWhileTheNodeIsDown(t *testing.T) {
    var pings = pinging(t, func(n int) bool { return true })
    var before = core.Current()
    waitFor(t, "several reconnect attempts", func() bool { return pings.Load() >= 8 })
    if core.Current() != before {
        t.Error("swapped in a connection whose ping failed")
    }
}

// Reset stops the watchdog: no ping reaches the node once it has returned.
func TestCoreResetStopsTheWatchdog(t *testing.T) {
    var pings = pinging(t, func(n int) bool { return false })
    waitFor(t, "a ping", func() bool { return pings.Load() >= 1 })
    core.Reset()
    time.Sleep(20 * time.Millisecond)
    var after = pings.Load()
    time.Sleep(50 * time.Millisecond)
    if n := pings.Load(); n != after {
        t.Errorf("%d pings after Reset", n-after)
    }
}

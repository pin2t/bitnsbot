package app

import "strings"
import "sync"
import "testing"
import "time"

// A listener hears every event Notify announces, and however often the mempool
// changes, one mempool event per interval — the first at once, the rest folded
// into one when it is up. Both Mini Apps are listeners, which is what keeps
// their streams in step.
func TestListenHearsAnnouncedEvents(t *testing.T) {
    var saved = throttled["mempool"]
    throttled["mempool"] = 200 * time.Millisecond
    t.Cleanup(func() { throttled["mempool"] = saved })
    throttleMu.Lock()
    throttleLast, throttlePending = map[string]time.Time{}, map[string]bool{}
    throttleMu.Unlock()
    var mu sync.Mutex
    var heard []string
    var on = true
    t.Cleanup(func() { mu.Lock(); on = false; mu.Unlock() })
    Listen(func(event string) {
        mu.Lock()
        defer mu.Unlock()
        if on { heard = append(heard, event) }
    })
    Notify("fees")
    for i := 0; i < 10; i++ {
        Notify("mempool")
        time.Sleep(10 * time.Millisecond)
    }
    time.Sleep(350 * time.Millisecond)
    mu.Lock()
    defer mu.Unlock()
    if strings.Join(heard, " ") != "fees mempool mempool" {
        t.Errorf("heard %q, want fees and two mempool events", heard)
    }
}

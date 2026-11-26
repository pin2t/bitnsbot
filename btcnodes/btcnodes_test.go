package btcnodes

import "context"
import "net/http"
import "net/http/httptest"
import "sync"
import "testing"
import "time"

// fake points URL at a local server answering with the given status and body,
// restoring the real URL after. It returns a hit counter.
func fake(t *testing.T, status int, body string) func() int {
    var mu sync.Mutex
    var hits int
    var srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
        mu.Lock()
        hits++
        mu.Unlock()
        w.WriteHeader(status)
        w.Write([]byte(body))
    }))
    t.Cleanup(srv.Close)
    var saved = URL
    t.Cleanup(func() { URL = saved; Reset() })
    URL = srv.URL
    Reset()
    return func() int { mu.Lock(); defer mu.Unlock(); return hits }
}

// A successful fetch is the count, and is cached.
func TestCountFetches(t *testing.T) {
    var hits = fake(t, http.StatusOK, `{"count":26105,"results":[]}`)
    var n, ok = Count(context.Background())
    if !ok || n != 26105 {
        t.Fatalf("Count = %d, %v; want 26105, true", n, ok)
    }
    if count != 26105 || hits() != 1 {
        t.Errorf("cached %d after %d fetches, want 26105 after 1", count, hits())
    }
}

// A count younger than ttl is reused without asking the API again — that is
// what keeps it to four requests a day.
func TestCountReusesCached(t *testing.T) {
    var hits = fake(t, http.StatusOK, `{"count":99999}`)
    count, seen, next = 30000, time.Now(), time.Now().Add(time.Hour)
    var n, ok = Count(context.Background())
    if hits() != 0 {
        t.Fatalf("a fresh count was reused, yet btcnodes.io was fetched %d times", hits())
    }
    if !ok || n != 30000 {
        t.Errorf("Count = %d, %v; want the cached 30000, true", n, ok)
    }
}

// A fetch that fails while the cached count is still under maxAge keeps
// serving that count.
func TestCountKeepsCachedWhenYoung(t *testing.T) {
    var hits = fake(t, http.StatusTooManyRequests, `rate limited`)
    count, seen = 30000, time.Now().Add(-time.Hour)
    var n, ok = Count(context.Background())
    if hits() != 1 {
        t.Fatalf("a due fetch was not made; %d hits", hits())
    }
    if !ok || n != 30000 {
        t.Errorf("Count = %d, %v; want the cached 30000, true", n, ok)
    }
}

// Once the cached count is older than maxAge a failed fetch leaves nothing to
// trust.
func TestCountStaleIsNotTrusted(t *testing.T) {
    fake(t, http.StatusTooManyRequests, `rate limited`)
    count, seen = 30000, time.Now().Add(-3*24*time.Hour)
    if n, ok := Count(context.Background()); ok {
        t.Errorf("Count = %d, true; a three-day-old count must not be trusted", n)
    }
}

// With no count at all and the API failing, there is nothing to report, and the
// failure still spaces out the next try by ttl.
func TestCountFailsCold(t *testing.T) {
    var hits = fake(t, http.StatusOK, `{"count":0}`)
    if n, ok := Count(context.Background()); ok {
        t.Errorf("Count = %d, true; a zero count is not a count", n)
    }
    Count(context.Background())
    if hits() != 1 {
        t.Errorf("btcnodes.io fetched %d times, want 1 — a failure waits ttl too", hits())
    }
}

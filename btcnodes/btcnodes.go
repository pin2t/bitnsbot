// Package btcnodes counts the reachable Bitcoin nodes, from btcnodes.io's crawl.
//
// That crawl probes every known node and counts the ones that answer, so it is
// the network-wide "active nodes" figure — smaller and truer than a node's own
// address manager, which counts every gossiped address whether or not it is
// reachable. The count is cached: the API is asked at most once every ttl, and a
// count stays trusted for maxAge, so a failed or throttled fetch keeps serving
// the last one until it is too old.
package btcnodes

import "context"
import "encoding/json"
import "fmt"
import "net/http"
import "time"
import "bitnsbot/logging"

// URL is BTC Nodes' list of reachable nodes. One page of one row is enough: the
// response carries the total in its count field. A var so tests point it at a
// local server.
var URL = "https://btcnodes.io/api/v1/nodes/?limit=1"

// client fetches the count with its own timeout, like the rate and market
// clients, so a slow API cannot stall a refresh.
var client = &http.Client{Timeout: 10 * time.Second}

// ttl is how long a successful count is reused before the API is asked again.
// Six hours is four requests a day, inside its daily limit; the same interval
// also spaces out retries after a failure, so a down or throttled API is not
// hammered between ticks.
var ttl = 6 * time.Hour

// maxAge is how old the cached count may get before Count stops trusting it.
var maxAge = 48 * time.Hour

// The crawl, cached. count is the last successful count (0 means none yet);
// seen is when that count was fetched; next is the earliest time the API may be
// asked again. All are touched only by the network-stats ticker, so they need
// no lock.
var count int64
var seen time.Time
var next time.Time

// Reset forgets the cached count, so a test starts cold.
func Reset() {
    count = 0
    seen = time.Time{}
    next = time.Time{}
}

// Count returns the number of reachable nodes and whether there is one to
// trust. It asks the API when the last request is at least ttl old, and
// otherwise — or when that request fails — serves the cached count while it is
// no older than maxAge. Reporting false is the caller's cue to count some other
// way.
func Count(ctx context.Context) (int64, bool) {
    if !time.Now().Before(next) {
        next = time.Now().Add(ttl)
        var n, err = active(ctx)
        if err == nil {
            count, seen = n, time.Now()
            return n, true
        }
        logging.Warn("network stats: btcnodes.io: %v", err)
    }
    if count > 0 && time.Since(seen) <= maxAge { return count, true }
    return 0, false
}

// active fetches the count from btcnodes.io. It reports an error when the API
// is unreachable, throttled (429), or the response cannot be parsed.
func active(ctx context.Context) (int64, error) {
    logging.Net("network: GET %s", URL)
    var req, err = http.NewRequestWithContext(ctx, http.MethodGet, URL, nil)
    if err != nil { return 0, err }
    var resp, derr = client.Do(req)
    if derr != nil { return 0, derr }
    defer resp.Body.Close()
    if resp.StatusCode != http.StatusOK {
        return 0, fmt.Errorf("status %d", resp.StatusCode)
    }
    var v struct {
        Count int64 `json:"count"`
    }
    if err := json.NewDecoder(resp.Body).Decode(&v); err != nil { return 0, err }
    if v.Count <= 0 { return 0, fmt.Errorf("non-positive count %d", v.Count) }
    return v.Count, nil
}

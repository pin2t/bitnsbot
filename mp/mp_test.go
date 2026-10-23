package mp

import "math"
import "testing"
import "time"

// A coinbase starts a block's republished transactions: they come off the list
// rather than going on it, and an open page is told which rows to drop. Once the
// burst is over an arrival is listed again.
func TestListDropsMinedTransactions(t *testing.T) {
    Reset()
    t.Cleanup(Reset)
    Record("first", 1000, 100, false)
    Record("second", 2000, 100, false)
    Record("coinbase", 5000000000, 100, true)
    Record("first", 1000, 100, false)
    Record("unlisted", 1000, 100, false)
    var top, txs, mined, _ = List(0)
    if len(txs) != 1 || txs[0].Txid != "second" {
        t.Fatalf("list = %+v, want only the unmined arrival", txs)
    }
    if len(mined) != 1 || mined[0] != "first" {
        t.Errorf("mined = %v, want first", mined)
    }
    if _, _, mined, _ = List(top); len(mined) != 0 {
        t.Errorf("mined after the page caught up = %v", mined)
    }
    mu.Lock()
    minedUntil = time.Time{}
    mu.Unlock()
    Record("third", 1000, 100, false)
    if _, txs, _, _ = List(0); txs[0].Txid != "third" {
        t.Errorf("an arrival after the block's burst is not listed")
    }
}

// Every frame moves the counts and the total: up for an arrival, down for a
// transaction a block mined, not at all for its coinbase.
func TestRecordMovesTheCounts(t *testing.T) {
    Reset()
    t.Cleanup(Reset)
    SetSummary(1000, 50, true)
    Record("a", 300, 141, false)
    if n, size, _ := Stats(); n != 1 || size != 141 {
        t.Errorf("after an arrival: %d txs, %d vB", n, size)
    }
    if amount, fee, ok := Summary(); amount != 1300 || fee != 50 || !ok {
        t.Errorf("summary = %d, %d, %v", amount, fee, ok)
    }
    Record("cb", 5000, 200, true)
    Record("a", 300, 141, false)
    if n, size, _ := Stats(); n != 0 || size != 0 {
        t.Errorf("after the block: %d txs, %d vB", n, size)
    }
    if amount, _, _ := Summary(); amount != 1000 {
        t.Errorf("amount after the block = %d", amount)
    }
}

// The flow rate is counted from the arrivals: those of the last window per
// second, against the window before. It is not shown until a whole window has
// been watched, nor its change until two have; arrivals older than two windows
// count for nothing, and a rate falls to zero once they stop. An arrival
// forgets the ones past two windows.
func TestFlowRate(t *testing.T) {
    Reset()
    t.Cleanup(Reset)
    var now = time.Now()
    var seed = func(n int, age time.Duration) {
        for i := 0; i < n; i++ { arrivals = append(arrivals, now.Add(-age)) }
    }
    mu.Lock()
    flowSince = now.Add(-30 * time.Second)
    seed(40, 20*time.Second)
    mu.Unlock()
    if _, rateOK, _, _ := Flow(now); rateOK {
        t.Fatal("a rate after half a window")
    }
    mu.Lock()
    flowSince, arrivals = now.Add(-3*time.Minute), nil
    seed(500, 150*time.Second)
    seed(120, 90*time.Second)
    seed(150, 10*time.Second)
    mu.Unlock()
    var rate, rateOK, change, changeOK = Flow(now)
    if !rateOK || !changeOK || rate != 2.5 || math.Abs(change-0.5) > 1e-9 {
        t.Fatalf("rate=%v (%v) change=%v (%v), want 2.5 and +0.5", rate, rateOK, change, changeOK)
    }
    mu.Lock()
    flowSince = now.Add(-90 * time.Second)
    mu.Unlock()
    if _, rateOK, _, changeOK = Flow(now); !rateOK || changeOK {
        t.Errorf("after a window and a half: rateOK=%v changeOK=%v, want true, false", rateOK, changeOK)
    }
    if rate, _, _, _ = Flow(now.Add(3 * time.Minute)); rate != 0 {
        t.Errorf("rate %v once arrivals stopped, want 0", rate)
    }
    Record("new", 1000, 100, false)
    mu.Lock()
    var kept = len(arrivals)
    mu.Unlock()
    if kept != 271 {
        t.Errorf("%d arrivals kept, want the 270 inside two windows and the new one", kept)
    }
}

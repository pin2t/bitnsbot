package miners

import "context"
import "encoding/json"
import "errors"
import "math"
import "reflect"
import "testing"
import "go.etcd.io/bbolt"
import "time"

// fakeSource stands in for the btcd-backed chain source: a fixed tip and a map of
// blocks, recording every height fetched so tests can assert what was processed.
type fakeSource struct {
    tip     int64
    blocks  map[int64]Block
    fetched []int64
    onBlock func(h int64)
    err     map[int64]bool
}

func (f *fakeSource) Tip(ctx context.Context) (int64, error) { return f.tip, nil }

func (f *fakeSource) Block(ctx context.Context, height int64) (Block, error) {
    f.fetched = append(f.fetched, height)
    if f.onBlock != nil { f.onBlock(height) }
    if f.err[height] { return Block{}, errors.New("block unavailable") }
    return f.blocks[height], nil
}

func seedAddresses(t *testing.T, addrs map[string]string) {
    var err = db.Update(func(tx *bbolt.Tx) error {
        var b = tx.Bucket(bucket)
        for a, n := range addrs {
            if err := b.Put([]byte(a), []byte(n)); err != nil { return err }
        }
        return nil
    })
    if err != nil { t.Fatalf("seed addresses: %v", err) }
}

func statOf(t *testing.T, name string) stat {
    var s stat
    db.View(func(tx *bbolt.Tx) error {
        if v := tx.Bucket(statBucket).Get([]byte(name)); v != nil {
            if err := json.Unmarshal(v, &s); err != nil { t.Fatalf("unmarshal %s: %v", name, err) }
        }
        return nil
    })
    return s
}

func setChunk(t *testing.T, chunk int64) {
    var sc = chunkSize
    t.Cleanup(func() { chunkSize = sc })
    chunkSize = chunk
}

func setCooldown(t *testing.T, p time.Duration) {
    var cd = cooldownPeriod
    t.Cleanup(func() { cooldownPeriod = cd })
    cooldownPeriod = p
}

// Satoshi amounts are exact integers, so they compare exactly — the float
// tolerance below exists for Work and ConsumptionGW, which genuinely are floats.
func equalSat(t *testing.T, label string, got, want int64) {
    if got != want {
        t.Fatalf("%s = %d, want %d", label, got, want)
    }
}

func equal(t *testing.T, label string, got, want float64) {
    if math.Abs(got-want) > 1e-6*math.Max(1, math.Abs(want)) {
        t.Fatalf("%s = %g, want %g", label, got, want)
    }
}

// blocks 1..5: PoolA mines 1, 3 (via a *second* address, which must combine into
// the same name) and 5; PoolB mines 2; block 4 is an unknown miner and is skipped.
// Block 3's payout is not the first coinbase output — real coinbases put witness
// commitments and other outputs alongside it — so every address must be checked.
// Block 5 is at a higher difficulty — a retarget — so LastWork differs from the
// per-block average and the consumption estimate can be told apart from one built
// on accumulated work.
func chainFixture() *fakeSource {
    var lo, hi = 1.0e14, 1.4e14
    return &fakeSource{
        tip: 5,
        blocks: map[int64]Block{
            1: {CoinbaseAddresses: []string{"aA"}, Reward: 650000000, Fees: 25000000, Difficulty: lo},
            2: {CoinbaseAddresses: []string{"aB"}, Reward: 640000000, Fees: 15000000, Difficulty: lo},
            3: {CoinbaseAddresses: []string{"unrelated", "aA2"}, Reward: 630000000, Fees: 5000000, Difficulty: lo},
            4: {CoinbaseAddresses: []string{"nobody"}, Reward: 620000000, Fees: 10000000, Difficulty: lo},
            5: {CoinbaseAddresses: []string{"aA"}, Reward: 660000000, Fees: 30000000, Difficulty: hi},
        },
    }
}

func fixtureDB(t *testing.T) {
    openTestDB(t)
    seedAddresses(t, map[string]string{"aA": "PoolA", "aA2": "PoolA", "aB": "PoolB"})
}

func TestCollectStats(t *testing.T) {
    fixtureDB(t)
    setChunk(t, 1000)
    var src = chainFixture()
    collect(src)
    var a = statOf(t, "PoolA")
    if a.Blocks != 3 { t.Fatalf("PoolA blocks = %d, want 3", a.Blocks) }
    equalSat(t, "PoolA reward", a.Reward, 650000000+630000000+660000000)
    equalSat(t, "PoolA fees", a.Fees, 25000000+5000000+30000000)
    equal(t, "PoolA work", a.Work, (1.0e14+1.0e14+1.4e14)*workPerDifficulty)
    equal(t, "PoolA last work", a.LastWork, 1.4e14*workPerDifficulty)
    var b = statOf(t, "PoolB")
    if b.Blocks != 1 { t.Fatalf("PoolB blocks = %d, want 1", b.Blocks) }
    equalSat(t, "PoolB reward", b.Reward, 640000000)
    equal(t, "PoolB last work", b.LastWork, 1.0e14*workPerDifficulty)
    // the unknown miner's block is not attributed to anyone
    if s := statOf(t, "Unknown"); s.Blocks != 0 { t.Fatalf("unknown miner was stored: %+v", s) }
    var last, ok = cursor()
    if !ok || last != 5 { t.Fatalf("cursor = (%d, %v), want (5, true)", last, ok) }
}

func TestTopConsumption(t *testing.T) {
    fixtureDB(t)
    setChunk(t, 1000)
    collect(chainFixture())
    var top = Top(10)
    if len(top) != 2 { t.Fatalf("top = %d entries, want 2", len(top)) }
    if top[0].Name != "PoolA" || top[1].Name != "PoolB" {
        t.Fatalf("top order = %q, %q; want PoolA, PoolB", top[0].Name, top[1].Name)
    }
    if top[0].Blocks != 3 { t.Fatalf("PoolA blocks = %d, want 3", top[0].Blocks) }
    // share of the 4 attributed blocks × the *current* network hashrate (last
    // block's difficulty × 2^32 ÷ 600s) × 1e-11 J/hash, in GW. Using accumulated
    // work instead would give 4.87 GW for PoolA, so this pins the LastWork formula.
    equal(t, "PoolA GW", top[0].ConsumptionGW, (3.0/4.0)*(1.4e14*workPerDifficulty/secondsPerBlock)*joulesPerHash/1e9)
    equal(t, "PoolB GW", top[1].ConsumptionGW, (1.0/4.0)*(1.0e14*workPerDifficulty/secondsPerBlock)*joulesPerHash/1e9)
    if top[0].ConsumptionGW < 7.4 || top[0].ConsumptionGW > 7.6 {
        t.Fatalf("PoolA GW = %g, want ≈7.51 (a 75%% share of a ~1000 EH/s network)", top[0].ConsumptionGW)
    }
    if got := Top(1); len(got) != 1 || got[0].Name != "PoolA" {
        t.Fatalf("Top(1) = %+v, want just PoolA", got)
    }
}

// A gap larger than chunkSize is processed in chunks, each flushed to the
// database before the next starts — the source's hook asserts that block 1's
// stats are already persisted by the time the second chunk begins.
func TestCollectChunks(t *testing.T) {
    fixtureDB(t)
    setChunk(t, 2)
    setCooldown(t, time.Millisecond)
    var src = chainFixture()
    var flushed bool
    src.onBlock = func(h int64) {
        if h != 3 { return }
        flushed = statOf(t, "PoolA").Blocks == 1
    }
    collect(src)
    if !flushed { t.Fatal("first chunk was not flushed before the second was processed") }
    if !reflect.DeepEqual(src.fetched, []int64{1, 2, 3, 4, 5}) {
        t.Fatalf("fetched %v, want 1..5 in order", src.fetched)
    }
    if a := statOf(t, "PoolA"); a.Blocks != 3 { t.Fatalf("PoolA blocks = %d, want 3", a.Blocks) }
    var last, _ = cursor()
    if last != 5 { t.Fatalf("cursor last = %d, want 5", last) }
}

// A second run resumes at the cursor: only the new blocks are fetched, and their
// stats add to what is already stored.
func TestCollectResumes(t *testing.T) {
    fixtureDB(t)
    setChunk(t, 1000)
    var src = chainFixture()
    collect(src)
    src.fetched = nil
    src.tip = 7
    src.blocks[6] = Block{CoinbaseAddresses: []string{"aB"}, Reward: 610000000, Fees: 20000000, Difficulty: 1.4e14}
    src.blocks[7] = Block{CoinbaseAddresses: []string{"aB"}, Reward: 620000000, Fees: 10000000, Difficulty: 1.4e14}
    collect(src)
    if !reflect.DeepEqual(src.fetched, []int64{6, 7}) {
        t.Fatalf("second run fetched %v, want only 6 and 7", src.fetched)
    }
    var b = statOf(t, "PoolB")
    if b.Blocks != 3 { t.Fatalf("PoolB blocks = %d, want 3", b.Blocks) }
    equalSat(t, "PoolB reward", b.Reward, 640000000+610000000+620000000)
    equalSat(t, "PoolB fees", b.Fees, 15000000+20000000+10000000)
    var last, _ = cursor()
    if last != 7 { t.Fatalf("cursor = %d, want 7", last) }
    // the window is over the total attributed blocks now (pool A 3 + pool B 3 = 6)
    var top = Top(10)
    var pb Stat
    for _, s := range top {
        if s.Name == "PoolB" { pb = s }
    }
    if pb.Blocks != 3 { t.Fatalf("PoolB in top = %+v, want 3 blocks", pb) }
    equal(t, "PoolB GW", pb.ConsumptionGW, (3.0/6.0)*(1.4e14*workPerDifficulty/secondsPerBlock)*joulesPerHash/1e9)
    // equal block counts tie-break by name, so the list is stable across calls
    if top[0].Name != "PoolA" || top[1].Name != "PoolB" {
        t.Fatalf("tied order = %q, %q; want PoolA, PoolB", top[0].Name, top[1].Name)
    }
}

// Until the pool address list has been fetched nothing can be attributed, so the
// collector stays put rather than burning its initial window on "unknown".
func TestCollectWaitsForAddresses(t *testing.T) {
    openTestDB(t)
    setChunk(t, 1000)
    var src = chainFixture()
    collect(src)
    if len(src.fetched) != 0 { t.Fatalf("fetched %v with no pool addresses loaded", src.fetched) }
    if _, ok := cursor(); ok { t.Fatal("cursor was stored with no pool addresses loaded") }
}

func TestTopEmpty(t *testing.T) {
    fixtureDB(t)
    if got := Top(10); len(got) != 0 { t.Fatalf("Top on an empty bucket = %+v, want none", got) }
}

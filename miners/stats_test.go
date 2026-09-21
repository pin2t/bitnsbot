package miners

import "errors"
import "fmt"
import "math"
import "reflect"
import "strconv"
import "strings"
import "testing"
import "time"
import "bitnsbot/core/coretest"

// mined is what one block's coinbase says: every address it pays, the fees on
// top of the subsidy, and the difficulty the block was mined at.
type mined struct {
    addresses  []string
    fees       int64
    difficulty float64
}

// subsidy is the block reward at every height these tests use.
const subsidy = 5000000000

// chain is a node holding blocks, which the collector reads over RPC the way it
// reads a real one. Every height whose hash is asked for is recorded in fetched,
// after onBlock has run, and a height in err answers with the node's error.
type chain struct {
    tip     int64
    blocks  map[int64]mined
    fetched []int64
    onBlock func(h int64)
    err     map[int64]bool
}

// node points core at the chain for the rest of the test.
func node(t *testing.T, c *chain) *chain {
    coretest.Start(t, c.respond)
    return c
}

// respond names each block by its height and its coinbase by the block, and pays
// the coinbase's whole output to its last address: the payout is not the first
// output in a real coinbase either, so attribution has to look at them all.
func (c *chain) respond(method string, params []interface{}) (interface{}, error) {
    switch method {
    case "getblockcount":
        return c.tip, nil
    case "getblockhash":
        var h = int64(params[0].(float64))
        if c.onBlock != nil { c.onBlock(h) }
        c.fetched = append(c.fetched, h)
        if c.err[h] { return nil, errors.New("block unavailable") }
        return fmt.Sprint(h), nil
    case "getblock":
        var h, _ = strconv.ParseInt(params[0].(string), 10, 64)
        return map[string]interface{}{"height": h, "difficulty": c.blocks[h].difficulty, "tx": []string{"coinbase" + params[0].(string)}}, nil
    case "getrawtransaction":
        var h, _ = strconv.ParseInt(strings.TrimPrefix(params[0].(string), "coinbase"), 10, 64)
        var b = c.blocks[h]
        var vout = []interface{}{}
        for i, a := range b.addresses {
            var value float64
            if i == len(b.addresses)-1 { value = float64(subsidy+b.fees) / 1e8 }
            vout = append(vout, map[string]interface{}{"value": value, "scriptPubKey": map[string]string{"address": a}})
        }
        return map[string]interface{}{"txid": params[0], "vin": []interface{}{map[string]string{"coinbase": ""}}, "vout": vout}, nil
    }
    return nil, fmt.Errorf("unexpected call %s %v", method, params)
}

// seedAddresses puts the addresses into their pools' rows the way the definitions
// refresh would, and rebuilds the in-memory mappings attribution reads.
func seedAddresses(t *testing.T, addrs map[string]string) {
    t.Helper()
    var byPool = map[string][]string{}
    for a, n := range addrs { byPool[n] = append(byPool[n], a) }
    var pools []poolDef
    for name, list := range byPool { pools = append(pools, poolDef{Name: name, Addresses: list}) }
    if _, _, err := store(pools); err != nil { t.Fatalf("seed addresses: %v", err) }
}

// statOf reads one pool's aggregate, which every row of that pool repeats.
//
// an aggregate over no rows comes back as NULL, which is a pool with
// nothing gathered rather than an error
func statOf(t *testing.T, name string) record {
    t.Helper()
    var s record
    var err = db.QueryRow(`select max(blocks), max(reward), max(fees), max(totalWork), max(lastWork)
        from miners where name = ?`, name).Scan(&s.Blocks, &s.Reward, &s.Fees, &s.Work, &s.LastWork)
    if err != nil && err.Error() != "sql: no rows in result set" {
        var n int
        if db.QueryRow("select count(*) from miners where name = ?", name).Scan(&n) == nil && n == 0 {
            return record{}
        }
        t.Fatalf("stat of %s: %v", name, err)
    }
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
func chainFixture() *chain {
    var lo, hi = 1.0e14, 1.4e14
    return &chain{
        tip: 5,
        blocks: map[int64]mined{
            1: {addresses: []string{"aA"}, fees: 25000000, difficulty: lo},
            2: {addresses: []string{"aB"}, fees: 15000000, difficulty: lo},
            3: {addresses: []string{"unrelated", "aA2"}, fees: 5000000, difficulty: lo},
            4: {addresses: []string{"nobody"}, fees: 10000000, difficulty: lo},
            5: {addresses: []string{"aA"}, fees: 30000000, difficulty: hi},
        },
    }
}

func fixtureDB(t *testing.T) {
    openTestDB(t)
    seedAddresses(t, map[string]string{"aA": "PoolA", "aA2": "PoolA", "aB": "PoolB"})
}

// the unknown miner's block is not attributed to anyone
func TestCollectStats(t *testing.T) {
    fixtureDB(t)
    setChunk(t, 1000)
    var src = node(t, chainFixture())
    Update(src.tip)
    var a = statOf(t, "PoolA")
    if a.Blocks != 3 { t.Fatalf("PoolA blocks = %d, want 3", a.Blocks) }
    equalSat(t, "PoolA reward", a.Reward, 3*subsidy+25000000+5000000+30000000)
    equalSat(t, "PoolA fees", a.Fees, 25000000+5000000+30000000)
    equal(t, "PoolA work", a.Work, (1.0e14+1.0e14+1.4e14)*workPerDifficulty)
    equal(t, "PoolA last work", a.LastWork, 1.4e14*workPerDifficulty)
    var b = statOf(t, "PoolB")
    if b.Blocks != 1 { t.Fatalf("PoolB blocks = %d, want 1", b.Blocks) }
    equalSat(t, "PoolB reward", b.Reward, subsidy+15000000)
    equal(t, "PoolB last work", b.LastWork, 1.0e14*workPerDifficulty)
    if s := statOf(t, "Unknown"); s.Blocks != 0 { t.Fatalf("unknown miner was stored: %+v", s) }
    var last, ok = cursor()
    if !ok || last != 5 { t.Fatalf("cursor = (%d, %v), want (5, true)", last, ok) }
}

// share of the 4 attributed blocks × the *current* network hashrate (last
// block's difficulty × 2^32 ÷ 600s) × 1e-11 J/hash, in GW. Using accumulated
// work instead would give 4.87 GW for PoolA, so this pins the LastWork formula.
func TestTopConsumption(t *testing.T) {
    fixtureDB(t)
    setChunk(t, 1000)
    var src = node(t, chainFixture())
    Update(src.tip)
    var top = Top(10)
    if len(top) != 2 { t.Fatalf("top = %d entries, want 2", len(top)) }
    if top[0].Name != "PoolA" || top[1].Name != "PoolB" {
        t.Fatalf("top order = %q, %q; want PoolA, PoolB", top[0].Name, top[1].Name)
    }
    if top[0].Blocks != 3 { t.Fatalf("PoolA blocks = %d, want 3", top[0].Blocks) }
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
    var src = node(t, chainFixture())
    var flushed bool
    src.onBlock = func(h int64) {
        if h != 3 { return }
        flushed = statOf(t, "PoolA").Blocks == 1
    }
    Update(src.tip)
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
//
// the window is over the total attributed blocks now (pool A 3 + pool B 3 = 6)
//
// equal block counts tie-break by name, so the list is stable across calls
func TestCollectResumes(t *testing.T) {
    fixtureDB(t)
    setChunk(t, 1000)
    var src = node(t, chainFixture())
    Update(src.tip)
    src.fetched = nil
    src.tip = 7
    src.blocks[6] = mined{addresses: []string{"aB"}, fees: 20000000, difficulty: 1.4e14}
    src.blocks[7] = mined{addresses: []string{"aB"}, fees: 10000000, difficulty: 1.4e14}
    Update(src.tip)
    if !reflect.DeepEqual(src.fetched, []int64{6, 7}) {
        t.Fatalf("second run fetched %v, want only 6 and 7", src.fetched)
    }
    var b = statOf(t, "PoolB")
    if b.Blocks != 3 { t.Fatalf("PoolB blocks = %d, want 3", b.Blocks) }
    equalSat(t, "PoolB reward", b.Reward, 3*subsidy+15000000+20000000+10000000)
    equalSat(t, "PoolB fees", b.Fees, 15000000+20000000+10000000)
    var last, _ = cursor()
    if last != 7 { t.Fatalf("cursor = %d, want 7", last) }
    var top = Top(10)
    var pb Stat
    for _, s := range top {
        if s.Name == "PoolB" { pb = s }
    }
    if pb.Blocks != 3 { t.Fatalf("PoolB in top = %+v, want 3 blocks", pb) }
    equal(t, "PoolB GW", pb.ConsumptionGW, (3.0/6.0)*(1.4e14*workPerDifficulty/secondsPerBlock)*joulesPerHash/1e9)
    if top[0].Name != "PoolA" || top[1].Name != "PoolB" {
        t.Fatalf("tied order = %q, %q; want PoolA, PoolB", top[0].Name, top[1].Name)
    }
}

// Until the pool address list has been fetched nothing can be attributed, so the
// collector stays put rather than burning its initial window on "unknown".
func TestCollectWaitsForAddresses(t *testing.T) {
    openTestDB(t)
    setChunk(t, 1000)
    var src = node(t, chainFixture())
    Update(src.tip)
    if len(src.fetched) != 0 { t.Fatalf("fetched %v with no pool addresses loaded", src.fetched) }
    if _, ok := cursor(); ok { t.Fatal("cursor was stored with no pool addresses loaded") }
}

// The pool definitions live in the same records as the statistics, so a pool
// that has mined nothing this counted must not be reported as a miner with zero
// blocks — fixtureDB defines two pools and neither has mined.
func TestTopEmpty(t *testing.T) {
    fixtureDB(t)
    if got := Top(10); len(got) != 0 { t.Fatalf("Top on an empty bucket = %+v, want none", got) }
}

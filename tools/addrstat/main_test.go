package main

import "bytes"
import "encoding/hex"
import "fmt"
import "path/filepath"
import "strconv"
import "strings"
import "sync"
import "testing"
import "time"
import "bitnsbot/addrindex"
import "bitnsbot/addrstat"
import "bitnsbot/core"
import "bitnsbot/core/coretest"
import "bitnsbot/cursors"

var scriptA = append([]byte{0x00, 20}, bytes.Repeat([]byte{0xa1}, 20)...)
var scriptB = append([]byte{0x76, 0xa9, 20}, append(bytes.Repeat([]byte{0xb2}, 20), 0x88, 0xac)...)

var addrA = addrindex.Address(scriptA)
var addrB = addrindex.Address(scriptB)

// chain is a node of hand-built blocks answering the calls a build makes, as
// getblock verbosity 3 reports them: an input per spent prevout, none for a
// coinbase.
type chain struct {
    tip    int
    blocks map[int]core.Block
    fails  map[int]int
    asked  map[int]int
    mu     sync.Mutex
}

func (c *chain) respond(method string, params []any) (any, error) {
    switch method {
    case "getblockcount":
        return c.tip, nil
    case "getblockhash":
        return fmt.Sprint(params[0]), nil
    case "getblock":
        var height, _ = strconv.Atoi(params[0].(string))
        c.mu.Lock()
        if c.asked == nil { c.asked = map[int]int{} }
        c.asked[height]++
        var failing = c.asked[height] <= c.fails[height]
        c.mu.Unlock()
        if failing { return nil, fmt.Errorf("block %d unavailable", height) }
        var amount = func(p core.Payment) map[string]any {
            return map[string]any{"value": float64(p.Sat) / 1e8, "scriptPubKey": map[string]string{"hex": hex.EncodeToString(p.Script)}}
        }
        var txs = []any{}
        for _, tx := range c.blocks[height].Txs {
            var vin = []any{}
            var vout = []any{}
            for _, p := range tx.Spent { vin = append(vin, map[string]any{"prevout": amount(p)}) }
            for _, p := range tx.Outputs { vout = append(vout, amount(p)) }
            txs = append(txs, map[string]any{"vin": vin, "vout": vout})
        }
        return map[string]any{"time": c.blocks[height].Time, "tx": txs}, nil
    }
    return nil, fmt.Errorf("unexpected call %s %v", method, params)
}

// fixture pays addrA 50 BTC in block 1, then spends it in block 2: 30 BTC to
// addrB and 19.9999 back to addrA, leaving 10 000 sat in fees. The tip is block
// 7, so a build stops at block 2, five below it.
func fixture() *chain {
    var pay = func(script []byte, sat int64) core.Payment { return core.Payment{Script: script, Sat: sat} }
    return &chain{tip: 7, blocks: map[int]core.Block{
        0: {Time: 1231006505},
        1: {Time: 1231469665, Txs: []core.Tx{{Outputs: []core.Payment{pay(scriptA, 5_000_000_000)}}}},
        2: {Time: 1231470000, Txs: []core.Tx{{
            Spent:   []core.Payment{pay(scriptA, 5_000_000_000)},
            Outputs: []core.Payment{pay(scriptB, 3_000_000_000), pay(scriptA, 1_999_990_000)},
        }}},
    }}
}

// noWait makes a retry immediate for the rest of the test.
func noWait(t *testing.T) {
    var saved = retryDelay
    retryDelay = time.Millisecond
    t.Cleanup(func() { retryDelay = saved })
}

// setup serves c, opens a fresh database through the tool's own openDB and
// returns options pointing at both, the way the command line would.
func setup(t *testing.T, c *chain) *options {
    t.Helper()
    var server = coretest.Server(t, c.respond)
    t.Cleanup(core.Reset)
    var path = filepath.Join(t.TempDir(), "stat.db")
    var handle, err = openDB(path, false)
    if err != nil { t.Fatalf("open: %v", err) }
    t.Cleanup(func() { handle.Close() })
    return &options{db: path, url: server.URL, user: "testuser", pass: "testpass"}
}

func TestBuildThenQuery(t *testing.T) {
    var c = fixture()
    var opt = setup(t, c)
    var out bytes.Buffer
    if err := build(opt, &out); err != nil { t.Fatalf("build: %v", err) }
    if !strings.Contains(out.String(), "Collected 3 blocks up to 2") { t.Errorf("build said %q", out.String()) }
    out.Reset()
    var found, err = query(opt, addrA, &out)
    if err != nil || !found { t.Fatalf("query: found %v, %v", found, err) }
    for _, want := range []string{
        "Balance:       1 999 990 000 sat",
        "Received:      6 999 990 000 sat",
        "Sent:          5 000 000 000 sat",
        "Fees:          10 000 sat",
        "Transactions:  2",
        "First tx:      9 jan 2009",
        "As of block:   2",
    } {
        if !strings.Contains(out.String(), want) { t.Errorf("query output lacks %q:\n%s", want, out.String()) }
    }
    if !strings.Contains(out.String(), "5 blocks ahead") { t.Errorf("the answer does not say it stands five behind:\n%s", out.String()) }
}

// A block that fails a few times is retried and counted; one that fails every
// try is reported, skipped and stepped over, and the build reaches the tip less
// five regardless.
func TestBuildRetriesAndSkips(t *testing.T) {
    noWait(t)
    var c = fixture()
    c.fails = map[int]int{1: attempts, 2: attempts - 1}
    var opt = setup(t, c)
    var out bytes.Buffer
    if err := build(opt, &out); err != nil { t.Fatalf("build: %v", err) }
    if !strings.Contains(out.String(), "Skipping block 1:") || !strings.Contains(out.String(), "Skipped 1 blocks: [1]") {
        t.Errorf("build said %q", out.String())
    }
    if c.asked[1] != attempts || c.asked[2] != attempts { t.Errorf("asked %v, want %d tries at blocks 1 and 2", c.asked, attempts) }
    if h, ok := cursors.Get(cursors.AddrStat); !ok || h != 2 { t.Errorf("cursor = %d ok=%v, want 2", h, ok) }
    out.Reset()
    if _, err := query(opt, addrB, &out); err != nil || !strings.Contains(out.String(), "Received:      3 000 000 000 sat") {
        t.Errorf("block 2 not counted: %v\n%s", err, out.String())
    }
}

// Many blocks on many workers merge to exactly what the bot's own one-at-a-time
// Update writes, and a second build resumes at the cursor rather than counting a
// block twice.
func TestBuildMatchesUpdate(t *testing.T) {
    var saved = workers
    t.Cleanup(func() { workers = saved })
    workers = 6
    var c = &chain{tip: 205, blocks: map[int]core.Block{}}
    for h := 0; h <= 205; h++ {
        var hash = bytes.Repeat([]byte{byte(h % 13)}, 20)
        var script = append([]byte{0x00, 20}, hash...)
        var tx = core.Tx{Outputs: []core.Payment{{Script: script, Sat: 1000}, {Script: scriptA, Sat: int64(h)}}}
        if h > 0 { tx.Spent = []core.Payment{{Script: scriptB, Sat: 7}} }
        c.blocks[h] = core.Block{Time: int64(1_600_000_000 + h*600), Txs: []core.Tx{tx}}
    }
    var opt = setup(t, c)
    c.tip = 100
    if err := build(opt, &bytes.Buffer{}); err != nil { t.Fatalf("build: %v", err) }
    c.tip = 205
    if err := build(opt, &bytes.Buffer{}); err != nil { t.Fatalf("build: %v", err) }
    for h := 0; h <= 200; h++ {
        if c.asked[h] != 1 { t.Errorf("block %d asked for %d times", h, c.asked[h]) }
    }
    var built = map[string]addrstat.Stat{}
    for _, a := range []string{addrA, addrB, addrindex.Address(append([]byte{0x00, 20}, bytes.Repeat([]byte{5}, 20)...))} {
        built[a], _ = addrstat.Get(a)
    }
    var other = setup(t, &chain{tip: 200, blocks: c.blocks})
    if err := core.Init(other.url, other.user, other.pass, ""); err != nil { t.Fatal(err) }
    if err := addrstat.Update(200); err != nil { t.Fatalf("Update: %v", err) }
    for a, want := range built {
        if got, _ := addrstat.Get(a); got != want || got.Txs == 0 { t.Errorf("%s: build %+v, Update %+v", a, want, got) }
    }
}

func TestQueryNeedsABuild(t *testing.T) {
    var opt = setup(t, fixture())
    var _, err = query(opt, addrA, &bytes.Buffer{})
    if err == nil || !strings.Contains(err.Error(), "never been built") { t.Errorf("query of an unbuilt database: %v", err) }
}

func TestQueryNeedsAnExistingDatabase(t *testing.T) {
    var _, err = openDB(filepath.Join(t.TempDir(), "missing.db"), true)
    if err == nil { t.Error("opened a database that does not exist") }
}

func TestGroup(t *testing.T) {
    for n, want := range map[int64]string{0: "0", 999: "999", 1000: "1 000", -1234567: "-1 234 567"} {
        if got := group(n); got != want { t.Errorf("group(%d) = %q, want %q", n, got, want) }
    }
}

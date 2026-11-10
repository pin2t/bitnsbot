package main

import "bytes"
import "context"
import "database/sql"
import "encoding/hex"
import "fmt"
import "os"
import "path/filepath"
import "slices"
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
    onAsk  func(height int)
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
        if c.onAsk != nil { c.onAsk(height) }
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

// times is how often block h has been asked for, read under the lock the
// server's handlers write it under: a handler for a request its client gave up
// on may still be running.
func (c *chain) times(h int) int {
    c.mu.Lock()
    defer c.mu.Unlock()
    return c.asked[h]
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
// returns options pointing at both, the way the command line would, with the
// handle. Range files go beside the database, the default.
func setup(t *testing.T, c *chain) (*options, *sql.DB) {
    t.Helper()
    var server = coretest.Server(t, c.respond)
    t.Cleanup(core.Reset)
    var path = filepath.Join(t.TempDir(), "stat.db")
    var handle, err = openDB(path, false)
    if err != nil { t.Fatalf("open: %v", err) }
    t.Cleanup(func() { handle.Close() })
    return &options{db: path, rows: 100_000_000, url: server.URL, user: "testuser", pass: "testpass"}, handle
}

// busy is a chain of tip+1 blocks, each paying one of thirteen addresses 1000
// sat and addrA its own height, and spending 7 sat from addrB — so the same
// addresses recur across every range a small -rows cuts it into.
func busy(tip int) *chain {
    var c = &chain{tip: tip, blocks: map[int]core.Block{}}
    for h := 0; h <= tip; h++ {
        var hash = bytes.Repeat([]byte{byte(h % 13)}, 20)
        var script = append([]byte{0x00, 20}, hash...)
        var tx = core.Tx{Outputs: []core.Payment{{Script: script, Sat: 1000}, {Script: scriptA, Sat: int64(h)}}}
        if h > 0 { tx.Spent = []core.Payment{{Script: scriptB, Sat: 7}} }
        c.blocks[h] = core.Block{Time: int64(1_600_000_000 + h*600), Txs: []core.Tx{tx}}
    }
    return c
}

// matchesUpdate checks that what a build left for addrA, addrB and one of the
// thirteen is what the bot's own one-at-a-time Update writes over blocks 0..to of
// the same chain, in a database of its own.
func matchesUpdate(t *testing.T, c *chain, to int) {
    t.Helper()
    var built = map[string]addrstat.Stat{}
    for _, a := range []string{addrA, addrB, addrindex.Address(append([]byte{0x00, 20}, bytes.Repeat([]byte{5}, 20)...))} {
        built[a], _ = addrstat.Get(a)
    }
    var other, _ = setup(t, &chain{tip: to, blocks: c.blocks})
    if err := core.Init(other.url, other.user, other.pass, ""); err != nil { t.Fatal(err) }
    if err := addrstat.Update(int64(to)); err != nil { t.Fatalf("Update: %v", err) }
    for a, want := range built {
        if got, _ := addrstat.Get(a); got != want || got.Txs == 0 { t.Errorf("%s: build %+v, Update %+v", a, want, got) }
    }
}

// rangeFiles is what the range-file pattern matches in dir, by name.
func rangeFiles(t *testing.T, dir string) []string {
    t.Helper()
    var entries, err = os.ReadDir(dir)
    if err != nil { t.Fatal(err) }
    var names []string
    for _, e := range entries {
        if rangeFile.MatchString(e.Name()) { names = append(names, e.Name()) }
    }
    return names
}

// A build writes its range file, installs it — the cursor moved, the ranking
// indexes built again after the bulk insert, the range file gone — and query
// reads the result.
func TestBuildThenQuery(t *testing.T) {
    var c = fixture()
    var opt, handle = setup(t, c)
    var out bytes.Buffer
    if err := build(opt, handle, &out); err != nil { t.Fatalf("build: %v", err) }
    for _, want := range []string{"Wrote blocks 0..2 to range000000..000002.db", "Installed range000000..000002.db, 2 addresses", "Collected 3 blocks up to 2"} {
        if !strings.Contains(out.String(), want) { t.Errorf("build said %q, want %q", out.String(), want) }
    }
    if left := rangeFiles(t, filepath.Dir(opt.db)); len(left) > 0 { t.Errorf("range files left after the install: %v", left) }
    var n int
    if err := handle.QueryRow(`select count(*) from sqlite_master where type = 'index'
        and name in ('addrstat_balance', 'addrstat_txs', 'addrstat_last')`).Scan(&n); err != nil || n != 3 {
        t.Errorf("%d ranking indexes after the install (%v), want 3", n, err)
    }
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
    var opt, handle = setup(t, c)
    var out bytes.Buffer
    if err := build(opt, handle, &out); err != nil { t.Fatalf("build: %v", err) }
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

// Many blocks on many workers, cut into range files of a few addresses each and
// merged two at a time, install exactly what the bot's own one-at-a-time Update
// writes — into an empty table the first time and onto the rows already there
// the second — and the second build resumes at the cursor rather than counting a
// block twice.
func TestBuildMatchesUpdate(t *testing.T) {
    var saved = workers
    t.Cleanup(func() { workers = saved })
    workers = 6
    var c = busy(205)
    var opt, handle = setup(t, c)
    opt.rows = 5
    c.tip = 100
    var out bytes.Buffer
    if err := build(opt, handle, &out); err != nil { t.Fatalf("build: %v", err) }
    if strings.Count(out.String(), "Merged ") < 10 { t.Errorf("a first build of %d blocks at 5 addresses a range merged too little:\n%s", 96, out.String()) }
    c.tip = 205
    if err := build(opt, handle, &out); err != nil { t.Fatalf("build: %v", err) }
    for h := 0; h <= 200; h++ {
        if c.asked[h] != 1 { t.Errorf("block %d asked for %d times", h, c.asked[h]) }
    }
    if left := rangeFiles(t, filepath.Dir(opt.db)); len(left) > 0 { t.Errorf("range files left: %v", left) }
    matchesUpdate(t, c, 200)
}

// A build stopped partway writes what it had applied to a range file, and the
// next build carries on after it — reading none of those blocks again — merges
// the two and ends with what Update writes.
func TestStoppedBuildCarriesOn(t *testing.T) {
    var saved = workers
    t.Cleanup(func() { workers = saved })
    workers = 4
    var c = busy(205)
    var opt, handle = setup(t, c)
    if err := core.Init(opt.url, opt.user, opt.pass, ""); err != nil { t.Fatal(err) }
    var ctx, cancel = context.WithCancel(context.Background())
    defer cancel()
    c.onAsk = func(h int) {
        if h == 120 { cancel() }
    }
    var dir = filepath.Dir(opt.db)
    var _, err = collect(ctx, 0, 200, dir, opt.rows, &bytes.Buffer{})
    if err == nil || !strings.Contains(err.Error(), "stopped at block") { t.Fatalf("a stopped collect returned %v", err) }
    var done, rerr = ranges(dir)
    if rerr != nil || len(done) != 1 || done[0].from != 0 || done[0].to < 50 || done[0].to >= 200 {
        t.Fatalf("range files after the stop: %v, %v", done, rerr)
    }
    var out bytes.Buffer
    if err := build(opt, handle, &out); err != nil { t.Fatalf("build: %v", err) }
    if !strings.Contains(out.String(), fmt.Sprintf("Collecting blocks %d..200", done[0].to+1)) { t.Errorf("build said %q", out.String()) }
    for h := 0; int64(h) <= done[0].to; h++ {
        if n := c.times(h); n != 1 { t.Errorf("block %d, in the range file, asked for %d times", h, n) }
    }
    matchesUpdate(t, c, 200)
}

// A build refuses range files that do not run on from the cursor, rather than
// counting blocks it cannot account for.
func TestBuildRefusesAGap(t *testing.T) {
    var opt, handle = setup(t, fixture())
    if err := os.WriteFile(filepath.Join(filepath.Dir(opt.db), "range000001..000002.db"), nil, 0o644); err != nil { t.Fatal(err) }
    var err = build(opt, handle, &bytes.Buffer{})
    if err == nil || !strings.Contains(err.Error(), "does not start at block 0") { t.Errorf("a gap before the range files: %v", err) }
}

// Listing the range files deletes what a stopped run left behind — a temporary
// file and the inputs of a merge whose result is in place — and refuses two
// files that overlap otherwise.
func TestRangesClearsLeftovers(t *testing.T) {
    var dir = t.TempDir()
    for _, name := range []string{"range000000..000009.db", "range000000..000004.db", "range000005..000009.db",
        "range000010..000019.db", "range000020..000029.db.tmp", "notes.txt"} {
        if err := os.WriteFile(filepath.Join(dir, name), nil, 0o644); err != nil { t.Fatal(err) }
    }
    var got, err = ranges(dir)
    if err != nil { t.Fatal(err) }
    var want = []span{{0, 9, filepath.Join(dir, "range000000..000009.db")}, {10, 19, filepath.Join(dir, "range000010..000019.db")}}
    if !slices.Equal(got, want) { t.Errorf("ranges = %v, want %v", got, want) }
    var entries, _ = os.ReadDir(dir)
    var names []string
    for _, e := range entries { names = append(names, e.Name()) }
    if !slices.Equal(names, []string{"notes.txt", "range000000..000009.db", "range000010..000019.db"}) { t.Errorf("left %v", names) }
    if err := os.WriteFile(filepath.Join(dir, "range000015..000024.db"), nil, 0o644); err != nil { t.Fatal(err) }
    if _, err := ranges(dir); err == nil || !strings.Contains(err.Error(), "overlap") { t.Errorf("overlapping range files: %v", err) }
}

// The in-memory stage has 64 KB pages, which is what keeps a hundred million
// rows near 10 GB of memory rather than 19: a DSN pragma that did not apply would
// fall back to 4 KB pages with no error at all.
func TestStageUsesLargePages(t *testing.T) {
    var s, err = newStage(0)
    if err != nil { t.Fatal(err) }
    defer s.close()
    var size int
    if perr := s.tx.QueryRow("pragma page_size").Scan(&size); perr != nil || size != 65536 {
        t.Errorf("stage page size %d (%v), want 65536", size, perr)
    }
}

func TestQueryNeedsABuild(t *testing.T) {
    var opt, _ = setup(t, fixture())
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

package main

import "bytes"
import "database/sql"
import "encoding/hex"
import "fmt"
import "path/filepath"
import "strconv"
import "strings"
import "testing"
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
}

func (c *chain) respond(method string, params []any) (any, error) {
    switch method {
    case "getblockcount":
        return c.tip, nil
    case "getblockhash":
        return fmt.Sprint(params[0]), nil
    case "getblock":
        var height, _ = strconv.Atoi(params[0].(string))
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
// addrB and 19.9999 back to addrA, leaving 10 000 sat in fees.
func fixture() *chain {
    var pay = func(script []byte, sat int64) core.Payment { return core.Payment{Script: script, Sat: sat} }
    return &chain{tip: 2, blocks: map[int]core.Block{
        0: {Time: 1231006505},
        1: {Time: 1231469665, Txs: []core.Tx{{Outputs: []core.Payment{pay(scriptA, 5_000_000_000)}}}},
        2: {Time: 1231470000, Txs: []core.Tx{{
            Spent:   []core.Payment{pay(scriptA, 5_000_000_000)},
            Outputs: []core.Payment{pay(scriptB, 3_000_000_000), pay(scriptA, 1_999_990_000)},
        }}},
    }}
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
    if strings.Contains(out.String(), "ahead") { t.Errorf("a caught-up answer says it is behind:\n%s", out.String()) }
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

// tableOf reads the file at path on a handle of its own, every row and the cursor
// as text, so a comparison sees the file and not whatever the packages point at.
func tableOf(t *testing.T, path string) (rows []string, cursor int64) {
    t.Helper()
    var handle, err = sql.Open("sqlite", "file:"+path)
    if err != nil { t.Fatal(err) }
    defer handle.Close()
    var res, qerr = handle.Query(`select addr || '|' || type || '|' || balance || '|' || recv || '|' || sent || '|' ||
        flow || '|' || fees || '|' || txs || '|' || first || '|' || last from addrstat order by addr`)
    if qerr != nil { t.Fatal(qerr) }
    defer res.Close()
    for res.Next() {
        var row string
        if serr := res.Scan(&row); serr != nil { t.Fatal(serr) }
        rows = append(rows, row)
    }
    if rerr := handle.QueryRow("select place from cursors where name = ?", cursors.AddrStat).Scan(&cursor); rerr != nil { t.Fatal(rerr) }
    return rows, cursor
}

// setupMemory is setup plus the handle buildMemory hands the file back to.
func setupMemory(t *testing.T, c *chain) (*options, *sql.DB) {
    t.Helper()
    var opt = setup(t, c)
    var handle, err = openDB(opt.db, false)
    if err != nil { t.Fatalf("open: %v", err) }
    t.Cleanup(func() { handle.Close() })
    return opt, handle
}

func TestMemoryBuildWritesWhatADiskBuildWrites(t *testing.T) {
    var disk = setup(t, fixture())
    if err := build(disk, &bytes.Buffer{}); err != nil { t.Fatalf("build: %v", err) }
    var want, wantAt = tableOf(t, disk.db)
    if len(want) != 2 || wantAt != 2 { t.Fatalf("disk build: %v at %d", want, wantAt) }
    var opt, handle = setupMemory(t, fixture())
    var out bytes.Buffer
    if err := buildMemory(opt, handle, &out); err != nil { t.Fatalf("buildMemory: %v", err) }
    var got, gotAt = tableOf(t, opt.db)
    if strings.Join(got, "\n") != strings.Join(want, "\n") || gotAt != wantAt {
        t.Errorf("memory build wrote %v at %d, disk build %v at %d", got, gotAt, want, wantAt)
    }
    if !strings.Contains(out.String(), "Wrote 2 addresses") { t.Errorf("build said %q", out.String()) }
    out.Reset()
    var found, err = query(opt, addrA, &out)
    if err != nil || !found { t.Fatalf("query after a memory build: found %v, %v", found, err) }
    if !strings.Contains(out.String(), "Balance:       1 999 990 000 sat") { t.Errorf("query said %q", out.String()) }
}

// A run resumes from what the file holds, and the row of an address emptied in
// memory has to be gone from the file, which a merge of the table would leave.
func TestMemoryBuildResumesAndDropsEmptied(t *testing.T) {
    var c = fixture()
    c.tip = 1
    var opt, handle = setupMemory(t, c)
    if err := build(opt, &bytes.Buffer{}); err != nil { t.Fatalf("build: %v", err) }
    c.tip = 3
    c.blocks[2].Txs[0].Outputs[1] = core.Payment{Script: scriptB, Sat: 1_999_990_000}
    c.blocks[3] = core.Block{Time: 1231470100}
    var calls = map[string]int{}
    var server = coretest.Server(t, func(method string, params []any) (any, error) {
        if method == "getblock" { calls[params[0].(string)]++ }
        return c.respond(method, params)
    })
    opt.url = server.URL
    if err := buildMemory(opt, handle, &bytes.Buffer{}); err != nil { t.Fatalf("buildMemory: %v", err) }
    if calls["0"] != 0 || calls["1"] != 0 || calls["2"] != 1 { t.Errorf("blocks read: %v", calls) }
    var rows, at = tableOf(t, opt.db)
    if at != 3 || len(rows) != 1 || !strings.HasPrefix(rows[0], addrB + "|") { t.Errorf("file holds %v at %d", rows, at) }
    if !strings.Contains(rows[0], "|4999990000|") { t.Errorf("the row was not carried over from the file: %v", rows) }
}

func TestMemoryBuildLeavesTheRestOfTheFile(t *testing.T) {
    var opt, handle = setupMemory(t, fixture())
    for _, s := range []string{
        "create table other (k text)",
        "insert into other values ('kept')",
        "insert into cursors (name, place) values ('addrindex', 77)",
    } {
        if _, err := handle.Exec(s); err != nil { t.Fatal(err) }
    }
    if err := buildMemory(opt, handle, &bytes.Buffer{}); err != nil { t.Fatalf("buildMemory: %v", err) }
    var kept string
    var place int64
    if err := handle.QueryRow("select k from other").Scan(&kept); err != nil || kept != "kept" { t.Errorf("other: %q, %v", kept, err) }
    if err := handle.QueryRow("select place from cursors where name = 'addrindex'").Scan(&place); err != nil || place != 77 { t.Errorf("addrindex cursor: %d, %v", place, err) }
    var indexes int
    if err := handle.QueryRow("select count(*) from sqlite_master where type = 'index' and tbl_name = 'addrstat' and sql is not null").Scan(&indexes); err != nil { t.Fatal(err) }
    if indexes != 3 { t.Errorf("the file has %d addrstat indexes, want 3", indexes) }
    if at, ok := cursors.Get(cursors.AddrStat); !ok || at != 2 { t.Errorf("the packages do not read the file: %d, %v", at, ok) }
    if addrstat.Count() != 2 { t.Errorf("Count = %d after the build, want 2", addrstat.Count()) }
}

// A node that never answers fails before anything moves, and the file is untouched.
func TestMemoryBuildFailureLeavesTheFile(t *testing.T) {
    var c = fixture()
    c.tip = 1
    var opt, handle = setupMemory(t, c)
    if err := build(opt, &bytes.Buffer{}); err != nil { t.Fatalf("build: %v", err) }
    var want, wantAt = tableOf(t, opt.db)
    opt.url = "http://127.0.0.1:1"
    if err := buildMemory(opt, handle, &bytes.Buffer{}); err == nil { t.Fatal("a build against no node succeeded") }
    var got, gotAt = tableOf(t, opt.db)
    if strings.Join(got, "\n") != strings.Join(want, "\n") || gotAt != wantAt { t.Errorf("file changed: %v at %d", got, gotAt) }
}

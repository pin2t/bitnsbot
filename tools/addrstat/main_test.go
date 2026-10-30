package main

import "bytes"
import "encoding/hex"
import "fmt"
import "path/filepath"
import "strconv"
import "strings"
import "testing"
import "bitnsbot/addrindex"
import "bitnsbot/core"
import "bitnsbot/core/coretest"

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

func TestBuildResumesAndReportsTheTip(t *testing.T) {
    var c = fixture()
    var opt = setup(t, c)
    c.tip = 1
    var out bytes.Buffer
    if err := build(opt, &out); err != nil { t.Fatalf("build: %v", err) }
    c.tip = 2
    out.Reset()
    if _, err := query(opt, addrB, &out); err != nil { t.Fatalf("query: %v", err) }
    if !strings.Contains(out.String(), "holds nothing as of block 1") { t.Errorf("query said %q", out.String()) }
    if !strings.Contains(out.String(), "tip is block 2, 1 blocks ahead") { t.Errorf("query did not report the tip: %q", out.String()) }
    out.Reset()
    if err := build(opt, &out); err != nil { t.Fatalf("build: %v", err) }
    if !strings.Contains(out.String(), "Collecting blocks 2..2") { t.Errorf("build did not resume: %q", out.String()) }
    out.Reset()
    if err := build(opt, &out); err != nil { t.Fatalf("build: %v", err) }
    if !strings.Contains(out.String(), "already at the tip") { t.Errorf("build at the tip said %q", out.String()) }
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

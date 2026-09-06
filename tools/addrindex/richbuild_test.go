package main

import "database/sql"
import "encoding/hex"
import "os"
import "path/filepath"
import "strings"
import "testing"

import "bitnsbot/addrindex"

// The fixture chain pays the miner a coinbase in each of its four blocks, funds
// payScript with 20000 in block 1, and in block 2 spends all of that back —
// 10000 to the miner, 10000 as change, and 500 into an OP_RETURN that no address
// can be derived from. So at the tip payScript holds 10000, otherScript holds
// four coinbases less the one it spent plus the 10000 it was paid, and the
// OP_RETURN's coins belong to no address at all.
const wantPay = 10000
const wantOther = 4*coinbaseSat - coinbaseSat + 10000

func richOptions(t *testing.T, url string) *options {
    return &options{dbsqlite: filepath.Join(t.TempDir(), "rich.db"), url: url,
        shards: 4, batch: 2, fetch: 2, tmp: filepath.Join(t.TempDir(), "shards")}
}

// richRows reads the table the command exists to write.
func richRows(t *testing.T, path string) map[string]int64 {
    t.Helper()
    return query(t, path, "select addr, balance from rich")
}

// balanceRows reads the state rich is built from, keyed by the script as hex so
// a script with no address still has a name here.
func balanceRows(t *testing.T, path string) map[string]int64 {
    t.Helper()
    var db, err = sql.Open("sqlite", "file:"+path+"?mode=ro")
    if err != nil { t.Fatalf("open %s: %v", path, err) }
    defer db.Close()
    var rows, qerr = db.Query("select script, balance from balances")
    if qerr != nil { t.Fatalf("query balances: %v", qerr) }
    defer rows.Close()
    var out = map[string]int64{}
    for rows.Next() {
        var script []byte
        var balance int64
        if err := rows.Scan(&script, &balance); err != nil { t.Fatalf("scan: %v", err) }
        out[hex.EncodeToString(script)] = balance
    }
    return out
}

func query(t *testing.T, path, sqlText string) map[string]int64 {
    t.Helper()
    var db, err = sql.Open("sqlite", "file:"+path+"?mode=ro")
    if err != nil { t.Fatalf("open %s: %v", path, err) }
    defer db.Close()
    var rows, qerr = db.Query(sqlText)
    if qerr != nil { t.Fatalf("%s: %v", sqlText, qerr) }
    defer rows.Close()
    var out = map[string]int64{}
    for rows.Next() {
        var key string
        var value int64
        if err := rows.Scan(&key, &value); err != nil { t.Fatalf("scan: %v", err) }
        out[key] = value
    }
    return out
}

func storedHeight(t *testing.T, path, key string) int {
    t.Helper()
    var store, err = openRich(path)
    if err != nil { t.Fatalf("open: %v", err) }
    defer store.close()
    var h, ok, herr = store.height(key)
    if herr != nil { t.Fatalf("height %s: %v", key, herr) }
    if !ok { return -1 }
    return h
}

// The whole command end to end: read the fake node's blocks and spent outputs
// over REST, and end up with what each address holds.
func TestRichBuild(t *testing.T) {
    var srv = fakeCore(t, 3)
    var opt = richOptions(t, srv.URL)
    var out = capture(t, func() {
        if err := richbuild(opt); err != nil { t.Fatalf("richbuild: %v", err) }
    })
    if !strings.Contains(out, "blocks 0..3") {
        t.Errorf("richbuild did not report its range: %q", out)
    }
    var rich = richRows(t, opt.dbsqlite)
    if len(rich) != 2 {
        t.Fatalf("rich = %v, want exactly the two addresses that hold coins", rich)
    }
    if got := rich[scriptAddress(payScript)]; got != wantPay {
        t.Errorf("%s holds %d, want %d", scriptAddress(payScript), got, wantPay)
    }
    if got := rich[scriptAddress(otherScript)]; got != wantOther {
        t.Errorf("%s holds %d, want %d", scriptAddress(otherScript), got, wantOther)
    }
    // the OP_RETURN's coins have no address, so they are kept in the state a
    // later run carries forward and left out of rich
    var balances = balanceRows(t, opt.dbsqlite)
    if got := balances[hex.EncodeToString(opReturnScript)]; got != 500 {
        t.Errorf("the addressless script holds %d in balances, want 500", got)
    }
    if len(balances) != 3 {
        t.Errorf("balances = %v, want the three funded scripts", balances)
    }
    if h := storedHeight(t, opt.dbsqlite, "height"); h != 3 {
        t.Errorf("stored height = %d, want the tip 3", h)
    }
    if h := storedHeight(t, opt.dbsqlite, "rich"); h != 3 {
        t.Errorf("rich was built at %d, want 3", h)
    }
    // the movements are worth nothing once they are summed, so the run takes
    // its working directory away with it
    if _, err := os.Stat(opt.tmp); !os.IsNotExist(err) {
        t.Errorf("the shard directory %s outlived the run", opt.tmp)
    }
}

// A second run has nothing to add and must say so rather than scanning again.
func TestRichBuildAtTip(t *testing.T) {
    var srv = fakeCore(t, 3)
    var opt = richOptions(t, srv.URL)
    capture(t, func() {
        if err := richbuild(opt); err != nil { t.Fatalf("first run: %v", err) }
    })
    var out = capture(t, func() {
        if err := richbuild(opt); err != nil { t.Fatalf("second run: %v", err) }
    })
    if !strings.Contains(out, "already at block 3") {
        t.Errorf("a second run should be a no-op, got %q", out)
    }
}

// Stopping short of the tip and running again must land on the same balances as
// one run over the whole chain — that is what makes the stored state a place to
// carry on from rather than a snapshot to start over from.
func TestRichBuildCarriesBalancesForward(t *testing.T) {
    var srv = fakeCore(t, 3)
    var whole = richOptions(t, srv.URL)
    capture(t, func() {
        if err := richbuild(whole); err != nil { t.Fatalf("whole: %v", err) }
    })
    var parts = richOptions(t, srv.URL)
    parts.to = 1
    capture(t, func() {
        if err := richbuild(parts); err != nil { t.Fatalf("to block 1: %v", err) }
    })
    if h := storedHeight(t, parts.dbsqlite, "height"); h != 1 {
        t.Fatalf("stopped at %d, want block 1", h)
    }
    if got := richRows(t, parts.dbsqlite)[scriptAddress(payScript)]; got != 20000 {
        t.Errorf("after block 1 the address holds %d, want the 20000 it was paid", got)
    }
    parts.to = 0
    var out = capture(t, func() {
        if err := richbuild(parts); err != nil { t.Fatalf("rest of the chain: %v", err) }
    })
    if !strings.Contains(out, "Carried") {
        t.Errorf("the second run should carry the stored balances forward: %q", out)
    }
    if !strings.Contains(out, "blocks 2..3") {
        t.Errorf("the second run should only read what is new: %q", out)
    }
    var got, want = richRows(t, parts.dbsqlite), richRows(t, whole.dbsqlite)
    if len(got) != len(want) {
        t.Fatalf("two runs = %v, one run = %v", got, want)
    }
    for addr, balance := range want {
        if got[addr] != balance {
            t.Errorf("%s holds %d after two runs, %d after one", addr, got[addr], balance)
        }
    }
}

// -min is what makes it a rich list rather than every address on the chain.
func TestRichBuildMinimum(t *testing.T) {
    var srv = fakeCore(t, 3)
    var opt = richOptions(t, srv.URL)
    opt.min = wantPay + 1
    capture(t, func() {
        if err := richbuild(opt); err != nil { t.Fatalf("richbuild: %v", err) }
    })
    var rich = richRows(t, opt.dbsqlite)
    if _, ok := rich[scriptAddress(payScript)]; ok {
        t.Errorf("rich = %v; the address below -min should not be in it", rich)
    }
    if got := rich[scriptAddress(otherScript)]; got != wantOther {
        t.Errorf("rich = %v; the address above -min should be", rich)
    }
    // the state keeps every balance whatever -min says, or the next run would
    // carry a wrong total forward
    if len(balanceRows(t, opt.dbsqlite)) != 3 {
        t.Errorf("balances = %v, want all three funded scripts", balanceRows(t, opt.dbsqlite))
    }
}

// It writes SQLite and nothing else, so it has nowhere to put the answer without
// being told where.
func TestRichBuildNeedsADatabase(t *testing.T) {
    var srv = fakeCore(t, 3)
    var opt = richOptions(t, srv.URL)
    opt.dbsqlite = ""
    if err := richbuild(opt); err == nil {
        t.Error("richbuild without -dbsqlite should say so, not write somewhere of its own choosing")
    }
}

// The three outputs Core's UTXO set never held are mainnet's, so another chain's
// blocks at those heights must be left alone.
func TestVoidedIsMainnetOnly(t *testing.T) {
    var blocks = chainBlocks()
    for _, height := range []int{0, 91722, 91812} {
        if got := voided("main", height, blocks[0][0]); len(got) != 1 {
            t.Errorf("mainnet block %d: voided %d outputs, want the coinbase's one", height, len(got))
        }
        if got := voided("regtest", height, blocks[0][0]); got != nil {
            t.Errorf("regtest block %d: voided %v, want nothing", height, got)
        }
    }
    if got := voided("main", 91721, blocks[0][0]); got != nil {
        t.Errorf("an ordinary block voided %v, want nothing", got)
    }
}

// A shard has to hand back exactly what was put in it, since a balance is the
// sum of every record and one lost byte is somebody's coins.
func TestShardsRoundTrip(t *testing.T) {
    var dir = filepath.Join(t.TempDir(), "shards")
    var sh, err = newShards(dir, 8, 4)
    if err != nil { t.Fatalf("newShards: %v", err) }
    defer sh.remove()
    var want = map[string]int64{}
    // a long script as well, since the length is a varint and 128 is where it
    // stops fitting in one byte
    var scripts = []string{string(payScript), string(otherScript), strings.Repeat("x", 300)}
    for i, script := range scripts {
        for n := 0; n < 10; n++ {
            var sat = int64((i+1)*1000 - n*7)
            if n%3 == 0 { sat = -sat }
            want[script] += sat
            if err := sh.put(script, sat); err != nil { t.Fatalf("put: %v", err) }
        }
    }
    if err := sh.flush(); err != nil { t.Fatalf("flush: %v", err) }
    var got = map[string]int64{}
    for i := 0; i < 8; i++ {
        if err := sh.each(i, func(script []byte, sat int64) error {
            got[string(script)] += sat
            return nil
        }); err != nil {
            t.Fatalf("each %d: %v", i, err)
        }
    }
    if len(got) != len(want) { t.Fatalf("read back %d scripts, want %d", len(got), len(want)) }
    for script, sat := range want {
        if got[script] != sat {
            t.Errorf("script %.10q summed to %d, want %d", script, got[script], sat)
        }
    }
    // each script's records all land in one shard, which is what lets a shard be
    // summed on its own
    for i := 0; i < 8; i++ {
        var seen = map[string]bool{}
        sh.each(i, func(script []byte, sat int64) error {
            seen[string(script)] = true
            return nil
        })
        for script := range seen {
            if int(fnvHash(script)%8) != i {
                t.Errorf("script %.10q was written to shard %d", script, i)
            }
        }
    }
}

// Balances is the shared parser's view of a block, and the amounts and signs it
// reports are what every figure downstream is made of: block 1 pays a coinbase,
// pays the watched address, and spends block 0's coinbase.
func TestBalancesReportsBothSides(t *testing.T) {
    var blocks = chainBlocks()
    var blk = addrindex.Block{Raw: blocks[1][0], Spent: blocks[1][1]}
    var moves, ok = addrindex.Balances(blk)
    if !ok { t.Fatal("Balances failed on the fixture block") }
    var sums = map[string]int64{}
    for _, m := range moves { sums[string(m.Script)] += m.Sat }
    if len(moves) != 3 {
        t.Fatalf("block 1 moved %d amounts, want the two outputs and the one spend", len(moves))
    }
    // the miner was paid a subsidy and spent one of the same size, so nothing of
    // it stayed; the address it paid kept what it was sent
    if got := sums[string(otherScript)]; got != 0 {
        t.Errorf("the miner's script moved %d, want 0 — it was paid and spent the same", got)
    }
    if got := sums[string(payScript)]; got != 20000 {
        t.Errorf("the address moved %d, want the 20000 it was paid", got)
    }
}

// The genesis coinbase is not in Core's UTXO set, so a mainnet run must not put
// it in anybody's balance — the one fixup that changes a real answer.
func TestRichBuildVoidsTheGenesisCoinbase(t *testing.T) {
    var srv = fakeCore(t, 3)
    var old = fakeChain
    fakeChain = "main"
    defer func() { fakeChain = old }()
    var opt = richOptions(t, srv.URL)
    capture(t, func() {
        if err := richbuild(opt); err != nil { t.Fatalf("richbuild: %v", err) }
    })
    // one coinbase less than the same chain sums to off mainnet
    if got := richRows(t, opt.dbsqlite)[scriptAddress(otherScript)]; got != wantOther-coinbaseSat {
        t.Errorf("the miner holds %d, want %d — genesis pays nobody", got, wantOther-coinbaseSat)
    }
}

// A tip can be reorged away minutes after a run ends. Carrying the stored
// balances forward onto a chain they were not summed on would keep counting
// coins from a block the node no longer has, so the run refuses instead.
func TestRichBuildRefusesAfterAReorg(t *testing.T) {
    var srv = fakeCore(t, 3)
    var opt = richOptions(t, srv.URL)
    capture(t, func() {
        if err := richbuild(opt); err != nil { t.Fatalf("first run: %v", err) }
    })
    var db, err = sql.Open("sqlite", "file:"+opt.dbsqlite)
    if err != nil { t.Fatalf("open: %v", err) }
    if _, err := db.Exec("update meta set value = ? where key = 'hash'", strings.Repeat("f", 64)); err != nil {
        t.Fatalf("rewrite the stored hash: %v", err)
    }
    db.Close()
    opt.to = 4
    var rerr = richbuild(opt)
    if rerr == nil {
        t.Fatal("a run whose stored block is not the node's should refuse, not carry the balances forward")
    }
    if !strings.Contains(rerr.Error(), "reorged") {
        t.Errorf("error = %v, want one that says what happened", rerr)
    }
}

// rich is built from the stored balances rather than accumulated, so changing
// the threshold has to rebuild it — reporting that there is nothing to do would
// leave the old rows sitting there under a -min they do not match.
func TestRichBuildRebuildsWhenMinChanges(t *testing.T) {
    var srv = fakeCore(t, 3)
    var opt = richOptions(t, srv.URL)
    capture(t, func() {
        if err := richbuild(opt); err != nil { t.Fatalf("first run: %v", err) }
    })
    if len(richRows(t, opt.dbsqlite)) != 2 {
        t.Fatalf("first run wrote %v", richRows(t, opt.dbsqlite))
    }
    opt.min = wantPay + 1
    var out = capture(t, func() {
        if err := richbuild(opt); err != nil { t.Fatalf("second run: %v", err) }
    })
    if strings.Contains(out, "already at block") {
        t.Errorf("a new -min should rebuild rich, got %q", out)
    }
    if got := richRows(t, opt.dbsqlite); len(got) != 1 {
        t.Errorf("rich = %v, want only the address above the new -min", got)
    }
}

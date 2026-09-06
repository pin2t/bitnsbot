package main

import "database/sql"
import "encoding/hex"
import "os"
import "path/filepath"
import "strings"
import "testing"

// The fixture chain pays otherScript a coinbase in each of its four blocks and
// pays it again in block 2, but the last time anything was spent *from* it was
// block 1. payScript is paid in block 1 and spends in block 2. So otherScript is
// the more abandoned of the two even though it was paid most recently, which is
// the whole distinction this command draws.
var wantLast = map[string]int64{
    scriptAddress(otherScript): blockTime(1),
    scriptAddress(payScript):   blockTime(2),
}

func abaOptions(t *testing.T, url string) *options {
    return &options{dbsqlite: filepath.Join(t.TempDir(), "abandoned.db"), url: url,
        shards: 4, batch: 2, fetch: 2, sum: 3, top: 10, tmp: filepath.Join(t.TempDir(), "shards")}
}

// abandonedRows reads the table the command exists to write, in the order it
// ranked them.
func abandonedRows(t *testing.T, path string) (addrs []string, balance, last map[string]int64) {
    t.Helper()
    var db, err = sql.Open("sqlite", "file:"+path+"?mode=ro")
    if err != nil { t.Fatalf("open %s: %v", path, err) }
    defer db.Close()
    var rows, qerr = db.Query("select addr, balance, lastTx from abandoned order by lastTx, addr")
    if qerr != nil { t.Fatalf("query abandoned: %v", qerr) }
    defer rows.Close()
    balance, last = map[string]int64{}, map[string]int64{}
    for rows.Next() {
        var addr string
        var bal, lastTx int64
        if err := rows.Scan(&addr, &bal, &lastTx); err != nil { t.Fatalf("scan: %v", err) }
        addrs = append(addrs, addr)
        balance[addr], last[addr] = bal, lastTx
    }
    return addrs, balance, last
}

// The whole command end to end: read the fake node's blocks and spent outputs
// over REST, and end up with who holds coins and when they last moved any.
func TestAbaBuild(t *testing.T) {
    var srv = fakeCore(t, 3)
    var opt = abaOptions(t, srv.URL)
    var out = capture(t, func() {
        if err := ababuild(opt); err != nil { t.Fatalf("ababuild: %v", err) }
    })
    if !strings.Contains(out, "blocks 0..3") {
        t.Errorf("ababuild did not report its range: %q", out)
    }
    var addrs, balance, last = abandonedRows(t, opt.dbsqlite)
    if len(addrs) != 2 {
        t.Fatalf("abandoned = %v, want exactly the two addresses that hold coins", addrs)
    }
    // the ranking itself: the address paid most recently is the one that has
    // gone longest without spending, and it comes first
    if addrs[0] != scriptAddress(otherScript) || addrs[1] != scriptAddress(payScript) {
        t.Errorf("abandoned ranks %v; the address that last spent earliest comes first", addrs)
    }
    for addr, want := range wantLast {
        if last[addr] != want {
            t.Errorf("%s last moved at %d, want %d — its last spend, not its last payment",
                addr, last[addr], want)
        }
    }
    if got := balance[scriptAddress(payScript)]; got != wantPay {
        t.Errorf("%s holds %d, want %d", scriptAddress(payScript), got, wantPay)
    }
    if got := balance[scriptAddress(otherScript)]; got != wantOther {
        t.Errorf("%s holds %d, want %d", scriptAddress(otherScript), got, wantOther)
    }
    // the OP_RETURN's coins have no address, so they are kept in the state a
    // later run carries forward and left out of the answer
    if got := abaBalances(t, opt.dbsqlite)[hex.EncodeToString(opReturnScript)]; got.sat != 500 {
        t.Errorf("the addressless script holds %d in the state, want 500", got.sat)
    }
    if h := storedHeight(t, opt.dbsqlite, "height"); h != 3 {
        t.Errorf("stored height = %d, want the tip 3", h)
    }
    if h := storedHeight(t, opt.dbsqlite, "abandoned"); h != 3 {
        t.Errorf("the list was built at %d, want 3", h)
    }
    if _, err := os.Stat(opt.tmp); !os.IsNotExist(err) {
        t.Errorf("the shard directory %s outlived the run", opt.tmp)
    }
}

// abaBalances reads the state the answer is selected from, keyed by the script as
// hex so a script with no address still has a name here.
func abaBalances(t *testing.T, path string) map[string]move {
    t.Helper()
    var store, err = openAba(path)
    if err != nil { t.Fatalf("open %s: %v", path, err) }
    defer store.close()
    var out = map[string]move{}
    if err := store.each(func(script []byte, balance, spent, paid int64) error {
        out[hex.EncodeToString(script)] = move{sat: balance, spent: spent, paid: paid}
        return nil
    }); err != nil {
        t.Fatalf("read the state: %v", err)
    }
    return out
}

// The rule the ranking rests on, driven straight at the shards so each case is
// one row rather than a chain that has to produce it: a spend outranks a later
// payment, and an address that has never spent is ranked by when it was paid.
func TestAbaLastMovedRule(t *testing.T) {
    var dir = filepath.Join(t.TempDir(), "shards")
    var sh, err = newTimedShards(dir, 4, 4)
    if err != nil { t.Fatalf("newTimedShards: %v", err) }
    defer sh.remove()
    // paid twice and never spent, so the later payment is its date
    if err := sh.putAt(string(payScript), 400, 0, 100); err != nil { t.Fatalf("put: %v", err) }
    if err := sh.putAt(string(payScript), 100, 0, 700); err != nil { t.Fatalf("put: %v", err) }
    // spent long ago and paid since, so the spend is its date
    if err := sh.putAt(string(otherScript), 900, 300, 800); err != nil { t.Fatalf("put: %v", err) }
    if err := sh.flush(); err != nil { t.Fatalf("flush: %v", err) }
    var opt = &options{dbsqlite: filepath.Join(t.TempDir(), "abandoned.db"), shards: 4, sum: 2, top: 10}
    var store, oerr = openAba(opt.dbsqlite)
    if oerr != nil { t.Fatalf("open: %v", oerr) }
    defer store.close()
    capture(t, func() {
        if err := combine(store, sh, opt, 9, "hash"); err != nil { t.Fatalf("combine: %v", err) }
        if _, err := store.shortlist(0, opt.top, 9); err != nil { t.Fatalf("shortlist: %v", err) }
    })
    var addrs, balance, last = abandonedRows(t, opt.dbsqlite)
    if len(addrs) != 2 { t.Fatalf("abandoned = %v, want both scripts", addrs) }
    if got := last[scriptAddress(otherScript)]; got != 300 {
        t.Errorf("the script that spent at 300 and was paid at 800 reports %d, want 300", got)
    }
    if got := last[scriptAddress(payScript)]; got != 700 {
        t.Errorf("the script that never spent reports %d, want 700 — the last time it was paid", got)
    }
    if addrs[0] != scriptAddress(otherScript) {
        t.Errorf("abandoned ranks %v, want the older date first", addrs)
    }
    if got := balance[scriptAddress(payScript)]; got != 500 {
        t.Errorf("the twice-paid script holds %d, want its two payments summed", got)
    }
}

// One address can be paid by more than one script — an early miner paid by
// `<pubkey> OP_CHECKSIG` and later by an ordinary pay-to-pubkey-hash — and those
// scripts hash into different shards. They have to end up as one row: the coins
// added together, and the later of the two dates, or the address would be ranked
// on half of its own history. These are precisely the addresses this list is
// about, so it is not a corner case here.
func TestAbaCombinesScriptsOfOneAddress(t *testing.T) {
    var pubkey = make([]byte, 33)
    pubkey[0] = 2
    for i := 1; i < 33; i++ { pubkey[i] = byte(i) }
    var p2pk = append(append([]byte{33}, pubkey...), 0xac)
    var p2pkh = append(append([]byte{0x76, 0xa9, 20}, hash160(pubkey)...), 0x88, 0xac)
    if scriptAddress(p2pk) != scriptAddress(p2pkh) {
        t.Fatalf("the fixture scripts pay %s and %s, want one address",
            scriptAddress(p2pk), scriptAddress(p2pkh))
    }
    var dir = filepath.Join(t.TempDir(), "shards")
    var sh, err = newTimedShards(dir, 4, 4)
    if err != nil { t.Fatalf("newTimedShards: %v", err) }
    defer sh.remove()
    if err := sh.putAt(string(p2pk), 5000, 200, 100); err != nil { t.Fatalf("put: %v", err) }
    if err := sh.putAt(string(p2pkh), 3000, 600, 400); err != nil { t.Fatalf("put: %v", err) }
    if err := sh.flush(); err != nil { t.Fatalf("flush: %v", err) }
    var opt = &options{dbsqlite: filepath.Join(t.TempDir(), "abandoned.db"), shards: 4, sum: 2, top: 10}
    var store, oerr = openAba(opt.dbsqlite)
    if oerr != nil { t.Fatalf("open: %v", oerr) }
    defer store.close()
    capture(t, func() {
        if err := combine(store, sh, opt, 9, "hash"); err != nil { t.Fatalf("combine: %v", err) }
        if _, err := store.shortlist(0, opt.top, 9); err != nil { t.Fatalf("shortlist: %v", err) }
    })
    var addrs, balance, last = abandonedRows(t, opt.dbsqlite)
    if len(addrs) != 1 {
        t.Fatalf("abandoned = %v, want the one address both scripts pay", addrs)
    }
    if balance[addrs[0]] != 8000 {
        t.Errorf("%s holds %d, want both scripts' coins", addrs[0], balance[addrs[0]])
    }
    if last[addrs[0]] != 600 {
        t.Errorf("%s last spent at %d, want 600 — the later of its two scripts", addrs[0], last[addrs[0]])
    }
}

// Stopping short of the tip and running again must land on the same list as one
// run over the whole chain, dates included — that is what makes the stored state
// a place to carry on from rather than a snapshot to start over from.
func TestAbaBuildCarriesStateForward(t *testing.T) {
    var srv = fakeCore(t, 3)
    var whole = abaOptions(t, srv.URL)
    capture(t, func() {
        if err := ababuild(whole); err != nil { t.Fatalf("whole: %v", err) }
    })
    var parts = abaOptions(t, srv.URL)
    parts.to = 2
    capture(t, func() {
        if err := ababuild(parts); err != nil { t.Fatalf("to block 2: %v", err) }
    })
    if h := storedHeight(t, parts.dbsqlite, "height"); h != 2 {
        t.Fatalf("stopped at %d, want block 2", h)
    }
    parts.to = 0
    var out = capture(t, func() {
        if err := ababuild(parts); err != nil { t.Fatalf("rest of the chain: %v", err) }
    })
    if !strings.Contains(out, "Carried") {
        t.Errorf("the second run should carry the stored state forward: %q", out)
    }
    if !strings.Contains(out, "blocks 3..3") {
        t.Errorf("the second run should only read what is new: %q", out)
    }
    var gotAddrs, gotBalance, gotLast = abandonedRows(t, parts.dbsqlite)
    var wantAddrs, wantBalance, wantLastTx = abandonedRows(t, whole.dbsqlite)
    if len(gotAddrs) != len(wantAddrs) {
        t.Fatalf("two runs = %v, one run = %v", gotAddrs, wantAddrs)
    }
    for i, addr := range wantAddrs {
        if gotAddrs[i] != addr {
            t.Fatalf("two runs ranked %v, one run %v", gotAddrs, wantAddrs)
        }
        if gotBalance[addr] != wantBalance[addr] || gotLast[addr] != wantLastTx[addr] {
            t.Errorf("%s = %d sat last moved %d after two runs, %d/%d after one",
                addr, gotBalance[addr], gotLast[addr], wantBalance[addr], wantLastTx[addr])
        }
    }
}

// -top is what makes it a list of ten thousand rather than of every address on
// the chain, so it has to cut from the recent end.
func TestAbaBuildTop(t *testing.T) {
    var srv = fakeCore(t, 3)
    var opt = abaOptions(t, srv.URL)
    opt.top = 1
    capture(t, func() {
        if err := ababuild(opt); err != nil { t.Fatalf("ababuild: %v", err) }
    })
    var addrs, _, _ = abandonedRows(t, opt.dbsqlite)
    if len(addrs) != 1 || addrs[0] != scriptAddress(otherScript) {
        t.Errorf("abandoned = %v, want only the most abandoned address", addrs)
    }
    // the state keeps every script whatever -top says, or the next run would
    // carry a wrong total forward
    if len(abaBalances(t, opt.dbsqlite)) != 3 {
        t.Errorf("the state = %v, want all three funded scripts", abaBalances(t, opt.dbsqlite))
    }
}

// The answer is selected from the state rather than accumulated, so asking for a
// different number of addresses has to rebuild it — reporting that there is
// nothing to do would leave rows that do not match the flags.
func TestAbaBuildRebuildsWhenTopChanges(t *testing.T) {
    var srv = fakeCore(t, 3)
    var opt = abaOptions(t, srv.URL)
    capture(t, func() {
        if err := ababuild(opt); err != nil { t.Fatalf("first run: %v", err) }
    })
    var again = capture(t, func() {
        if err := ababuild(opt); err != nil { t.Fatalf("second run: %v", err) }
    })
    if !strings.Contains(again, "already at block 3") {
        t.Errorf("a second run with the same flags should be a no-op, got %q", again)
    }
    opt.top = 1
    var out = capture(t, func() {
        if err := ababuild(opt); err != nil { t.Fatalf("third run: %v", err) }
    })
    if strings.Contains(out, "already at block") {
        t.Errorf("a new -top should rebuild the list, got %q", out)
    }
    if addrs, _, _ := abandonedRows(t, opt.dbsqlite); len(addrs) != 1 {
        t.Errorf("abandoned = %v, want the one address the new -top allows", addrs)
    }
}

// It writes SQLite and nothing else, so it has nowhere to put the answer without
// being told where.
func TestAbaBuildNeedsADatabase(t *testing.T) {
    var srv = fakeCore(t, 3)
    var opt = abaOptions(t, srv.URL)
    opt.dbsqlite = ""
    if err := ababuild(opt); err == nil {
        t.Error("ababuild without -dbsqlite should say so, not write somewhere of its own choosing")
    }
}

// A timed shard has to hand back the dates as well as the amounts, since a date
// lost is an address ranked on the wrong day. The untimed round trip is
// TestShardsRoundTrip; this is the same guarantee for the wider record.
func TestTimedShardsRoundTrip(t *testing.T) {
    var dir = filepath.Join(t.TempDir(), "shards")
    var sh, err = newTimedShards(dir, 8, 4)
    if err != nil { t.Fatalf("newTimedShards: %v", err) }
    defer sh.remove()
    var want = map[string]move{}
    var scripts = []string{string(payScript), string(otherScript), strings.Repeat("x", 300)}
    for i, script := range scripts {
        for n := 0; n < 10; n++ {
            var sat = int64((i+1)*1000 - n*7)
            if n%3 == 0 { sat = -sat }
            // dates that rise and fall, so a reader taking the last one rather
            // than the latest would be caught
            var spent = int64(1231006505 + (n%4)*600)
            var paid = int64(1231006505 + (7-n%5)*600)
            var e = want[script]
            e.at(sat, spent, true)
            e.at(0, paid, false)
            want[script] = e
            if err := sh.putAt(script, sat, spent, paid); err != nil { t.Fatalf("put: %v", err) }
        }
    }
    if err := sh.flush(); err != nil { t.Fatalf("flush: %v", err) }
    var got = map[string]move{}
    for i := 0; i < 8; i++ {
        if err := sh.eachAt(i, func(script []byte, sat, spent, paid int64) error {
            var e = got[string(script)]
            e.at(sat, spent, true)
            e.at(0, paid, false)
            got[string(script)] = e
            return nil
        }); err != nil {
            t.Fatalf("eachAt %d: %v", i, err)
        }
    }
    if len(got) != len(want) { t.Fatalf("read back %d scripts, want %d", len(got), len(want)) }
    for script, m := range want {
        if got[script] != m {
            t.Errorf("script %.10q read back as %+v, want %+v", script, got[script], m)
        }
    }
}

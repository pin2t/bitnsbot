package addrstat

import "bytes"
import "context"
import "encoding/binary"
import "path/filepath"
import "testing"

import "database/sql"

import _ "modernc.org/sqlite"
import "bitnsbot/addrindex"
import "bitnsbot/cursors"
import "bitnsbot/signals"

// Two addresses the tests follow and one they do not, each built from a real
// script so the address and the script meet on the key the scan matches by.
var scriptA = append([]byte{0x00, 20}, bytes.Repeat([]byte{0xa1}, 20)...)
var scriptB = append([]byte{0x76, 0xa9, 20}, append(bytes.Repeat([]byte{0xb2}, 20), 0x88, 0xac)...)
var scriptC = append([]byte{0x51, 32}, bytes.Repeat([]byte{0xc3}, 32)...)

var addrA = addrindex.Address(scriptA)
var addrB = addrindex.Address(scriptB)
var addrC = addrindex.Address(scriptC)

// tx is one transaction's two sides: what it pays, and what it spends.
type tx struct {
    outs  []addrindex.Payment
    spent []addrindex.Payment
}

func pay(script []byte, sat int64) addrindex.Payment { return addrindex.Payment{Script: script, Sat: sat} }

// blockOf serializes a block and its spent-outputs blob the way Core's REST
// interface returns them, with the timestamp in the header where BlockTime
// reads it.
func blockOf(when int64, txs ...tx) addrindex.Block {
    var raw = new(bytes.Buffer)
    var header = make([]byte, 80)
    binary.LittleEndian.PutUint32(header[68:72], uint32(when))
    raw.Write(header)
    raw.WriteByte(byte(len(txs)))
    var spent = new(bytes.Buffer)
    spent.WriteByte(byte(len(txs)))
    for _, t := range txs {
        raw.Write(make([]byte, 4)) // version
        // one input, coinbase-shaped: a zero input count is the segwit marker on
        // the real wire, so even a fixture must carry one
        raw.WriteByte(1)
        raw.Write(make([]byte, 36))
        raw.WriteByte(0)
        raw.Write(make([]byte, 4))
        raw.WriteByte(byte(len(t.outs)))
        for _, o := range t.outs {
            var v = make([]byte, 8)
            binary.LittleEndian.PutUint64(v, uint64(o.Sat))
            raw.Write(v)
            raw.WriteByte(byte(len(o.Script)))
            raw.Write(o.Script)
        }
        raw.Write(make([]byte, 4)) // locktime
        spent.WriteByte(byte(len(t.spent)))
        for _, o := range t.spent {
            var v = make([]byte, 8)
            binary.LittleEndian.PutUint64(v, uint64(o.Sat))
            spent.Write(v)
            spent.WriteByte(byte(len(o.Script)))
            spent.Write(o.Script)
        }
    }
    return addrindex.Block{Hash: "fixture", Raw: raw.Bytes(), Spent: spent.Bytes()}
}

type fakeChain struct {
    tip     int
    blocks  map[int]addrindex.Block
    fetched []int
}

func (f *fakeChain) Tip(ctx context.Context) (int, error) { return f.tip, nil }

func (f *fakeChain) BlockAt(ctx context.Context, height int) (addrindex.Block, error) {
    f.fetched = append(f.fetched, height)
    return f.blocks[height], nil
}

// The two tables as openDB creates them, from the schema tools/tosqlite defines.
const ddl = `create table addrstat (addr TEXT PRIMARY KEY, type TEXT NOT NULL, balance INTEGER NOT NULL,
    recv INTEGER NOT NULL, sent INTEGER NOT NULL, flow INTEGER NOT NULL, fees INTEGER NOT NULL,
    txs INTEGER NOT NULL, first INTEGER NOT NULL, last INTEGER NOT NULL);
    create table cursors (name TEXT PRIMARY KEY, place INTEGER NOT NULL)`

// open puts a row under each address and runs Init over them, which is how the
// set is filled: the table's keys are the set, and inserting one is what adds an
// address to it.
func open(t *testing.T, addrs ...string) *sql.DB {
    t.Helper()
    var handle, err = sql.Open("sqlite", filepath.Join(t.TempDir(), "t.db"))
    if err != nil { t.Fatalf("open: %v", err) }
    if _, err := handle.Exec(ddl); err != nil { t.Fatal(err) }
    if err := cursors.Init(handle); err != nil { t.Fatalf("cursors: %v", err) }
    t.Cleanup(func() { handle.Close(); db = nil; watched = map[string]string{} })
    add(t, handle, addrs...)
    if err := Init(handle); err != nil { t.Fatalf("init: %v", err) }
    return handle
}

func add(t *testing.T, handle *sql.DB, addrs ...string) {
    t.Helper()
    for _, a := range addrs {
        var _, kind, _ = addrindex.Decode(a)
        var _, err = handle.Exec(`insert into addrstat (addr, type, balance, recv, sent, flow, fees,
            txs, first, last) values (?, ?, 0, 0, 0, 0, 0, 0, 0, 0)
            on conflict(addr) do nothing`, a, kind)
        if err != nil { t.Fatalf("add %s: %v", a, err) }
    }
}

func statOf(t *testing.T, addr string) Stat {
    t.Helper()
    var s Stat
    var err = db.QueryRow(`select type, balance, recv, sent, flow, fees, txs, first, last
        from addrstat where addr = ?`, addr).Scan(&s.Type, &s.Balance, &s.Recv, &s.Sent, &s.Flow,
        &s.Fees, &s.Txs, &s.First, &s.Last)
    if err != nil { t.Fatalf("no row for %s: %v", addr, err) }
    return s
}

// The chain pays addrA twice and then spends from it, paying part back to addrB
// and leaving the rest as a fee. Everything the record holds is asserted against
// what those blocks actually moved.
func TestCollectGathersStatistics(t *testing.T) {
    open(t, addrA, addrB)
    var src = &fakeChain{tip: 2, blocks: map[int]addrindex.Block{
        0: blockOf(1000, tx{outs: []addrindex.Payment{pay(scriptA, 5000)}}),
        1: blockOf(2000, tx{outs: []addrindex.Payment{pay(scriptA, 3000), pay(scriptC, 100)}}),
        // spends both of addrA's outputs, pays 7000 to addrB, so the fee is 1000
        2: blockOf(3000,
            tx{outs: []addrindex.Payment{pay(scriptC, 50)}},
            tx{outs: []addrindex.Payment{pay(scriptB, 7000)}, spent: []addrindex.Payment{pay(scriptA, 5000), pay(scriptA, 3000)}}),
    }}
    if err := Collect(src); err != nil { t.Fatalf("Collect: %v", err) }
    var a = statOf(t, addrA)
    if a.Recv != 8000 || a.Sent != 8000 || a.Balance != 0 || a.Flow != 16000 {
        t.Errorf("addrA amounts = %+v", a)
    }
    // the fee is charged once, though the transaction spends two of its outputs
    if a.Fees != 1000 { t.Errorf("addrA fees = %d, want 1000", a.Fees) }
    // three transactions: two paying it, one spending from it
    if a.Txs != 3 { t.Errorf("addrA txs = %d, want 3", a.Txs) }
    if a.First != 1000 || a.Last != 3000 { t.Errorf("addrA dates = %d..%d, want 1000..3000", a.First, a.Last) }
    if a.Type != "segwit" { t.Errorf("addrA type = %q, want segwit", a.Type) }
    var b = statOf(t, addrB)
    if b.Recv != 7000 || b.Sent != 0 || b.Balance != 7000 || b.Txs != 1 || b.Fees != 0 {
        t.Errorf("addrB = %+v", b)
    }
    if b.Type != "p2pkh" { t.Errorf("addrB type = %q, want p2pkh", b.Type) }
    // an address nobody asked about is not stored, which is the whole point of
    // gathering for a set rather than for the chain
    var n int
    if err := db.QueryRow("select count(*) from addrstat where addr = ?", addrC).Scan(&n); err != nil {
        t.Fatal(err)
    }
    if n != 0 { t.Error("an unwatched address was stored") }
}

// A transaction that both pays an address and spends from it is one transaction
// for it, not two.
func TestTransactionCountedOncePerAddress(t *testing.T) {
    open(t, addrA)
    var src = &fakeChain{tip: 0, blocks: map[int]addrindex.Block{
        0: blockOf(1000, tx{
            outs:  []addrindex.Payment{pay(scriptA, 400), pay(scriptA, 100)},
            spent: []addrindex.Payment{pay(scriptA, 900)},
        }),
    }}
    if err := Collect(src); err != nil { t.Fatalf("Collect: %v", err) }
    var a = statOf(t, addrA)
    if a.Txs != 1 { t.Errorf("txs = %d, want 1", a.Txs) }
    if a.Recv != 500 || a.Sent != 900 || a.Balance != -400 { t.Errorf("%+v", a) }
    if a.Fees != 400 { t.Errorf("fees = %d, want 400", a.Fees) }
}

// The scan resumes from its cursor: a second pass fetches only what is new and
// adds to what is stored rather than counting a block twice.
func TestCollectResumes(t *testing.T) {
    open(t, addrA)
    var src = &fakeChain{tip: 0, blocks: map[int]addrindex.Block{
        0: blockOf(1000, tx{outs: []addrindex.Payment{pay(scriptA, 500)}}),
        1: blockOf(2000, tx{outs: []addrindex.Payment{pay(scriptA, 700)}}),
    }}
    if err := Collect(src); err != nil { t.Fatalf("Collect: %v", err) }
    src.tip, src.fetched = 1, nil
    if err := Collect(src); err != nil { t.Fatalf("Collect: %v", err) }
    if len(src.fetched) != 1 || src.fetched[0] != 1 {
        t.Errorf("fetched %v on the second pass, want only block 1", src.fetched)
    }
    var a = statOf(t, addrA)
    if a.Recv != 1200 || a.Txs != 2 { t.Errorf("%+v, want 1200 over 2 transactions", a) }
    if a.First != 1000 || a.Last != 2000 { t.Errorf("dates = %d..%d", a.First, a.Last) }
    if h, ok := cursors.Get(cursors.AddrStat); !ok || h != 1 { t.Errorf("cursor = %d ok=%v, want 1", h, ok) }
}

// The three ranked address lists are built from these records, so a pass that
// moved them says so — and one that found nothing to do says nothing, or every
// idle tick would rebuild three indexes for no reason.
func TestCollectSignalsWhenItHasCaughtUp(t *testing.T) {
    open(t, addrA)
    signals.Reset()
    var wake = signals.Subscribe(signals.AddrStat)
    var src = &fakeChain{tip: 0, blocks: map[int]addrindex.Block{
        0: blockOf(1000, tx{outs: []addrindex.Payment{pay(scriptA, 500)}}),
    }}
    if err := Collect(src); err != nil { t.Fatalf("Collect: %v", err) }
    select {
    case <-wake:
    default:
        t.Error("a pass that scanned a block did not signal")
    }
    // nothing new: the cursor is at the tip, so there is nothing to announce
    if err := Collect(src); err != nil { t.Fatalf("Collect: %v", err) }
    select {
    case <-wake:
        t.Error("an idle pass signalled a rebuild")
    default:
    }
}

// A record with nothing gathered in it is not an answer: an address is in the
// set because somebody expects it to have a history, so the live path answers it
// until the scan has been past it.
func TestGetWaitsForSomethingToReport(t *testing.T) {
    open(t, addrA)
    if _, ok := Get(addrA); ok { t.Fatal("answered from an empty record") }
    var src = &fakeChain{tip: 0, blocks: map[int]addrindex.Block{
        0: blockOf(1000, tx{outs: []addrindex.Payment{pay(scriptA, 500)}}),
    }}
    if err := Collect(src); err != nil { t.Fatalf("Collect: %v", err) }
    var s, ok = Get(addrA)
    if !ok || s.Recv != 500 { t.Fatalf("Get = %+v ok=%v", s, ok) }
    if _, ok := Get(addrC); ok { t.Error("answered for an address nobody follows") }
}

// An address put into the bucket is in the set from the next Init, which is what
// makes the bucket itself the set — and the scan's place is left alone, since
// restarting the bot cannot mean rescanning the chain. Gathering that address's
// history means clearing the cursor by hand; nothing here does it.
func TestAnAddedRecordJoinsTheSet(t *testing.T) {
    var handle = open(t, addrA)
    var src = &fakeChain{tip: 0, blocks: map[int]addrindex.Block{
        0: blockOf(1000, tx{outs: []addrindex.Payment{pay(scriptA, 500)}}),
    }}
    if err := Collect(src); err != nil { t.Fatalf("Collect: %v", err) }
    add(t, handle, addrB)
    if err := Init(handle); err != nil { t.Fatalf("re-init: %v", err) }
    if Count() != 2 { t.Errorf("watched %d addresses, want 2", Count()) }
    if h, ok := cursors.Get(cursors.AddrStat); !ok || h != 0 { t.Errorf("cursor = %d ok=%v, want 0", h, ok) }
    if a := statOf(t, addrA); a.Recv != 500 { t.Errorf("the gathered record was disturbed: %+v", a) }
}

// Init is run on every start, so one must leave the scan's place alone.
func TestRepeatedInitKeepsTheCursor(t *testing.T) {
    var handle = open(t, addrA)
    var src = &fakeChain{tip: 0, blocks: map[int]addrindex.Block{
        0: blockOf(1000, tx{outs: []addrindex.Payment{pay(scriptA, 500)}}),
    }}
    if err := Collect(src); err != nil { t.Fatalf("Collect: %v", err) }
    for i := 0; i < 3; i++ {
        if err := Init(handle); err != nil { t.Fatalf("init %d: %v", i, err) }
    }
    if h, ok := cursors.Get(cursors.AddrStat); !ok || h != 0 { t.Errorf("cursor = %d ok=%v, want 0", h, ok) }
    if a := statOf(t, addrA); a.Recv != 500 { t.Errorf("a repeated Init changed the record: %+v", a) }
}

package addrbal

import "bytes"
import "crypto/sha256"
import "encoding/binary"
import "encoding/hex"
import "errors"
import "fmt"
import "path/filepath"
import "slices"
import "strconv"
import "sync"
import "testing"
import "database/sql"
import _ "modernc.org/sqlite"
import "bitnsbot/addrindex"
import "bitnsbot/core/coretest"
import "bitnsbot/cursors"
import "bitnsbot/signals"

// Scripts of each form an address can take, and two that are no address: a
// public key paid directly, which is its P2PKH address, and an OP_RETURN.
var scriptA = append([]byte{0x00, 20}, bytes.Repeat([]byte{0xa1}, 20)...)
var scriptB = append([]byte{0x76, 0xa9, 20}, append(bytes.Repeat([]byte{0xb2}, 20), 0x88, 0xac)...)
var scriptC = append([]byte{0x51, 32}, bytes.Repeat([]byte{0xc3}, 32)...)
var pubkey = append([]byte{0x02}, bytes.Repeat([]byte{0xd4}, 32)...)
var scriptPK = append(append([]byte{33}, pubkey...), 0xac)
var scriptPKH = append([]byte{0x76, 0xa9, 20}, append(addrindex.Hash160(pubkey), 0x88, 0xac)...)
var opReturn = []byte{0x6a, 0x04, 0xde, 0xad, 0xbe, 0xef}

func pay(script []byte, sat int64) addrindex.Payment { return addrindex.Payment{Script: script, Sat: sat} }

// chain is a node holding blocks, served over RPC the way a real one serves them.
// A height with no block is an empty one, every getblock is recorded in fetched,
// and a getblock for fail is refused. The blocks are fetched several at a time,
// so what is recorded is behind a lock.
type chain struct {
    mu      sync.Mutex
    tip     int
    blocks  map[int][]addrindex.Tx
    fetched []int
    fail    int
}

func (c *chain) respond(method string, params []interface{}) (interface{}, error) {
    switch method {
    case "getblockcount":
        return c.tip, nil
    case "getblockhash":
        return fmt.Sprint(params[0]), nil
    case "getblock":
        var height, _ = strconv.Atoi(params[0].(string))
        c.mu.Lock()
        c.fetched = append(c.fetched, height)
        c.mu.Unlock()
        if height == c.fail { return nil, errors.New("node is busy") }
        var amount = func(p addrindex.Payment) map[string]interface{} {
            return map[string]interface{}{"value": float64(p.Sat) / 1e8, "scriptPubKey": map[string]string{"hex": hex.EncodeToString(p.Script)}}
        }
        var txs = []interface{}{}
        for _, tx := range c.blocks[height] {
            var vin = []interface{}{}
            var vout = []interface{}{}
            for _, p := range tx.Spent { vin = append(vin, map[string]interface{}{"prevout": amount(p)}) }
            for _, p := range tx.Outputs { vout = append(vout, amount(p)) }
            txs = append(txs, map[string]interface{}{"vin": vin, "vout": vout})
        }
        return map[string]interface{}{"time": 1231006505 + height, "tx": txs}, nil
    }
    return nil, fmt.Errorf("unexpected method %s", method)
}

// open gives the package a fresh database and forgets the count, with the chunk
// and flush bounds restored when the test ends.
func open(t *testing.T, c *chain) {
    var handle, err = sql.Open("sqlite", "file:"+filepath.Join(t.TempDir(), "addrbal.db"))
    if err != nil { t.Fatal(err) }
    for _, s := range []string{
        "create table addrbal (shard INTEGER PRIMARY KEY, data BLOB NOT NULL)",
        "create table cursors (name TEXT PRIMARY KEY, place INTEGER NOT NULL)",
    } {
        if _, err := handle.Exec(s); err != nil { t.Fatal(err) }
    }
    cursors.Init(handle)
    Init(handle)
    count.Store(0)
    loaded.Store(false)
    caughtUp.Store(false)
    var chunk, keys = chunkSize, flushKeys
    t.Cleanup(func() {
        chunkSize, flushKeys = chunk, keys
        handle.Close()
    })
    coretest.Start(t, c.respond)
}

// balance reads what the table holds for a script's address.
func balance(t *testing.T, script []byte) (int64, bool) {
    var key, ok = addrindex.Key(script)
    if !ok { t.Fatalf("%x is no address", script) }
    var sum = sha256.Sum256([]byte(key))
    var hash = binary.BigEndian.Uint64(sum[:8])
    var data []byte
    if err := db.QueryRow("select data from addrbal where shard = ?", int64(hash>>remainderBits)).Scan(&data); err != nil { return 0, false }
    for i := 0; i+entryLen <= len(data); i += entryLen {
        if remainder(data[i:]) == hash&remainderMask { return int64(binary.BigEndian.Uint64(data[i+remainderLen:])), true }
    }
    return 0, false
}

func place(t *testing.T) int64 {
    var h, ok = cursors.Get(cursors.AddrBal)
    if !ok { t.Fatal("no cursor") }
    return h
}

// firstChain pays A and B, has A pay C with change back to itself and burn some
// to an OP_RETURN, then empties B into the P2PKH address of a key an earlier
// coinbase paid directly. Five blocks mined on top are inside the confirmations
// the scan leaves alone, and the sixth is not.
func firstChain() *chain {
    return &chain{tip: 3 + confirmations, fail: -1, blocks: map[int][]addrindex.Tx{
        0: {{Outputs: []addrindex.Payment{pay(scriptA, 50_0000_0000)}}},
        1: {{Outputs: []addrindex.Payment{pay(scriptB, 50_0000_0000)}},
            {Spent: []addrindex.Payment{pay(scriptA, 50_0000_0000)},
                Outputs: []addrindex.Payment{pay(scriptC, 30_0000_0000), pay(scriptA, 19_9999_0000), pay(opReturn, 1000)}}},
        2: {{Outputs: []addrindex.Payment{pay(scriptPK, 50_0000_0000)}}},
        3: {{Spent: []addrindex.Payment{pay(scriptB, 50_0000_0000)}, Outputs: []addrindex.Payment{pay(scriptPKH, 49_9999_0000)}}},
        4: {{Outputs: []addrindex.Payment{pay(scriptB, 1)}}},
    }}
}

// A pass counts the addresses holding something and nothing else: B, emptied,
// is gone; the OP_RETURN is no address; a key paid both directly and to its
// P2PKH address is one address holding both; and block 4, inside the
// confirmations, is not read.
func TestBuildCountsFundedAddresses(t *testing.T) {
    var c = firstChain()
    open(t, c)
    if err := Build(); err != nil { t.Fatal(err) }
    for _, want := range []struct {
        script []byte
        sat    int64
    }{{scriptA, 19_9999_0000}, {scriptC, 30_0000_0000}, {scriptPK, 99_9999_0000}, {scriptPKH, 99_9999_0000}} {
        if got, ok := balance(t, want.script); !ok || got != want.sat {
            t.Errorf("balance of %x = %d, %v; want %d", want.script, got, ok, want.sat)
        }
    }
    if got, ok := balance(t, scriptB); ok {
        t.Errorf("B holds %d after being emptied, want it removed", got)
    }
    if n, ok := Count(); n != 3 || !ok {
        t.Errorf("Count() = %d, %v; want 3, true", n, ok)
    }
    if got := place(t); got != 3 {
        t.Errorf("cursor = %d, want 3", got)
    }
    if slices.Sort(c.fetched); !slices.Equal(c.fetched, []int{0, 1, 2, 3}) {
        t.Errorf("fetched %v, want [0 1 2 3]", c.fetched)
    }
}

// Merging a block at a time, the way a pass does once flushKeys addresses have
// piled up, comes to the same balances as merging them all at once; a second
// pass reads only the new blocks; and a shard whose last address is emptied is
// deleted rather than kept as an empty row.
func TestBuildFlushesAndResumes(t *testing.T) {
    var c = firstChain()
    open(t, c)
    chunkSize, flushKeys = 2, 1
    if err := Build(); err != nil { t.Fatal(err) }
    if n, _ := Count(); n != 3 {
        t.Errorf("Count() = %d after flushing block by block, want 3", n)
    }
    c.tip += 2
    c.fetched = nil
    c.blocks[5] = []addrindex.Tx{{Spent: []addrindex.Payment{pay(scriptC, 30_0000_0000), pay(scriptA, 19_9999_0000), pay(scriptB, 1)},
        Outputs: []addrindex.Payment{pay(scriptPKH, 49_9998_0000)}}}
    if err := Build(); err != nil { t.Fatal(err) }
    if slices.Sort(c.fetched); !slices.Equal(c.fetched, []int{4, 5}) {
        t.Errorf("second pass fetched %v, want [4 5]", c.fetched)
    }
    if n, _ := Count(); n != 1 {
        t.Errorf("Count() = %d, want 1", n)
    }
    var rows int
    if err := db.QueryRow("select count(*) from addrbal").Scan(&rows); err != nil { t.Fatal(err) }
    if rows != 1 {
        t.Errorf("table holds %d rows for one funded address, want 1", rows)
    }
}

// A restart counts what the table holds, rather than starting from zero.
func TestBuildLoadsTheCount(t *testing.T) {
    open(t, firstChain())
    if err := Build(); err != nil { t.Fatal(err) }
    count.Store(0)
    loaded.Store(false)
    caughtUp.Store(false)
    if n, ok := Count(); n != 0 || ok {
        t.Fatalf("Count() = %d, %v before a pass, want 0, false", n, ok)
    }
    if err := Build(); err != nil { t.Fatal(err) }
    if n, ok := Count(); n != 3 || !ok {
        t.Errorf("Count() = %d, %v after a restart, want 3, true", n, ok)
    }
}

// A block the node will not serve stops the pass at the last merge: the cursor
// stays there, the count is what was merged, and the pass has not caught up.
func TestBuildStopsAtAFailedBlock(t *testing.T) {
    var c = firstChain()
    c.fail = 3
    open(t, c)
    chunkSize, flushKeys = 2, 1_000_000
    if err := Build(); err == nil { t.Fatal("Build succeeded past a block the node refused") }
    if got := place(t); got != 1 {
        t.Errorf("cursor = %d, want 1, the last chunk merged", got)
    }
    if n, ok := Count(); n != 3 || ok {
        t.Errorf("Count() = %d, %v; want 3 as of block 1, and not caught up", n, ok)
    }
}

// merge keeps a shard's entries sorted, adds an address new to it wherever it
// falls, moves a balance, and drops one that comes to zero.
func TestMergeKeepsEntriesSorted(t *testing.T) {
    var entry = func(rem uint64, sat int64) []byte {
        var e = binary.BigEndian.AppendUint32(binary.BigEndian.AppendUint16(nil, uint16(rem>>32)), uint32(rem))
        return binary.BigEndian.AppendUint64(e, uint64(sat))
    }
    var shard = uint64(0xabcd) << remainderBits
    var data = slices.Concat(entry(0x20, 500), entry(0x40, 700), entry(0x010000000000, 900))
    var deltas = map[uint64]int64{shard | 0x10: 100, shard | 0x20: -500, shard | 0x30: 300, shard | 0x40: 5, shard | 0xff0000000001: 1}
    var keys = []uint64{shard | 0x10, shard | 0x20, shard | 0x30, shard | 0x40, shard | 0xff0000000001}
    var got, change = merge(data, keys, deltas)
    var want = slices.Concat(entry(0x10, 100), entry(0x30, 300), entry(0x40, 705), entry(0x010000000000, 900), entry(0xff0000000001, 1))
    if !bytes.Equal(got, want) {
        t.Errorf("merged\n%x\nwant\n%x", got, want)
    }
    if change != 2 {
        t.Errorf("change = %d, want 2: three added, one removed", change)
    }
}

// The Blockchain card refreshes on signals.AddrBal, so a pass sends it when it
// first catches up — a restart with nothing new to read included — and when it
// merges a block, and an idle pass after that sends nothing.
func TestBuildSignalsTheCount(t *testing.T) {
    var c = firstChain()
    open(t, c)
    signals.Reset()
    var funded = signals.Subscribe(signals.AddrBal)
    var sent = func() bool {
        select {
        case <-funded:
            return true
        default:
            return false
        }
    }
    if err := Build(); err != nil { t.Fatal(err) }
    if !sent() { t.Error("no signal when the first pass caught up") }
    if err := Build(); err != nil { t.Fatal(err) }
    if sent() { t.Error("an idle pass sent a signal") }
    c.tip++
    if err := Build(); err != nil { t.Fatal(err) }
    if !sent() { t.Error("no signal after merging a block") }
    caughtUp.Store(false)
    if err := Build(); err != nil { t.Fatal(err) }
    if !sent() { t.Error("no signal when a restarted scan caught up with nothing to read") }
}

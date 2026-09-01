package main

import "encoding/binary"
import "testing"

import "bitnsbot/addrindex"

func key(script []byte) uint64 { return binary.BigEndian.Uint64(addrindex.Prefix(script)) }

func scriptN(n uint64) []byte {
    var s = make([]byte, 25)
    s[0], s[1], s[2], s[23], s[24] = 0x76, 0xa9, 20, 0x88, 0xac
    binary.BigEndian.PutUint64(s[3:], n)
    return s
}

// The count rises once per call and is readable back through the merge.
func TestCounterCounts(t *testing.T) {
    var c = newCounter(0, 2)
    for i := 0; i < 5; i++ { c.add(payScript) }
    c.add(otherScript)
    if got := c.count(key(payScript)); got != 5 {
        t.Errorf("count = %d, want 5", got)
    }
    if got := c.count(key(otherScript)); got != 1 {
        t.Errorf("count = %d, want 1", got)
    }
    if c.entries() != 2 { t.Errorf("entries = %d, want 2", c.entries()) }
    // and the same after folding the buffer into the run
    c.flush()
    if got := c.count(key(payScript)); got != 5 {
        t.Errorf("after flush count = %d, want 5", got)
    }
    if c.entries() != 2 { t.Errorf("after flush entries = %d, want 2", c.entries()) }
}

// Counts have to survive many merges, arriving out of order and interleaved,
// which is where a packed sorted run goes wrong.
func TestCounterMergesKeepCounts(t *testing.T) {
    var old = bufferedAddrs
    bufferedAddrs = 8
    defer func() { bufferedAddrs = old }()

    var c = newCounter(0, 1000000)
    var want = map[uint64]uint32{}
    var rnd uint64 = 99
    for i := 0; i < 3000; i++ {
        rnd = rnd*6364136223846793005 + 1442695040888963407
        var s = scriptN(rnd % 200)
        c.add(s)
        want[key(s)]++
    }
    c.flush()
    if c.entries() != len(want) {
        t.Fatalf("holds %d distinct prefixes, want %d", c.entries(), len(want))
    }
    for k, n := range want {
        if got := c.count(k); got != n {
            t.Errorf("count for %d = %d, want %d", k, got, n)
        }
    }
    // the run must come back sorted, or the binary search above was luck
    for i := 1; i < len(c.run)/entryLen; i++ {
        if c.prefixAt(i-1) >= c.prefixAt(i) {
            t.Fatalf("run is not sorted at %d", i)
        }
    }
}

// Three bytes hold 16777215. A count that reaches it stops rather than wrapping
// to nothing, which would turn the busiest address on the chain into a quiet one.
func TestCounterSaturates(t *testing.T) {
    var c = newCounter(0, 1)
    c.add(payScript)
    c.flush()
    c.setCountAt(0, maxCount-1)
    if got := c.add(payScript); got != maxCount {
        t.Fatalf("count = %d, want %d", got, maxCount)
    }
    if got := c.add(payScript); got != maxCount {
        t.Errorf("count wrapped to %d instead of holding at %d", got, maxCount)
    }
}

// Only the scripts past the threshold are kept, and they are kept as scripts —
// a prefix is a hash and cannot be turned back into an address.
func TestCounterKeepsQualifyingScripts(t *testing.T) {
    var c = newCounter(0, 2)
    c.add(payScript)
    c.add(payScript)
    if len(c.qualified) != 0 {
        t.Errorf("kept a script at the threshold; it must be past it")
    }
    c.add(payScript)
    if len(c.qualified) != 1 {
        t.Fatalf("kept %d scripts, want the one that crossed", len(c.qualified))
    }
    if got := c.qualified[key(payScript)]; string(got) != string(payScript) {
        t.Errorf("kept %x, want the script itself", got)
    }
    var active = c.active()
    if n, ok := active[string(payScript)]; !ok || n != 3 {
        t.Errorf("active = %v, want the script at 3", active)
    }
    if _, ok := active[string(otherScript)]; ok {
        t.Error("an address that never qualified was reported active")
    }
}

// Reserving room means the run never reallocates, which keeps the peak at the
// size of the set rather than twice it.
func TestCounterReserves(t *testing.T) {
    var c = newCounter(1000, 1000000)
    var before = cap(c.run)
    if before < 1000*entryLen {
        t.Fatalf("reserved %d bytes, want room for 1000 entries", before)
    }
    var old = bufferedAddrs
    bufferedAddrs = 4
    defer func() { bufferedAddrs = old }()
    for i := 0; i < 500; i++ { c.add(scriptN(uint64(i))) }
    c.flush()
    if cap(c.run) != before {
        t.Errorf("the run reallocated (%d to %d) despite the reservation", before, cap(c.run))
    }
}

// A record is eleven bytes: eight of prefix, three of count.
func TestCounterRecordLayout(t *testing.T) {
    if entryLen != prefixLen+countLen { t.Fatalf("entryLen %d", entryLen) }
    var c = newCounter(0, 1000000)
    c.add(payScript)
    c.flush()
    if len(c.run) != entryLen {
        t.Fatalf("one entry is %d bytes, want %d", len(c.run), entryLen)
    }
    if c.prefixAt(0) != key(payScript) {
        t.Error("the record does not lead with the index prefix")
    }
    if c.countAt(0) != 1 {
        t.Errorf("count = %d, want 1", c.countAt(0))
    }
}

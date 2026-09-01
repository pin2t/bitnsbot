package main

import "encoding/binary"
import "sort"

import "bitnsbot/addrindex"

// entryLen is one record of the counter: the script's 8-byte index prefix and a
// 3-byte count of the transactions it appeared in. Eleven bytes packed, because
// this holds an entry per address on the chain and every byte is a gigabyte and
// a half.
const entryLen = 11
const prefixLen = 8
const countLen = 3

// maxCount is what three bytes hold. A count that reaches it stops rising rather
// than wrapping, so a saturated figure reads as "at least this" instead of as a
// small number.
const maxCount = 1<<(8*countLen) - 1

// bufferedAddrs is how many new prefixes are held before being folded into the
// sorted run. Its cost is the one constant on top of the run's eleven bytes an
// entry — the buffer is a map, so it peaks at this many times ~40 bytes — and
// against that, each merge rewrites the whole run, so a bigger buffer means
// fewer passes over it. Deliberately not pre-sized: a hint would cost that peak
// from the start and again after every flush.
var bufferedAddrs = 1 << 24

// counter holds, for every script prefix the chain has paid to, how many
// transactions paid to it.
//
// The bulk is a flat sorted run of 11-byte records, searched by binary search
// and incremented in place. A map would be the obvious structure and is the
// wrong one at this scale: map[uint64]uint32 measured ~30 bytes an entry against
// this packed run's exact 11, which over ~1.3 B addresses is 39 GB against 14.
//
// New prefixes land in a small map and are merged in when it fills. Inserting
// each into the run as it arrives would shift everything after it, which over a
// billion additions is quadratic; the merge is one pass. A prefix already in the
// run is incremented there and never reaches the buffer, so the merge only ever
// inserts.
type counter struct {
    run []byte
    buf map[uint64]uint32
    // scripts of the prefixes that have crossed the threshold, captured at the
    // moment they cross. A prefix cannot be turned back into an address — it is
    // a hash — so the script has to be kept when it is in hand, and only for the
    // few that qualify.
    qualified map[uint64][]byte
    min       uint32
}

func newCounter(reserve int, min uint32) *counter {
    var c = &counter{buf: map[uint64]uint32{}, qualified: map[uint64][]byte{}, min: min}
    if reserve > 0 { c.run = make([]byte, 0, reserve*entryLen) }
    return c
}

func (c *counter) entries() int { return len(c.run)/entryLen + len(c.buf) }

// prefixAt and countAt read one record of the run.
func (c *counter) prefixAt(i int) uint64 { return binary.BigEndian.Uint64(c.run[i*entryLen:]) }

func (c *counter) countAt(i int) uint32 {
    var p = c.run[i*entryLen+prefixLen:]
    return uint32(p[0])<<16 | uint32(p[1])<<8 | uint32(p[2])
}

func (c *counter) setCountAt(i int, n uint32) {
    var p = c.run[i*entryLen+prefixLen:]
    p[0], p[1], p[2] = byte(n>>16), byte(n>>8), byte(n)
}

// add records that one transaction paid to this script, and reports the count it
// reached. The caller passes the script rather than the prefix because a prefix
// that crosses the threshold needs its script kept.
func (c *counter) add(script []byte) uint32 {
    var key = binary.BigEndian.Uint64(addrindex.Prefix(script))
    var n uint32
    var i = c.search(key)
    if i < len(c.run)/entryLen && c.prefixAt(i) == key {
        n = c.countAt(i)
        if n < maxCount {
            n++
            c.setCountAt(i, n)
        }
    } else {
        n = c.buf[key] + 1
        c.buf[key] = n
        if len(c.buf) >= bufferedAddrs { c.flush() }
    }
    // captured the first time it crosses, while the script is in hand
    if n > c.min {
        if _, ok := c.qualified[key]; !ok {
            c.qualified[key] = append([]byte(nil), script...)
        }
    }
    return n
}

func (c *counter) search(key uint64) int {
    return sort.Search(len(c.run)/entryLen, func(i int) bool { return c.prefixAt(i) >= key })
}

// count returns what the run and buffer together hold for a prefix.
func (c *counter) count(key uint64) uint32 {
    var i = c.search(key)
    if i < len(c.run)/entryLen && c.prefixAt(i) == key { return c.countAt(i) }
    return c.buf[key]
}

// flush folds the buffer into the sorted run in one merge, backwards so it can
// write in place whenever the slice already has the capacity — which keeps the
// peak at the size of the result rather than twice it. Reserve the capacity up
// front (-addrs) and it never reallocates at all.
func (c *counter) flush() {
    if len(c.buf) == 0 { return }
    var add = make([]uint64, 0, len(c.buf))
    for k := range c.buf { add = append(add, k) }
    sort.Slice(add, func(i, j int) bool { return add[i] < add[j] })
    var have = len(c.run) / entryLen
    var total = have + len(add)
    if cap(c.run) < total*entryLen {
        var grown = make([]byte, total*entryLen, (total+total/4)*entryLen)
        copy(grown, c.run)
        c.run = grown
    } else {
        c.run = c.run[:total*entryLen]
    }
    var a, b = have - 1, len(add) - 1
    for i := total - 1; i >= 0; i-- {
        if b >= 0 && (a < 0 || add[b] >= c.prefixAt(a)) {
            binary.BigEndian.PutUint64(c.run[i*entryLen:], add[b])
            c.setCountAt(i, c.buf[add[b]])
            b--
        } else {
            copy(c.run[i*entryLen:(i+1)*entryLen], c.run[a*entryLen:(a+1)*entryLen])
            a--
        }
    }
    c.buf = map[uint64]uint32{}
}

// active returns the scripts whose transaction count is past the threshold, with
// that count, once the whole chain has been read.
func (c *counter) active() map[string]uint32 {
    c.flush()
    var out = map[string]uint32{}
    for key, script := range c.qualified {
        if n := c.count(key); n > c.min { out[string(script)] = n }
    }
    return out
}

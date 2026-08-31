package main

import "encoding/binary"
import "sort"
import "fmt"
import "strconv"
import "time"

import "go.etcd.io/bbolt"
import "bitnsbot/addrindex"
import "bitnsbot/logging"

// activeBucket holds the addresses busy enough to be worth calling out: the key
// is the address, the value its transaction count at the time it qualified.
var activeBucket = []byte("addrindex-active")

// activeCursor is this pass's own place, kept in the index's cursor bucket
// beside the index's own so neither disturbs the other. It counts **block
// files**, not heights — a scan of the raw files has no cheap notion of height,
// and the name is deliberately not the old height-based one, so a cursor written
// by an earlier version cannot be read as a file number.
const activeCursor = "actbuild-file"

// activeMin is how many transactions an address needs before it counts as
// active. Lookup is asked for one more than this and no further: the question is
// only whether the history is longer, never how much longer.
var activeMin = 1000

// lookupLimit bounds the one lookup made per address. The same call both decides
// whether the address is active and counts its history, so it has to be high
// enough to be a real count for a busy address rather than a cap. actbuild
// overwrites it from -limit, whose default matches — raising only one of the two
// would leave the other in force.
var lookupLimit = 5000000

// bufferedAddrs is how many new prefixes are held before being folded into the
// sorted set. It is the one constant overhead on top of the slice's 8 bytes an
// entry: the buffer is a map, so it peaks at this many times ~30 bytes — 0.5 GB
// at the value below. Against that, each merge rewrites the whole set, so a
// bigger buffer means fewer passes over it. The map is deliberately not
// pre-sized: a hint would cost that peak from the start and again after every
// flush, rather than only while it is actually full.
var bufferedAddrs = 1 << 24

// processed is the set of script prefixes this run has already decided about,
// held in memory rather than in the database. An entry is the whole 8-byte index
// prefix packed into a uint64, so the set is exact and two scripts are only ever
// confused if the index itself would confuse them.
//
// The bulk is a **sorted slice**, searched by binary search. That is the whole
// reason for the shape: a map[uint64]struct{} measured 30.3 bytes an entry on
// Go 1.25 against the slice's exact 8.0, which over ~1.3 B addresses is 39 GB
// against 10 GB. Even so it beats a lookup in the index for the same question —
// that walks a shard's every range on disk, where this is ~30 comparisons in
// memory.
//
// New prefixes land in a small map and are merged in when it fills, rather than
// each being inserted into the big slice as it arrives: a single insert has to
// shift everything after it, which over a billion additions is quadratic. The
// merge is one pass, and membership meanwhile is the binary search plus a lookup
// in the buffer.
//
// It does not survive the run. A resumed scan therefore starts empty and looks
// up addresses it decided about last time, which costs work but changes no
// answer: the count it writes is the same either way.
type processed struct {
    sorted []uint64
    buf    map[uint64]struct{}
}

func newProcessed(reserve int) *processed {
    var p = &processed{buf: map[uint64]struct{}{}}
    if reserve > 0 { p.sorted = make([]uint64, 0, reserve) }
    return p
}

// take reports whether this script is new, and records it when it is.
func (p *processed) take(script []byte) bool {
    var key = binary.BigEndian.Uint64(addrindex.Prefix(script))
    if _, ok := p.buf[key]; ok { return false }
    var i = sort.Search(len(p.sorted), func(i int) bool { return p.sorted[i] >= key })
    if i < len(p.sorted) && p.sorted[i] == key { return false }
    p.buf[key] = struct{}{}
    if len(p.buf) >= bufferedAddrs { p.flush() }
    return true
}

// flush folds the buffer into the sorted set in one merge. It runs backwards so
// it can write in place whenever the slice already has the capacity, which is
// what keeps the peak at the size of the result rather than twice it — reserve
// the capacity up front (-addrs) and it never reallocates at all.
func (p *processed) flush() {
    if len(p.buf) == 0 { return }
    var add = make([]uint64, 0, len(p.buf))
    for k := range p.buf { add = append(add, k) }
    sort.Slice(add, func(i, j int) bool { return add[i] < add[j] })
    var total = len(p.sorted) + len(add)
    if cap(p.sorted) < total {
        var grown = make([]uint64, total, total+total/4)
        copy(grown, p.sorted)
        p.sorted = grown
    } else {
        p.sorted = p.sorted[:total]
    }
    // a and b never run ahead of the write position, so nothing unread is
    // overwritten
    var a, b = total - len(add) - 1, len(add) - 1
    for i := total - 1; i >= 0; i-- {
        if b >= 0 && (a < 0 || add[b] >= p.sorted[a]) {
            p.sorted[i] = add[b]
            b--
        } else {
            p.sorted[i] = p.sorted[a]
            a--
        }
    }
    p.buf = map[uint64]struct{}{}
}

func (p *processed) len() int { return len(p.sorted) + len(p.buf) }

// actbuild walks Core's raw block files and, for every script it has not decided
// about before, asks the index how many transactions that address has. Anything
// past activeMin is written to addrindex-active.
//
// It reads the files rather than Core's REST interface: the whole point is one
// sequential pass over local data instead of a request per block. It needs no
// undo data either — see addrindex.OutputScripts for why the outputs alone see
// every address.
func actbuild(opt *options) {
    if opt.blocks == "" {
        logging.Fatal("actbuild needs -blocks pointing at Core's blocks directory")
    }
    var files, err = blockFiles(opt.blocks)
    if err != nil { logging.Fatal("%v", err) }
    var key, kerr = xorKey(opt.blocks)
    if kerr != nil { logging.Fatal("read xor.dat: %v", kerr) }
    if _, ok := addrindex.Cursor(); !ok {
        logging.Fatal("the address index is empty — run addrindex build first")
    }
    if err := ensureBuckets(); err != nil { logging.Fatal("create buckets: %v", err) }

    var from = 0
    if n, ok := addrindex.GetCursor(activeCursor); ok { from = n + 1 }
    if from >= len(files) {
        fmt.Printf("Every block file has been scanned (%d of %d)\n", from, len(files))
        return
    }
    fmt.Printf("Scanning %s files %d..%d for addresses with more than %d transactions\n",
        opt.blocks, from, len(files)-1, activeMin)
    var started = time.Now()
    var first = from
    var seenScripts = newProcessed(opt.addrs)
    var looked, found int
    for i := from; i < len(files); i++ {
        var c, serr = scanFile(files[i], key, seenScripts)
        if serr != nil {
            logging.Err("actbuild: %v", serr)
            break
        }
        if err := flushActive(c, i); err != nil {
            logging.Err("actbuild: write file %d: %v", i, err)
            break
        }
        looked += c.fresh
        found += len(c.active)
        logging.Info("actbuild: file %d of %d: %d blocks, %d scripts, %d new addresses, %d active so far, %s",
            i, len(files)-1, c.blocks, c.scripts, c.fresh, found,
            progress(started, first, i, len(files)-1, looked))
    }
    var at, _ = addrindex.GetCursor(activeCursor)
    var elapsed = time.Since(started)
    fmt.Printf("Scanned through file %d in %s: %s addresses looked up (%s addr/sec), %d active\n",
        at, took(elapsed), group(int64(looked)), group(rate(looked, elapsed)), found)
}

// scanFile reads one blk file end to end and decides about every script in it
// that this run has not seen before.
func scanFile(name string, key []byte, seenScripts *processed) (*chunk, error) {
    var r, err = openBlockFile(name, key)
    if err != nil { return nil, err }
    defer r.Close()
    var c = &chunk{active: map[string]int{}}
    for {
        var raw, rerr = r.next()
        if rerr != nil { return nil, fmt.Errorf("%s: %w", name, rerr) }
        if raw == nil { break }
        c.blocks++
        var scripts, ok = addrindex.OutputScripts(raw)
        if !ok {
            logging.Warn("actbuild: could not parse a block in %s", name)
            continue
        }
        c.scripts += len(scripts)
        for _, script := range scripts {
            if !seenScripts.take(script) { continue }
            c.fresh++
            c.classify(script)
        }
    }
    return c, nil
}

// progress reports how fast the scan is going and how much longer it has. The
// rate is measured over the whole run rather than the last chunk: the address
// rate falls away as the processed set fills up, and a per-chunk figure would
// swing with it. The estimate is off blocks, which is what actually measures the
// distance to the tip.
func progress(started time.Time, first, done, tip, addrs int) string {
    var elapsed = time.Since(started)
    if elapsed <= 0 { return "" }
    var out = fmt.Sprintf("%s addr/sec", group(rate(addrs, elapsed)))
    var blocks = done - first + 1
    if blocks > 0 && done < tip {
        var perBlock = elapsed / time.Duration(blocks)
        out += ", ETA " + took(time.Duration(tip-done)*perBlock)
    }
    return out
}

// rate is a per-second figure, rounded, for a count over an elapsed time.
func rate(n int, elapsed time.Duration) int64 {
    if elapsed <= 0 { return 0 }
    return int64(float64(n)/elapsed.Seconds() + 0.5)
}

// group renders a count with spaces between thousands, the way every other
// figure this repo prints reads.
func group(n int64) string {
    var digits = strconv.FormatInt(n, 10)
    var out []byte
    for i := range digits {
        if i > 0 && (len(digits)-i)%3 == 0 { out = append(out, ' ') }
        out = append(out, digits[i])
    }
    return string(out)
}

// chunk is what one range of blocks accumulates: the remainders to add to the
// processed set, grouped by shard and kept sorted, and the active addresses
// found. Nothing else is held — in particular not the scripts themselves, which
// at 2000 blocks number several million and were what made an earlier version
// of this need 8 GB.
type chunk struct {
    active map[string]int
    blocks int
    scripts int
    fresh   int
}


// classify asks the index how long a script's history is and queues the ones
// past the threshold for resolving.
//
// One lookup, not two. An earlier version asked for activeMin+1 first and only
// then for the real count, on the theory that stopping early was cheaper — it is
// not: Lookup walks the whole shard whichever limit it is given, and the limit
// only bites for an address that actually exceeds it. So the second call
// repeated the entire first one.
// One lookup, not two. An earlier version asked for activeMin+1 first and only
// then for the real count, on the theory that stopping early was cheaper — it is
// not: Lookup walks the whole shard whichever limit it is given, and the limit
// only bites for an address that actually exceeds it.
//
// The address is encoded here rather than asked of the node. That was the last
// thing tying this pass to Core's RPC interface, and it was a round trip per
// qualifying script; see address.go.
func (c *chunk) classify(script []byte) {
    var touches, capped = addrindex.Lookup(script, lookupLimit)
    if len(touches) <= activeMin { return }
    if capped {
        logging.Warn("actbuild: a script has more than -limit %d transactions; recording the cap", lookupLimit)
    }
    // a nonstandard script is not an address, so there is nothing to record
    if addr := scriptAddress(script); addr != "" { c.active[addr] = len(touches) }
}


// flushActive writes a chunk's findings and advances the cursor in one
// transaction, so an interrupted scan resumes from the last chunk that landed.
func flushActive(c *chunk, file int) error {
    return db.Update(func(tx *bbolt.Tx) error {
        var a = tx.Bucket(activeBucket)
        for addr, count := range c.active {
            var v = make([]byte, 8)
            binary.BigEndian.PutUint64(v, uint64(count))
            if err := a.Put([]byte(addr), v); err != nil { return err }
        }
        return addrindex.SetCursorIn(tx, activeCursor, file)
    })
}

func ensureBuckets() error {
    return db.Update(func(tx *bbolt.Tx) error {
        var _, err = tx.CreateBucketIfNotExists(activeBucket)
        return err
    })
}

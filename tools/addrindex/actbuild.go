package main

import "bytes"
import "context"
import "encoding/binary"
import "fmt"
import "sort"
import "strconv"
import "time"

import "go.etcd.io/bbolt"
import "bitnsbot/addrindex"
import "bitnsbot/logging"

// activeBucket holds the addresses busy enough to be worth calling out: the key
// is the address, the value its transaction count at the time it qualified.
var activeBucket = []byte("addrindex-active")

// processedBucket is the set of scripts actbuild has already decided about,
// sharded exactly as the index itself is: the key is the first 2 bytes of the
// script's index prefix and the value the packed run of 6-byte remainders
// falling in that shard. Same reason, too — one bbolt key per address costs far
// more in page overhead than the entry itself, which a 2-byte split avoids by
// keeping the whole set in 65 536 keys.
//
// The run is kept **sorted**, so membership is a binary search rather than a
// walk — at full chain scale a shard holds tens of thousands of entries, where
// that is ~15 comparisons against ~23 000. Appending still rewrites the whole
// value, which is what the chunk size trades against.
var processedBucket = []byte("processed")

const shardLen = 2
const remainderLen = 6

// activeCursor is this pass's own place in the chain, kept in the index's cursor
// bucket beside the index's own, so neither disturbs the other.
const activeCursor = "actbuild"

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

// decodeBatch is how many scripts are resolved to addresses in one JSON-RPC
// call. Only the addresses past the threshold get here, so at the default
// -active this is rarely full — it earns its keep when the threshold is low
// enough that most scripts qualify.
var decodeBatch = 1000

// actChunk is how many blocks are scanned before the batch is written and the
// cursor advanced, so a crash mid-scan resumes from the last flushed chunk.
//
// Bigger is better for writes and worse for memory: every flush rewrites each
// shard it touches, and with 65 536 shards a chunk touches nearly all of them,
// so the whole bucket is rewritten once per chunk. Against that, a chunk holds
// every distinct script it saw. See CLAUDE.md for what 2000 measured.
var actChunk = 2000

// shardKey splits a script's index prefix into the two halves the processed set
// is keyed by.
func shardKey(script []byte) (shard, remainder []byte) {
    var p = addrindex.Prefix(script)
    return p[:shardLen], p[shardLen : shardLen+remainderLen]
}

// nth returns the i'th remainder of a shard's run.
func nth(shard []byte, i int) []byte { return shard[i*remainderLen : (i+1)*remainderLen] }

// seen reports whether a remainder is recorded in a shard's run. The run is
// sorted, so this is a binary search — the one thing that keeps membership cheap
// once a shard holds tens of thousands of entries.
func seen(shard, remainder []byte) bool {
    var n = len(shard) / remainderLen
    var i = sort.Search(n, func(i int) bool { return bytes.Compare(nth(shard, i), remainder) >= 0 })
    return i < n && bytes.Equal(nth(shard, i), remainder)
}

// insertOne adds a remainder in the place that keeps the run sorted, which is
// what lets seen binary-search the chunk's own additions as well as the stored
// ones. A shard collects a few hundred entries within a chunk, so shifting the
// tail is a short move.
func insertOne(run []byte, rem []byte) []byte {
    var n = len(run) / remainderLen
    var i = sort.Search(n, func(i int) bool { return bytes.Compare(nth(run, i), rem) >= 0 })
    if i < n && bytes.Equal(nth(run, i), rem) { return run }
    run = append(run, make([]byte, remainderLen)...)
    copy(run[(i+1)*remainderLen:], run[i*remainderLen:n*remainderLen])
    copy(run[i*remainderLen:], rem)
    return run
}

// merge combines two sorted runs into one, which is how a chunk's additions
// reach the stored shard. One pass rather than an insert per entry: a flush
// stays linear in the shard's size where repeated single inserts would be
// quadratic. A remainder present in both is kept once, or a re-scan would
// double every entry.
func merge(a, b []byte) []byte {
    var out = make([]byte, 0, len(a)+len(b))
    var i, j = 0, 0
    var na, nb = len(a) / remainderLen, len(b) / remainderLen
    for i < na && j < nb {
        switch bytes.Compare(nth(a, i), nth(b, j)) {
        case 0:
            out = append(out, nth(a, i)...)
            i, j = i+1, j+1
        case -1:
            out = append(out, nth(a, i)...)
            i++
        default:
            out = append(out, nth(b, j)...)
            j++
        }
    }
    out = append(out, a[i*remainderLen:]...)
    return append(out, b[j*remainderLen:]...)
}

// actbuild walks the chain from its own cursor to the tip and, for every script
// it has not decided about before, asks the index how many transactions that
// address has. Anything past activeMin is written to addrindex-active.
//
// The processed set is what makes this affordable: the index lookup is the
// expensive part, and the overwhelming majority of scripts are seen again and
// again as the scan goes on.
func actbuild(opt *options) {
    var src = addrindex.NewREST(opt.url)
    var ctx = context.Background()
    var probe, cancel = context.WithTimeout(ctx, 30*time.Second)
    var tip, err = src.Tip(probe)
    cancel()
    if err != nil {
        logging.Fatal("Core REST is unreachable at %s (%v) — enable -rest=1", opt.url, err)
    }
    var client, rerr = newRPC(opt.url, opt.user, opt.pass, opt.cookie)
    if rerr != nil { logging.Fatal("RPC client: %v", rerr) }
    if _, ok := addrindex.Cursor(); !ok {
        logging.Fatal("the address index is empty — run addrindex build first")
    }
    if err := ensureBuckets(); err != nil { logging.Fatal("create buckets: %v", err) }

    var from = 0
    if h, ok := addrindex.GetCursor(activeCursor); ok { from = h + 1 }
    if from > tip {
        fmt.Printf("Active addresses are already up to the tip (block %d)\n", tip)
        return
    }
    fmt.Printf("Scanning blocks %d..%d for addresses with more than %d transactions\n", from, tip, activeMin)
    var started = time.Now()
    var first = from
    var looked, found int
    for from <= tip {
        var to = from + actChunk - 1
        if to > tip { to = tip }
        var c, serr = scanChunk(ctx, src, client, from, to)
        if serr != nil {
            logging.Err("actbuild: %v", serr)
            break
        }
        if err := flushActive(c, to); err != nil {
            logging.Err("actbuild: write blocks %d..%d: %v", from, to, err)
            break
        }
        looked += c.fresh
        found += len(c.active)
        logging.Info("actbuild: blocks %d..%d of %d: %d scripts, %d new addresses, %d active so far, %s",
            from, to, tip, c.scripts, c.fresh, found, progress(started, first, to, tip, looked))
        from = to + 1
    }
    var at, _ = addrindex.GetCursor(activeCursor)
    var elapsed = time.Since(started)
    fmt.Printf("Scanned to block %d in %s: %s addresses looked up (%s addr/sec), %d active\n",
        at, took(elapsed), group(int64(looked)), group(rate(looked, elapsed)), found)
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
    pending map[string][]byte
    active  map[string]int
    // qualified but not yet resolved to an address: decodescript is sent in
    // batches, so a script waits here until decodeBatch of them accumulate
    waiting []pending
    scripts int
    fresh   int
}

// pending is a script that passed the threshold, with the count that got it
// there, waiting for the batch that turns it into an address.
type pending struct {
    script []byte
    count  int
}

// scanChunk walks a range of blocks and decides about every script in it that
// has not been decided about before.
//
// It works a block at a time rather than collecting the range first: the whole
// range's distinct scripts do not fit comfortably in memory, and nothing needs
// them all at once. The processed check runs in a short read transaction per
// block, and the index lookups run outside it — a Lookup opens its own read
// transaction, and nesting one inside another risks deadlocking against a
// waiting writer.
func scanChunk(ctx context.Context, src *addrindex.REST, client *rpc, from, to int) (*chunk, error) {
    var c = &chunk{pending: map[string][]byte{}, active: map[string]int{}}
    for h := from; h <= to; h++ {
        var blk, err = src.BlockAt(ctx, h)
        if err != nil { return nil, fmt.Errorf("block %d: %w", h, err) }
        var scripts, ok = addrindex.Scripts(blk)
        if !ok {
            logging.Warn("actbuild: could not parse block %d", h)
            continue
        }
        c.scripts += len(scripts)
        var fresh, ferr = c.take(scripts)
        if ferr != nil { return nil, ferr }
        c.fresh += len(fresh)
        for _, script := range fresh {
            c.classify(script)
        }
        if len(c.waiting) >= decodeBatch {
            if err := c.resolve(ctx, client); err != nil { return nil, err }
        }
    }
    if err := c.resolve(ctx, client); err != nil { return nil, err }
    return c, nil
}

// take records the scripts this chunk has not decided about yet and returns
// them, in one read transaction for the block.
func (c *chunk) take(scripts [][]byte) ([][]byte, error) {
    var fresh [][]byte
    var err = db.View(func(tx *bbolt.Tx) error {
        var p = tx.Bucket(processedBucket)
        for _, s := range scripts {
            var prefix = addrindex.Prefix(s)
            var shard, rem = string(prefix[:shardLen]), prefix[shardLen:]
            // Both runs are sorted, so both are searched the same way — the
            // chunk's own is kept in order as it grows rather than appended to.
            if seen(p.Get(prefix[:shardLen]), rem) || seen(c.pending[shard], rem) { continue }
            c.pending[shard] = insertOne(c.pending[shard], rem)
            fresh = append(fresh, s)
        }
        return nil
    })
    return fresh, err
}

// classify asks the index how long a script's history is and queues the ones
// past the threshold for resolving.
//
// One lookup, not two. An earlier version asked for activeMin+1 first and only
// then for the real count, on the theory that stopping early was cheaper — it is
// not: Lookup walks the whole shard whichever limit it is given, and the limit
// only bites for an address that actually exceeds it. So the second call
// repeated the entire first one.
func (c *chunk) classify(script []byte) {
    var touches, capped = addrindex.Lookup(script, lookupLimit)
    if len(touches) <= activeMin { return }
    if capped {
        logging.Warn("actbuild: a script has more than -limit %d transactions; recording the cap", lookupLimit)
    }
    c.waiting = append(c.waiting, pending{script: script, count: len(touches)})
}

// resolve turns the queued scripts into addresses in one JSON-RPC call per
// decodeBatch, rather than a round trip each.
func (c *chunk) resolve(ctx context.Context, client *rpc) error {
    for len(c.waiting) > 0 {
        var n = len(c.waiting)
        if n > decodeBatch { n = decodeBatch }
        var group = c.waiting[:n]
        var scripts = make([][]byte, 0, n)
        for _, p := range group { scripts = append(scripts, p.script) }
        var addrs, err = client.addressesOf(ctx, scripts)
        if err != nil { return fmt.Errorf("decode %d scripts: %w", n, err) }
        for i, addr := range addrs {
            // a nonstandard script is not an address, so there is nothing to
            // record for it
            if addr == "" { continue }
            c.active[addr] = group[i].count
        }
        c.waiting = c.waiting[n:]
    }
    return nil
}

// flushActive writes a chunk's findings and advances the cursor in one
// transaction, so an interrupted scan resumes from the last chunk that landed.
func flushActive(c *chunk, height int) error {
    return db.Update(func(tx *bbolt.Tx) error {
        var p = tx.Bucket(processedBucket)
        // Each shard is rewritten once, its whole batch merged into place, so
        // the stored run comes back sorted.
        for shard, remainders := range c.pending {
            if err := p.Put([]byte(shard), merge(p.Get([]byte(shard)), remainders)); err != nil {
                return err
            }
        }
        var a = tx.Bucket(activeBucket)
        for addr, count := range c.active {
            var v = make([]byte, 8)
            binary.BigEndian.PutUint64(v, uint64(count))
            if err := a.Put([]byte(addr), v); err != nil { return err }
        }
        return addrindex.SetCursorIn(tx, activeCursor, height)
    })
}

func ensureBuckets() error {
    return db.Update(func(tx *bbolt.Tx) error {
        for _, name := range [][]byte{activeBucket, processedBucket} {
            if _, err := tx.CreateBucketIfNotExists(name); err != nil { return err }
        }
        return nil
    })
}

package main

import "bytes"
import "context"
import "encoding/binary"
import "encoding/hex"
import "fmt"
import "sort"
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

// insert adds remainders to a shard's run, each in the place that keeps the run
// sorted. The batch is merged in one pass rather than inserted one at a time —
// the same result for a fraction of the work, since a flush then stays linear in
// the shard's size where repeated single inserts would be quadratic.
func insert(shard []byte, remainders [][]byte) []byte {
    sort.Slice(remainders, func(i, j int) bool {
        return bytes.Compare(remainders[i], remainders[j]) < 0
    })
    var out = make([]byte, 0, len(shard)+len(remainders)*remainderLen)
    var i, n = 0, len(shard)/remainderLen
    var prev []byte
    for _, rem := range remainders {
        // the caller deduplicates within a chunk, but a repeat here would
        // silently double an entry and nothing downstream would catch it
        if prev != nil && bytes.Equal(prev, rem) { continue }
        prev = rem
        for i < n && bytes.Compare(nth(shard, i), rem) < 0 {
            out = append(out, nth(shard, i)...)
            i++
        }
        if i < n && bytes.Equal(nth(shard, i), rem) { continue }
        out = append(out, rem...)
    }
    return append(out, shard[i*remainderLen:]...)
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
    var looked, found int
    for from <= tip {
        var to = from + actChunk - 1
        if to > tip { to = tip }
        var scripts, serr = chunkScripts(ctx, src, from, to)
        if serr != nil {
            logging.Err("actbuild: %v", serr)
            break
        }
        var fresh, ferr = unprocessed(scripts)
        if ferr != nil {
            logging.Err("actbuild: %v", ferr)
            break
        }
        var active = classify(ctx, client, fresh, opt.limit)
        if err := flushActive(fresh, active, to); err != nil {
            logging.Err("actbuild: write blocks %d..%d: %v", from, to, err)
            break
        }
        looked += len(fresh)
        found += len(active)
        logging.Info("actbuild: scanned blocks %d..%d (tip %d): %d scripts, %d new addresses, %d active so far",
            from, to, tip, len(scripts), len(fresh), found)
        from = to + 1
    }
    var at, _ = addrindex.GetCursor(activeCursor)
    fmt.Printf("Scanned to block %d in %s: %d addresses looked up, %d active\n",
        at, took(time.Since(started)), looked, found)
}

// chunkScripts reads a range of blocks and returns the distinct scripts in them.
// Deduplicating here is what keeps the range's cost proportional to the addresses
// it touches rather than to its outputs.
func chunkScripts(ctx context.Context, src *addrindex.REST, from, to int) ([][]byte, error) {
    var seenScript = map[string]bool{}
    var out [][]byte
    for h := from; h <= to; h++ {
        var blk, err = src.BlockAt(ctx, h)
        if err != nil { return nil, fmt.Errorf("block %d: %w", h, err) }
        var scripts, ok = addrindex.Scripts(blk)
        if !ok {
            logging.Warn("actbuild: could not parse block %d", h)
            continue
        }
        for _, s := range scripts {
            if seenScript[string(s)] { continue }
            seenScript[string(s)] = true
            out = append(out, s)
        }
    }
    return out, nil
}

// unprocessed filters out the scripts already recorded in the processed set, in
// one read transaction rather than one per script.
func unprocessed(scripts [][]byte) ([][]byte, error) {
    var out [][]byte
    // What this chunk has already taken, held as a plain set: the run on disk is
    // sorted and binary-searched, which the chunk's own additions are not until
    // they are merged into it at flush.
    var pending = map[string]bool{}
    var err = db.View(func(tx *bbolt.Tx) error {
        var p = tx.Bucket(processedBucket)
        for _, s := range scripts {
            var prefix = addrindex.Prefix(s)
            if pending[string(prefix)] { continue }
            if seen(p.Get(prefix[:shardLen]), prefix[shardLen:]) { continue }
            pending[string(prefix)] = true
            out = append(out, s)
        }
        return nil
    })
    return out, err
}

// classify asks the index how long each script's history is and resolves the
// ones past the threshold to an address. Only those few cost an RPC.
func classify(ctx context.Context, client *rpc, scripts [][]byte, limit int) map[string]int {
    var active = map[string]int{}
    for _, script := range scripts {
        // Deciding needs one lookup past the threshold and no further: the
        // question is whether the history is longer, never how much longer, and
        // this runs against every address on the chain.
        if touches, _ := addrindex.Lookup(script, activeMin+1); len(touches) <= activeMin { continue }
        var addr, err = client.addressOf(ctx, script)
        if err != nil {
            logging.Warn("actbuild: decode %s: %v", hex.EncodeToString(script), err)
            continue
        }
        // a nonstandard script is not an address, so there is nothing to record
        if addr == "" { continue }
        // Only now, for the few that qualified, is the real count worth reading.
        // The deciding lookup stopped at the threshold, so its length would be
        // the threshold and not a count at all.
        var touches, capped = addrindex.Lookup(script, limit)
        if capped {
            logging.Warn("actbuild: %s has more than -limit %d transactions; recording the cap", addr, limit)
        }
        active[addr] = len(touches)
    }
    return active
}

// flushActive writes a chunk's findings and advances the cursor in one
// transaction, so an interrupted scan resumes from the last chunk that landed.
func flushActive(processed [][]byte, active map[string]int, height int) error {
    return db.Update(func(tx *bbolt.Tx) error {
        var p = tx.Bucket(processedBucket)
        // Grouped by shard so each one is rewritten once, with its whole batch
        // merged into place — the run comes back sorted.
        var pending = map[string][][]byte{}
        for _, s := range processed {
            var shard, rem = shardKey(s)
            pending[string(shard)] = append(pending[string(shard)], rem)
        }
        for shard, remainders := range pending {
            if err := p.Put([]byte(shard), insert(p.Get([]byte(shard)), remainders)); err != nil {
                return err
            }
        }
        var a = tx.Bucket(activeBucket)
        for addr, count := range active {
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

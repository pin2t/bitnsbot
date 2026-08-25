package main

import "context"
import "encoding/binary"
import "encoding/hex"
import "fmt"
import "time"

import "go.etcd.io/bbolt"
import "bitnsbot/addrindex"
import "bitnsbot/logging"

// activeBucket holds the addresses busy enough to be worth calling out: the key
// is the address, the value its transaction count at the time it qualified.
var activeBucket = []byte("addrindex-active")

// processedBucket is the set of scripts actbuild has already decided about,
// sharded the way the index itself is and for the same reason: one bbolt key per
// address costs far more in page overhead than the entry itself. The key is the
// first 4 bytes of the script's index prefix, the value the packed run of 4-byte
// remainders falling in that shard.
//
// The split is wider than the index's own 2 bytes, which spreads addresses over
// 4 billion shards — so in practice this is close to one key per address, the
// shape the index measured and rejected. Worth re-measuring against a real chain
// before trusting any size estimate for it.
var processedBucket = []byte("processed")

const shardLen = 4
const remainderLen = 4

// activeCursor is this pass's own place in the chain, kept in the index's cursor
// bucket beside the index's own, so neither disturbs the other.
const activeCursor = "actbuild"

// activeMin is how many transactions an address needs before it counts as
// active. Lookup is asked for one more than this and no further: the question is
// only whether the history is longer, never how much longer.
var activeMin = 1000

// actChunk is how many blocks are scanned before the batch is written and the
// cursor advanced, so a crash mid-scan resumes from the last flushed chunk. It
// is smaller than the index's own chunk because a chunk holds every distinct
// script it saw, which is a heavier thing to carry than the index's touches.
var actChunk = 100

// shardKey splits a script's index prefix into the two halves the processed set
// is keyed by.
func shardKey(script []byte) (shard, remainder []byte) {
    var p = addrindex.Prefix(script)
    return p[:shardLen], p[shardLen : shardLen+remainderLen]
}

// seen reports whether a remainder is already recorded in a shard's packed run.
func seen(shard, remainder []byte) bool {
    for i := 0; i+remainderLen <= len(shard); i += remainderLen {
        if string(shard[i:i+remainderLen]) == string(remainder) { return true }
    }
    return false
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
    var err = db.View(func(tx *bbolt.Tx) error {
        var p = tx.Bucket(processedBucket)
        // A shard may gain several remainders within this one chunk, so the
        // pending additions are tracked alongside what is already on disk.
        var pending = map[string][]byte{}
        for _, s := range scripts {
            var shard, rem = shardKey(s)
            if seen(p.Get(shard), rem) || seen(pending[string(shard)], rem) { continue }
            pending[string(shard)] = append(pending[string(shard)], rem...)
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
        var pending = map[string][]byte{}
        for _, s := range processed {
            var shard, rem = shardKey(s)
            pending[string(shard)] = append(pending[string(shard)], rem...)
        }
        for shard, remainders := range pending {
            var merged = append(append([]byte{}, p.Get([]byte(shard))...), remainders...)
            if err := p.Put([]byte(shard), merged); err != nil { return err }
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

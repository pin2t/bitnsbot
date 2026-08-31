package main

import "encoding/binary"
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

// processed is the set of script prefixes this run has already decided about,
// held in memory rather than in the database. An entry is the whole 8-byte index
// prefix packed into a uint64, so the set is exact and two scripts are only ever
// confused if the index itself would confuse them.
//
// A plain map, not the sorted run the database-backed version used: sorting was
// there to make a packed bbolt value searchable, and in memory it buys nothing.
//
// It does not survive the run. A resumed scan therefore starts empty and looks
// up addresses it decided about last time, which costs work but changes no
// answer: the count it writes is the same either way.
type processed map[uint64]struct{}

// take reports whether this script is new, and records it when it is.
func (p processed) take(script []byte) bool {
    var key = binary.BigEndian.Uint64(addrindex.Prefix(script))
    if _, ok := p[key]; ok { return false }
    p[key] = struct{}{}
    return true
}

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
    var seenScripts = processed{}
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
func scanFile(name string, key []byte, seenScripts processed) (*chunk, error) {
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

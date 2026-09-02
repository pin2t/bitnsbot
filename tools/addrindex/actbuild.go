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
// active.
var activeMin = 1000

// actbuild walks Core's raw block files and counts, for every address the chain
// pays to, how many transactions paid to it. Anything past activeMin is written
// to addrindex-active when the scan finishes.
//
// It reads nothing but the files. An earlier version asked the index for each
// address's history, which is what made the scan impossibly slow: a Lookup walks
// a shard's every range on disk, and there is one address to ask about per
// script on the chain. The count is derived from the blocks as they go past
// instead, so the whole pass is one sequential read.
//
// Note what the count now means. The blocks carry outputs, not the undo data a
// spend would need, so this counts the transactions that **paid to** an address
// — not those that spent from it. Every address is still seen, since an input
// can only spend an output paid earlier, but a busy spender whose receipts are
// few will count lower here than the index would report.
func actbuild(opt *options) {
    if opt.blocks == "" {
        logging.Fatal("actbuild needs -blocks pointing at Core's blocks directory")
    }
    var files, err = blockFiles(opt.blocks)
    if err != nil { logging.Fatal("%v", err) }
    var key, kerr = xorKey(opt.blocks)
    if kerr != nil { logging.Fatal("read xor.dat: %v", kerr) }
    if err := ensureBuckets(); err != nil { logging.Fatal("create buckets: %v", err) }

    // Every file, every time. The counts live only in memory, so a partial scan
    // would write partial counts and call them totals — there is no cursor to
    // resume from because a resumed run's numbers would be wrong.
    fmt.Printf("Scanning %d files in %s for addresses in more than %d transactions\n",
        len(files), opt.blocks, activeMin)
    var started = time.Now()
    var counts = newCounter(opt.addrs, uint32(activeMin))
    var blocks, scripts int
    for i, name := range files {
        var b, s, serr = countFile(name, key, counts)
        if serr != nil {
            logging.Fatal("actbuild: %v", serr)
        }
        blocks += b
        scripts += s
        logging.Info("actbuild: file %d of %d: %d blocks, %s addresses known, %d past %d, %s",
            i, len(files)-1, b, group(int64(counts.entries())), len(counts.qualified), activeMin,
            progress(started, 0, i, len(files)-1, counts.entries()))
    }
    var active = counts.active()
    if err := storeActive(active); err != nil {
        logging.Fatal("actbuild: write results: %v", err)
    }
    var elapsed = time.Since(started)
    fmt.Printf("Read %d blocks and %s outputs in %s: %s addresses, %d in more than %d transactions\n",
        blocks, group(int64(scripts)), took(elapsed), group(int64(counts.entries())), len(active), activeMin)
}

// countFile reads one blk file end to end, counting every address each
// transaction pays to.
func countFile(name string, key []byte, counts *counter) (blocks, scripts int, err error) {
    var r, oerr = openBlockFile(name, key)
    if oerr != nil { return 0, 0, oerr }
    defer r.Close()
    var seen = map[string]bool{}
    for {
        var raw, rerr = r.next()
        if rerr != nil { return blocks, scripts, fmt.Errorf("%s: %w", name, rerr) }
        if raw == nil { break }
        blocks++
        var txs, ok = addrindex.OutputsByTx(raw)
        if !ok {
            logging.Warn("actbuild: could not parse a block in %s", name)
            continue
        }
        for _, outputs := range txs {
            // one transaction counts once per address, however many of its
            // outputs pay there
            clear(seen)
            for _, script := range outputs {
                if len(script) == 0 || seen[string(script)] { continue }
                seen[string(script)] = true
                scripts++
                counts.add(script)
            }
        }
    }
    return blocks, scripts, nil
}

// storeActive writes the qualifying addresses, encoded from the scripts kept
// when they crossed the threshold.
func storeActive(active map[string]uint32) error {
    return db.Update(func(tx *bbolt.Tx) error {
        var b = tx.Bucket(activeBucket)
        for script, n := range active {
            var addr = scriptAddress([]byte(script))
            // a nonstandard script is not an address, so there is nothing to
            // record for it
            if addr == "" { continue }
            var v = make([]byte, 8)
            binary.BigEndian.PutUint64(v, uint64(n))
            if err := b.Put([]byte(addr), v); err != nil { return err }
        }
        return nil
    })
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

func ensureBuckets() error {
    return db.Update(func(tx *bbolt.Tx) error {
        var _, err = tx.CreateBucketIfNotExists(activeBucket)
        return err
    })
}

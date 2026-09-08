// Package cursors is the one bucket every scan over the chain keeps its place
// in. A scan resumes where it stopped, so its place is the only thing standing
// between a restart and starting over — and each used to keep it in a bucket of
// its own (blocks-cursor, miners-cursor, addrindex-cursor), each holding a
// single key called "cursor".
//
// They hold the same thing in the same form — a block height, or a file number,
// as decimal text — so they are one bucket keyed by the scan's own name:
//
//	blocks         the block-info cache's backfill (blocks.go)
//	miners         the per-pool statistics collector (miners/stats.go)
//	addrindex      the address index's build (addrindex/)
//	actbuild-file  the busy-address pass over Core's block files (tools/addrindex)
//
// Init carries the old buckets forward, so a database written before this keeps
// its place rather than rescanning the chain. The old buckets are left where
// they are: an address index that had to be built again is hours of work, and
// leaving them costs three keys and makes going back free.
package cursors

import "strconv"

import "go.etcd.io/bbolt"
import "bitnsbot/logging"

// The scans that keep a place here. Constants rather than strings at the call
// sites: a name that does not match is not an error, it is a scan that silently
// starts from the beginning, which for the address index is a rebuild measured
// in hours.
const Blocks = "blocks"
const Miners = "miners"
const AddrIndex = "addrindex"
const ActBuild = "actbuild-file"

var bucket = []byte("cursors")

var db *bbolt.DB

// carried is where each cursor used to live. Only the value is moved, and only
// when this bucket has nothing under that name yet, so running Init again — it
// is called from every package that keeps a cursor, and by tools/addrindex —
// cannot walk a scan backwards.
var carried = []struct{ bucket, key, name string }{
    {"blocks-cursor", "cursor", Blocks},
    {"miners-cursor", "cursor", Miners},
    {"addrindex-cursor", "cursor", AddrIndex},
    {"addrindex-cursor", "actbuild-file", ActBuild},
}

func Init(handle *bbolt.DB) error {
    db = handle
    return db.Update(func(tx *bbolt.Tx) error {
        var b, err = tx.CreateBucketIfNotExists(bucket)
        if err != nil { return err }
        for _, c := range carried {
            if b.Get([]byte(c.name)) != nil { continue }
            var old = tx.Bucket([]byte(c.bucket))
            if old == nil { continue }
            var v = old.Get([]byte(c.key))
            if v == nil { continue }
            if err := b.Put([]byte(c.name), v); err != nil { return err }
            logging.Status("cursors: carried %s/%s forward as %s (%s)", c.bucket, c.key, c.name, v)
        }
        return nil
    })
}

// Get reads one scan's place. Not found is not zero: a scan that has never run
// starts somewhere of its own choosing — genesis for the block cache, a window
// back from the tip for the miner statistics — which is not where a scan that
// stopped at height 0 resumes.
func Get(name string) (int64, bool) {
    if db == nil { return 0, false }
    var v int64
    var ok bool
    db.View(func(tx *bbolt.Tx) error {
        var b = tx.Bucket(bucket)
        if b == nil { return nil }
        if raw := b.Get([]byte(name)); raw != nil {
            var n, err = strconv.ParseInt(string(raw), 10, 64)
            if err == nil { v, ok = n, true }
        }
        return nil
    })
    return v, ok
}

// Set writes one inside the caller's transaction, which is the whole point of
// taking a tx: a scan advances its place in the same commit as the batch that
// reached it, so a crash between the two cannot skip work or repeat it.
func Set(tx *bbolt.Tx, name string, v int64) error {
    var b = tx.Bucket(bucket)
    if b == nil {
        var created, err = tx.CreateBucket(bucket)
        if err != nil { return err }
        b = created
    }
    return b.Put([]byte(name), []byte(strconv.FormatInt(v, 10)))
}

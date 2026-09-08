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
// Init also removes the three buckets those cursors used to live in. The values
// were carried across by the version that introduced this bucket; what is left
// behind is an empty shell that nothing reads, and a place to edit a number and
// wonder why it changes nothing.
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

// obsolete are the buckets each cursor used to live in, dropped on the first
// start that finds them. A database that has not been through the version which
// carried their values across loses them here, and the scans start over — for
// the address index, hours of work — so that version is the upgrade step between
// an older database and this one.
var obsolete = []string{"blocks-cursor", "miners-cursor", "addrindex-cursor"}

// Init is called from every package that keeps a cursor, and by tools/addrindex,
// so it runs several times a start and must be safe to repeat: creating the
// bucket and dropping the old ones are both no-ops the second time.
func Init(handle *bbolt.DB) error {
    db = handle
    return db.Update(func(tx *bbolt.Tx) error {
        var _, err = tx.CreateBucketIfNotExists(bucket)
        if err != nil { return err }
        for _, name := range obsolete {
            if tx.Bucket([]byte(name)) == nil { continue }
            if err := tx.DeleteBucket([]byte(name)); err != nil { return err }
            logging.Status("cursors: removed the obsolete %s bucket", name)
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

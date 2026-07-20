// Package addrindex is a compact address→transactions index, built because
// Bitcoin Core (unlike btcd's --addrindex) has no address index at all and the
// bot's /info <address> history stats need one.
//
// The scheme is electrs/bindex-rs adapted for bbolt. bindex keys one row per
// "touch" (an address appearing in an output, or in a spent input) as
// `scriptHashPrefix(8) + globalTxNum(4)` in RocksDB, whose LSM makes billions of
// tiny rows cheap and whose Zstd compression shrinks them. bbolt is a B+tree: it
// neither compacts nor compresses, and a row per touch would pay bbolt's
// per-key page overhead billions of times over. So the layout is inverted — one
// key per address, whose value is the append-only list of that address's touches
// — which amortizes the key overhead across every touch of the address.
//
// A touch is stored as `height(4) + txIndex(2)` = 6 bytes, not bindex's 4-byte
// global tx number, because that avoids a second txNum→txid index: (height,
// txIndex) resolves straight to a txid via one getblock at verbosity 1.
//
// Two deliberate consequences, both matching what the btcd path already did:
//   - The 8-byte script-hash prefix can collide, so a reader must confirm each
//     resolved transaction actually involves the address (post-filtering).
//   - Per-address history is capped (maxTouches), which both bounds the value
//     size — killing bbolt's write amplification on exchange-hot addresses — and
//     mirrors the old addrTxLimit that showed "10000+".
package addrindex

import "crypto/sha256"
import "encoding/binary"
import "encoding/hex"

import "go.etcd.io/bbolt"

var db *bbolt.DB
var bucket = []byte("addrindex")
var cursorBucket = []byte("addrindex-cursor")

// prefixLen is the script-hash prefix width. 8 bytes is what bindex uses: wide
// enough that collisions are astronomically rare, narrow enough to keep keys
// small. Readers post-filter, so a collision costs a wasted fetch, never a wrong
// answer.
const prefixLen = 8

// touchLen is the on-disk size of one touch: big-endian height then txIndex, so
// appended touches stay in chronological order and sort that way.
const touchLen = 6

// maxTouches caps how many touches are stored per address. It bounds the value
// size (so a hot address doesn't force multi-megabyte rewrites in a B+tree) and
// matches the history limit the btcd path already imposed. A package var for
// tests.
var maxTouches = 10000

// Touch is one appearance of an address in the chain: an output paying it or an
// input spending from it, located by block height and the transaction's index in
// that block.
type Touch struct {
    Height  uint32
    TxIndex uint16
}

// Init stores the shared bbolt handle and ensures the index buckets exist.
func Init(handle *bbolt.DB) error {
    db = handle
    return db.Update(func(tx *bbolt.Tx) error {
        for _, name := range [][]byte{bucket, cursorBucket} {
            if _, err := tx.CreateBucketIfNotExists(name); err != nil { return err }
        }
        return nil
    })
}

// Prefix is the index key for a scriptPubKey: the first prefixLen bytes of its
// SHA-256, the same script-hash electrs derives (before its display reversal).
func Prefix(script []byte) []byte {
    var sum = sha256.Sum256(script)
    return sum[:prefixLen]
}

// PrefixHex is Prefix over a hex-encoded scriptPubKey (what validateaddress and
// the block parser hand back), returning "" for malformed hex.
func PrefixHex(scriptHex string) []byte {
    var raw, err = hex.DecodeString(scriptHex)
    if err != nil { return nil }
    return Prefix(raw)
}

func encodeTouch(t Touch) []byte {
    var b = make([]byte, touchLen)
    binary.BigEndian.PutUint32(b[0:4], t.Height)
    binary.BigEndian.PutUint16(b[4:6], t.TxIndex)
    return b
}

// merge folds a chunk of per-address touches into the bucket in one transaction,
// appending to each address's existing list up to the cap. Touches arrive in
// chronological order (blocks oldest-first, txs in block order), so the stored
// list stays ordered without sorting. Returns nil-safe when db is unset (tests).
func merge(touches map[string][]Touch, cursor Cursor) error {
    if db == nil { return nil }
    return db.Update(func(tx *bbolt.Tx) error {
        var b = tx.Bucket(bucket)
        for prefix, list := range touches {
            var existing = b.Get([]byte(prefix))
            if len(existing) >= maxTouches*touchLen { continue }
            var buf = make([]byte, len(existing), len(existing)+len(list)*touchLen)
            copy(buf, existing)
            for _, t := range list {
                if len(buf) >= maxTouches*touchLen { break }
                buf = append(buf, encodeTouch(t)...)
            }
            if err := b.Put([]byte(prefix), buf); err != nil { return err }
        }
        return saveCursor(tx, cursor)
    })
}

// Lookup returns an address's touches, oldest first, and whether the stored list
// was capped (so the caller can flag partial history — the "10000+" case).
func Lookup(script []byte) (touches []Touch, capped bool) {
    if db == nil { return nil, false }
    var prefix = Prefix(script)
    db.View(func(tx *bbolt.Tx) error {
        var v = tx.Bucket(bucket).Get(prefix)
        for i := 0; i+touchLen <= len(v); i += touchLen {
            touches = append(touches, Touch{
                Height:  binary.BigEndian.Uint32(v[i : i+4]),
                TxIndex: binary.BigEndian.Uint16(v[i+4 : i+6]),
            })
        }
        capped = len(v) >= maxTouches*touchLen
        return nil
    })
    return touches, capped
}

// Cursor records how far the index has been built: the last indexed height. Like
// the miners package's collector, a block reorged out after being indexed stays
// indexed — not worth correcting in a best-effort browsing aid.
type Cursor struct {
    Height int64
}

func saveCursor(tx *bbolt.Tx, c Cursor) error {
    return tx.Bucket(cursorBucket).Put([]byte("cursor"), itob(c.Height))
}

// LoadCursor returns the built-to position, or ok=false on a fresh index.
func LoadCursor() (Cursor, bool) {
    if db == nil { return Cursor{}, false }
    var c Cursor
    var ok bool
    db.View(func(tx *bbolt.Tx) error {
        if v := tx.Bucket(cursorBucket).Get([]byte("cursor")); len(v) == 8 {
            c.Height = int64(binary.BigEndian.Uint64(v))
            ok = true
        }
        return nil
    })
    return c, ok
}

func itob(v int64) []byte {
    var b = make([]byte, 8)
    binary.BigEndian.PutUint64(b, uint64(v))
    return b
}

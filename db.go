package main

import "encoding/binary"

import "go.etcd.io/bbolt"
import "bitnsbot/addrindex"
import "bitnsbot/logging"
import "bitnsbot/miners"
import "bitnsbot/rates"
import "bitnsbot/watches"

var db *bbolt.DB

// openDB opens the shared bbolt file and hands it to every package that owns
// buckets. One file, one handle.
//
// Every package that owns buckets must be Init'd here. Forgetting one is not a
// loud failure: those packages guard their operations on a nil handle, so the
// package silently does nothing — which is exactly how the address index came to
// fetch and parse the whole chain while storing none of it. TestOpenDBBuckets
// pins the full set.
func openDB(path string) error {
    logging.Db("open %s", path)
    var opened, err = bbolt.Open(path, 0600, nil)
    if err != nil { return err }
    db = opened
    if err := blockInit(db); err != nil { return err }
    if err := rates.Init(db); err != nil { return err }
    if err := watches.Init(db); err != nil { return err }
    if err := miners.Init(db); err != nil { return err }
    if err := addrindex.Init(db); err != nil { return err }
    return nil
}

func closeDB() error {
    return db.Close()
}

// itob encodes a uint64 as a big-endian key — the standard bbolt idiom for keys
// that sort in numeric order. Used by the block cache (rates/watches keep their
// own copies inside their packages).
func itob(v uint64) []byte {
    var buf = make([]byte, 8)
    binary.BigEndian.PutUint64(buf, v)
    return buf
}

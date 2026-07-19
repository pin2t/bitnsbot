package main

import "encoding/binary"

import "go.etcd.io/bbolt"
import "bitnsbot/logging"
import "bitnsbot/miners"
import "bitnsbot/rates"
import "bitnsbot/watches"

var db *bbolt.DB

// openDB opens the shared bbolt file, creates the block-cache bucket, and hands
// the same handle to the rates, watches, and miners packages (which own their
// own buckets). One file, one handle, four buckets.
func openDB(path string) error {
    logging.Db("open %s", path)
    var opened, err = bbolt.Open(path, 0600, nil)
    if err != nil { return err }
    err = opened.Update(func(tx *bbolt.Tx) error {
        for _, name := range [][]byte{blocksBucket} {
            if _, err := tx.CreateBucketIfNotExists(name); err != nil { return err }
        }
        return nil
    })
    if err != nil {
        opened.Close()
        return err
    }
    db = opened
    if err := rates.Init(db); err != nil { return err }
    if err := watches.Init(db); err != nil { return err }
    if err := miners.Init(db); err != nil { return err }
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

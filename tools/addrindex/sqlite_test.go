package main

import "database/sql"
import "encoding/binary"
import "path/filepath"
import "strings"
import "testing"

import "go.etcd.io/bbolt"

// migrate copies the built bbolt index into a SQLite database shaped exactly as
// tools/tosqlite writes one — the same DDL, and the same packing of the six-byte
// key into one integer — so the listing is read back through the real format
// rather than through a fixture written to suit it.
func migrate(t *testing.T) string {
    t.Helper()
    var path = filepath.Join(t.TempDir(), "ai.sqlite.db")
    var conn, err = sql.Open("sqlite", path)
    if err != nil { t.Fatalf("open: %v", err) }
    defer conn.Close()
    if _, err := conn.Exec(`create table addrindex (shard INTEGER PRIMARY KEY, data BLOB NOT NULL)`); err != nil {
        t.Fatalf("schema: %v", err)
    }
    var rows int
    if err := db.View(func(tx *bbolt.Tx) error {
        return tx.Bucket([]byte("addrindex")).ForEach(func(k, v []byte) error {
            var packed int64
            for _, c := range k { packed = packed<<8 | int64(c) }
            var _, err = conn.Exec("insert into addrindex (shard, data) values (?, ?)", packed, v)
            rows++
            return err
        })
    }); err != nil {
        t.Fatalf("migrate: %v", err)
    }
    if rows == 0 { t.Fatal("migrated an empty index — the fixture built nothing to read") }
    return path
}

// The listing off the SQLite copy must be the listing off the bbolt index, line
// for line: only the storage differs.
func TestListFromSQLite(t *testing.T) {
    openIndex(t)
    var srv = fakeCore(t, 3)
    var opt = &options{url: srv.URL, limit: 1000}
    capture(t, func() { build(opt) })
    var want = capture(t, func() { list(opt, address) })
    var sqlited = &options{url: srv.URL, limit: 1000, dbsqlite: migrate(t)}
    var got = capture(t, func() { list(sqlited, address) })
    if got != want {
        t.Errorf("the SQLite listing differs from the bbolt one:\ngot:\n%swant:\n%s", got, want)
    }
    if !strings.Contains(got, "bbbbbbbb..bbbbbbb1    20000 sat") {
        t.Errorf("the funding transaction is missing from:\n%s", got)
    }
}

// The shard scan must find the address's every range and no one else's: a shard
// holds every address whose hash starts with the same two bytes.
func TestSQLiteTouches(t *testing.T) {
    openIndex(t)
    var srv = fakeCore(t, 3)
    capture(t, func() { build(&options{url: srv.URL, limit: 1000}) })
    var path = migrate(t)
    var touches, capped, err = sqliteTouches(path, payScript, 1000)
    if err != nil { t.Fatalf("lookup: %v", err) }
    if capped { t.Error("capped at a limit far above the fixture's two touches") }
    // block 1 pays the address, block 2 spends from it and pays change back
    if len(touches) != 2 || touches[0].Height != 1 || touches[1].Height != 2 {
        t.Fatalf("touches = %v, want heights 1 and 2", touches)
    }
    if touches[0].TxIndex != 1 || touches[1].TxIndex != 1 {
        t.Errorf("touches = %v, want the second transaction of each block", touches)
    }
    // the entries of every other address in the same shard must be filtered out
    var unrelated, _, uerr = sqliteTouches(path, []byte("no address ever paid this script"), 1000)
    if uerr != nil { t.Fatalf("lookup: %v", uerr) }
    if len(unrelated) != 0 { t.Errorf("an unindexed script matched %v", unrelated) }
}

// The limit is what bounds a reply, so it has to hold across the shard's rows.
func TestSQLiteTouchesLimit(t *testing.T) {
    openIndex(t)
    var srv = fakeCore(t, 3)
    capture(t, func() { build(&options{url: srv.URL, limit: 1000}) })
    var touches, capped, err = sqliteTouches(migrate(t), payScript, 1)
    if err != nil { t.Fatalf("lookup: %v", err) }
    if !capped || len(touches) != 1 {
        t.Errorf("touches = %v, capped = %v; want one touch and the partial flag", touches, capped)
    }
}

// A mistyped path must say so. SQLite would otherwise create the file and report
// a missing table, which reads like a broken migration.
func TestSQLiteMissingFile(t *testing.T) {
    var _, _, err = sqliteTouches(filepath.Join(t.TempDir(), "absent.db"), payScript, 10)
    if err == nil { t.Error("a missing database was read as an empty one") }
}

// The shard bound is the packing tosqlite documents: the two shard bytes and the
// four range bytes read as one big-endian integer, which is what keeps a shard's
// ranges together and in order.
func TestShardBounds(t *testing.T) {
    var key = []byte{0xab, 0xcd, 0x00, 0x00, 0x01, 0x2c}
    var packed int64
    for _, c := range key { packed = packed<<8 | int64(c) }
    var shard = int64(binary.BigEndian.Uint16(key[:shardLen]))
    if packed < shard<<32 || packed >= (shard+1)<<32 {
        t.Errorf("key %x packs to %d, outside its own shard's range", key, packed)
    }
    if got := uint32(packed) * rangeBlocks; got != 300000 {
        t.Errorf("range 300 starts at block %d, want 300000", got)
    }
}

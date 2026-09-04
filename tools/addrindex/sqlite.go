package main

import "bytes"
import "database/sql"
import "encoding/binary"
import "os"

import _ "modernc.org/sqlite"
import "bitnsbot/addrindex"

// The index's packed layout, which a reader outside the addrindex package has to
// know: a key is a 2-byte shard and a 4-byte block-range number, and a value is
// the run of that shard-range's touches, each one the script hash's remaining
// bytes, the block's offset within the range, and the transaction's index in
// that block. These are the format's numbers, not this tool's — prefixLen is
// counter.go's, the same 8-byte script-hash prefix — so they cannot be changed
// on one side alone.
const shardLen = 2
const remainderLen = prefixLen - shardLen
const touchLen = remainderLen + 2 + 2
const rangeBlocks = 1000

// sqliteTouches reads an address's touches out of the SQLite database tosqlite
// writes, which holds exactly what the bbolt bucket holds: the migration packs
// the six-byte key into one integer — big-endian, so a shard's ranges keep the
// order a scan depends on — and copies each packed run of touches verbatim. So a
// lookup is one range scan over the primary key, and the entries are unpacked
// here the way the index packs them.
func sqliteTouches(path string, script []byte, limit int) ([]addrindex.Touch, bool, error) {
    // a missing file would otherwise be opened as a new empty database and
    // answer "no such table", which reads like a broken migration rather than a
    // mistyped path
    if _, err := os.Stat(path); err != nil { return nil, false, err }
    // read-only, so a listing cannot damage a database the migration spent hours
    // building
    var conn, err = sql.Open("sqlite", "file:"+path+"?mode=ro")
    if err != nil { return nil, false, err }
    defer conn.Close()
    var prefix = addrindex.Prefix(script)
    var shard = int64(binary.BigEndian.Uint16(prefix[:shardLen]))
    // every range of the one shard the address falls in, which is the whole of
    // what can hold it
    var rows, qerr = conn.Query(
        "select shard, data from addrindex where shard >= ? and shard < ? order by shard",
        shard<<32, (shard+1)<<32)
    if qerr != nil { return nil, false, qerr }
    defer rows.Close()
    // a shard holds every address whose hash starts with the same two bytes, so
    // the remainder stored per touch is what tells them apart
    var remainder = prefix[shardLen:prefixLen]
    var touches []addrindex.Touch
    for rows.Next() {
        var packed int64
        var data []byte
        if err := rows.Scan(&packed, &data); err != nil { return nil, false, err }
        var base = uint32(packed) * rangeBlocks
        for i := 0; i+touchLen <= len(data); i += touchLen {
            if !bytes.Equal(data[i:i+remainderLen], remainder) { continue }
            if len(touches) >= limit { return touches, true, nil }
            touches = append(touches, addrindex.Touch{
                Height:  base + uint32(binary.BigEndian.Uint16(data[i+remainderLen:])),
                TxIndex: binary.BigEndian.Uint16(data[i+remainderLen+2:]),
            })
        }
    }
    return touches, false, rows.Err()
}

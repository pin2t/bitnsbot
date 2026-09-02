package main

import "database/sql"
import "encoding/json"
import "path/filepath"
import "testing"

import "go.etcd.io/bbolt"

// setup builds a bbolt database from seed plus an empty SQLite database carrying
// the real schema, so every test migrates through exactly what the tool ships.
func setup(t *testing.T, seed func(*bbolt.Tx) error) (*bbolt.DB, *sql.DB) {
    t.Helper()
    var dir = t.TempDir()
    var source, err = bbolt.Open(filepath.Join(dir, "src.db"), 0600, nil)
    if err != nil { t.Fatal(err) }
    t.Cleanup(func() { source.Close() })
    if err := source.Update(seed); err != nil { t.Fatal(err) }
    target, err := sql.Open("sqlite", filepath.Join(dir, "dst.db"))
    if err != nil { t.Fatal(err) }
    t.Cleanup(func() { target.Close() })
    target.SetMaxOpenConns(1)
    for _, s := range schema {
        if _, err := target.Exec(s); err != nil { t.Fatalf("%s: %v", s, err) }
    }
    return source, target
}

func put(t *testing.T, tx *bbolt.Tx, bucket string, key []byte, value any) {
    t.Helper()
    var b, err = tx.CreateBucketIfNotExists([]byte(bucket))
    if err != nil { t.Fatal(err) }
    var data, ok = value.([]byte)
    if !ok {
        data, err = json.Marshal(value)
        if err != nil { t.Fatal(err) }
    }
    if err := b.Put(key, data); err != nil { t.Fatal(err) }
}

func count(t *testing.T, db *sql.DB, query string, args ...any) int {
    t.Helper()
    var n int
    if err := db.QueryRow(query, args...).Scan(&n); err != nil { t.Fatal(err) }
    return n
}

func TestCopyBlocks(t *testing.T) {
    var source, target = setup(t, func(tx *bbolt.Tx) error {
        put(t, tx, "blocks-stat", itob(963268), blockInfo{
            Height: 963268, Hash: "0000abc", Time: 1756771200, Size: 1398242, NumTx: 3105,
            Miner: "Foundry USA", FeesOK: true, FeeMin: 1, FeeAvg: 4, FeeMax: 900,
            TxSizeMin: 188, TxSizeAvg: 450, TxSizeMax: 99000,
            Reward: 312500000, Total: 320000000, Difficulty: 1.4e14,
        })
        put(t, tx, "blocks-stat", itob(963269), blockInfo{Height: 963269, Hash: "0000def", Miner: "Unknown"})
        put(t, tx, "blocks-stat", itob(963270), []byte("{not json"))
        return nil
    })
    var rows, skipped, err = copyBlocks(source, target)
    if err != nil { t.Fatal(err) }
    if rows != 2 || skipped != 1 { t.Fatalf("rows=%d skipped=%d, want 2 and 1", rows, skipped) }
    var hash, miner string
    var ts, size, txs, feesOK, reward, fees int64
    var difficulty float64
    var q = "select hash, ts, size, txs, miner, feesOK, reward, fees, difficulty from blocks where height = ?"
    if err := target.QueryRow(q, 963268).Scan(&hash, &ts, &size, &txs, &miner, &feesOK, &reward, &fees, &difficulty); err != nil {
        t.Fatal(err)
    }
    if hash != "0000abc" || ts != 1756771200 || size != 1398242 || txs != 3105 || miner != "Foundry USA" {
        t.Errorf("got %s %d %d %d %s", hash, ts, size, txs, miner)
    }
    if feesOK != 1 { t.Errorf("feesOK = %d, want 1", feesOK) }
    // total is the whole coinbase output, so the fees column is total - reward
    if reward != 312500000 || fees != 7500000 { t.Errorf("reward=%d fees=%d, want 312500000 and 7500000", reward, fees) }
    if difficulty != 1.4e14 { t.Errorf("difficulty = %v", difficulty) }
    if got := count(t, target, "select feesOK from blocks where height = 963269"); got != 0 {
        t.Errorf("feesOK = %d for a block without fee stats, want 0", got)
    }
}

func TestCopyMarketToCents(t *testing.T) {
    var source, target = setup(t, func(tx *bbolt.Tx) error {
        put(t, tx, "market", itob(1756771200), marketRecord{
            Timestamp: 1756771200, Price: 66223.005, MarketCap: 1.33e12, Volume24h: 3.191e10,
        })
        return nil
    })
    var rows, skipped, err = copyMarket(source, target)
    if err != nil { t.Fatal(err) }
    if rows != 1 || skipped != 0 { t.Fatalf("rows=%d skipped=%d", rows, skipped) }
    var price, cap_, volume int64
    if err := target.QueryRow("select price, cap, volume24h from market where ts = 1756771200").Scan(&price, &cap_, &volume); err != nil {
        t.Fatal(err)
    }
    if price != 6622301 { t.Errorf("price = %d, want 6622301 cents (rounded)", price) }
    if cap_ != 133000000000000 { t.Errorf("cap = %d, want 133000000000000 cents", cap_) }
    if volume != 3191000000000 { t.Errorf("volume24h = %d, want 3191000000000 cents", volume) }
}

func TestCopyMinersPairsAddressesAndTags(t *testing.T) {
    var source, target = setup(t, func(tx *bbolt.Tx) error {
        put(t, tx, "miners", []byte("addr-f2-1"), []byte("F2Pool"))
        put(t, tx, "miners", []byte("addr-f2-2"), []byte("F2Pool"))
        put(t, tx, "miners", []byte("addr-ant"), []byte("AntPool"))
        put(t, tx, "miners-tag", []byte("/f2pool/"), []byte("F2Pool"))
        put(t, tx, "miners-tag", []byte("Mined by AntPool"), []byte("AntPool"))
        put(t, tx, "miners-tag", []byte("/Foundry USA Pool/"), []byte("Foundry USA"))
        put(t, tx, "miners-stat", []byte("F2Pool"), minerStat{Blocks: 12, Reward: 3801, Fees: 39, Work: 8.5, LastWork: 6.0})
        put(t, tx, "miners-stat", []byte("Braiins"), minerStat{Blocks: 1, Reward: 312, Fees: 4, Work: 1.5, LastWork: 1.5})
        return nil
    })
    var rows, skipped, err = copyMiners(source, target)
    if err != nil { t.Fatal(err) }
    if skipped != 0 { t.Fatalf("skipped = %d", skipped) }
    // F2Pool: 2 addresses zipped against 1 tag = 2 rows; AntPool 1; Foundry USA
    // tag-only 1; Braiins known only from its aggregate 1
    if rows != 5 { t.Fatalf("rows = %d, want 5", rows) }
    if got := count(t, target, "select count(*) from miners"); got != 5 { t.Fatalf("stored %d rows", got) }
    var tag string
    var blocks, reward, fees int64
    var totalWork, lastWork float64
    var q = "select tag, blocks, reward, fees, totalWork, lastWork from miners where name = 'F2Pool' and address = ?"
    if err := target.QueryRow(q, "addr-f2-1").Scan(&tag, &blocks, &reward, &fees, &totalWork, &lastWork); err != nil {
        t.Fatal(err)
    }
    if tag != "/f2pool/" { t.Errorf("tag = %q, want the pool's one tag on its first row", tag) }
    if blocks != 12 || reward != 3801 || fees != 39 || totalWork != 8.5 || lastWork != 6.0 {
        t.Errorf("stats = %d %d %d %v %v", blocks, reward, fees, totalWork, lastWork)
    }
    // the second address has no tag to pair with, and the aggregate repeats
    if err := target.QueryRow(q, "addr-f2-2").Scan(&tag, &blocks, &reward, &fees, &totalWork, &lastWork); err != nil {
        t.Fatal(err)
    }
    if tag != "" || blocks != 12 { t.Errorf("tag = %q blocks = %d, want an unpaired row carrying the same aggregate", tag, blocks) }
    var address string
    if err := target.QueryRow("select address, tag from miners where name = 'Foundry USA'").Scan(&address, &tag); err != nil {
        t.Fatal(err)
    }
    if address != "" || tag != "/Foundry USA Pool/" { t.Errorf("tag-only pool stored as address=%q tag=%q", address, tag) }
    if err := target.QueryRow("select address, tag, blocks from miners where name = 'Braiins'").Scan(&address, &tag, &blocks); err != nil {
        t.Fatal(err)
    }
    if address != "" || tag != "" || blocks != 1 {
        t.Errorf("stats-only pool stored as address=%q tag=%q blocks=%d", address, tag, blocks)
    }
}

func TestCopyRatesReadsTimestampFromKey(t *testing.T) {
    var source, target = setup(t, func(tx *bbolt.Tx) error {
        // the stored value carries only the price; the timestamp is the key
        put(t, tx, "rates", itob(1756771200), rateRecord{Cents: 6622300})
        put(t, tx, "rates", itob(1756771500), rateRecord{Cents: 6630000})
        // a key that went through dbui's CSV export and came back undecoded
        put(t, tx, "rates", []byte("hex:0000000068b6b3fc"), rateRecord{Cents: 6700000})
        put(t, tx, "rates", []byte("short"), rateRecord{Cents: 1})
        put(t, tx, "rates", []byte("hex:nothexatall"), rateRecord{Cents: 2})
        return nil
    })
    var rows, skipped, err = copyRates(source, target)
    if err != nil { t.Fatal(err) }
    if rows != 3 || skipped != 2 { t.Fatalf("rows=%d skipped=%d, want 3 and 2", rows, skipped) }
    if got := count(t, target, "select cents from rates where ts = 1756771200"); got != 6622300 {
        t.Errorf("cents = %d, want 6622300", got)
    }
    // 0x68b6b3fc — the hex: marker decoded back to the timestamp it stands for
    if got := count(t, target, "select cents from rates where ts = 1756804092"); got != 6700000 {
        t.Errorf("a dbui-encoded key did not decode to its timestamp")
    }
    if got := count(t, target, "select count(*) from rates where ts = 0"); got != 0 {
        t.Errorf("a record with an unusable key was stored at ts 0")
    }
}

func TestCopyWatchesCollapsesDuplicates(t *testing.T) {
    var source, target = setup(t, func(tx *bbolt.Tx) error {
        put(t, tx, "watches", itob(1), watchRecord{Created: 100, Chat: 42, Watch: "bc1qaaa", Alias: "cold"})
        put(t, tx, "watches", itob(2), watchRecord{Created: 200, Chat: 42, Watch: "bc1qbbb", Alias: ""})
        // the same chat watching the same address twice, which the bucket permits
        put(t, tx, "watches", itob(3), watchRecord{Created: 300, Chat: 42, Watch: "bc1qaaa", Alias: "renamed"})
        put(t, tx, "watches", itob(4), watchRecord{Created: 400, Chat: 7, Watch: "bc1qaaa", Alias: "someone else"})
        return nil
    })
    var rows, skipped, err = copyWatches(source, target)
    if err != nil { t.Fatal(err) }
    if rows != 3 || skipped != 0 { t.Fatalf("rows=%d skipped=%d, want 3 and 0", rows, skipped) }
    if got := count(t, target, "select count(*) from watches"); got != 3 { t.Fatalf("stored %d rows", got) }
    var alias string
    var created int64
    if err := target.QueryRow("select alias, created from watches where chat = 42 and addr = 'bc1qaaa'").Scan(&alias, &created); err != nil {
        t.Fatal(err)
    }
    if alias != "renamed" || created != 300 { t.Errorf("alias=%q created=%d, want the last record to win", alias, created) }
    // the same address under another chat is a different watch, not a duplicate
    if got := count(t, target, "select count(*) from watches where addr = 'bc1qaaa'"); got != 2 {
        t.Errorf("%d rows for bc1qaaa, want one per chat", got)
    }
}

func TestCopyAddrindexPacksShardAndRange(t *testing.T) {
    var keyOf = func(shard uint16, rangeIndex uint32) []byte {
        var k = make([]byte, 6)
        k[0], k[1] = byte(shard>>8), byte(shard)
        k[2], k[3], k[4], k[5] = byte(rangeIndex>>24), byte(rangeIndex>>16), byte(rangeIndex>>8), byte(rangeIndex)
        return k
    }
    var source, target = setup(t, func(tx *bbolt.Tx) error {
        put(t, tx, "addrindex", keyOf(0x0102, 3), []byte{0xde, 0xad, 0xbe, 0xef})
        put(t, tx, "addrindex", keyOf(0x0102, 4), []byte{0x01})
        put(t, tx, "addrindex", keyOf(0x0103, 0), []byte{0x02})
        put(t, tx, "addrindex", []byte("bad"), []byte{0x03})
        return nil
    })
    var rows, skipped, err = copyAddrindex(source, target)
    if err != nil { t.Fatal(err) }
    if rows != 3 || skipped != 1 { t.Fatalf("rows=%d skipped=%d, want 3 and 1", rows, skipped) }
    // 0x010200000003, the whole six-byte key read as one big-endian integer
    var want int64 = 0x010200000003
    var data []byte
    if err := target.QueryRow("select data from addrindex where shard = ?", want).Scan(&data); err != nil { t.Fatal(err) }
    if string(data) != string([]byte{0xde, 0xad, 0xbe, 0xef}) { t.Errorf("data = % x", data) }
    // packing must keep the order the index's cursor scan depends on
    var rowsOut, err2 = target.Query("select shard from addrindex order by shard")
    if err2 != nil { t.Fatal(err2) }
    defer rowsOut.Close()
    var got []int64
    for rowsOut.Next() {
        var s int64
        if err := rowsOut.Scan(&s); err != nil { t.Fatal(err) }
        got = append(got, s)
    }
    var expect = []int64{0x010200000003, 0x010200000004, 0x010300000000}
    if len(got) != len(expect) { t.Fatalf("got %d rows", len(got)) }
    for i := range expect {
        if got[i] != expect[i] { t.Errorf("row %d = %#x, want %#x", i, got[i], expect[i]) }
    }
}

func TestCopyAcrossBatches(t *testing.T) {
    var source, target = setup(t, func(tx *bbolt.Tx) error {
        for i := 0; i < 25; i++ { put(t, tx, "rates", itob(uint64(1756771200+i)), rateRecord{Cents: int64(i)}) }
        return nil
    })
    var saved = *batch
    *batch = 4
    defer func() { *batch = saved }()
    var rows, _, err = copyRates(source, target)
    if err != nil { t.Fatal(err) }
    // 25 rows over a batch of 4 leaves a partial final transaction to commit
    if rows != 25 { t.Fatalf("rows = %d", rows) }
    if got := count(t, target, "select count(*) from rates"); got != 25 { t.Errorf("stored %d rows", got) }
}

func TestCopyEmptyDatabase(t *testing.T) {
    var source, target = setup(t, func(tx *bbolt.Tx) error { return nil })
    for _, c := range tables {
        var rows, skipped, err = c.copy(source, target)
        if err != nil { t.Fatalf("%s: %v", c.name, err) }
        if rows != 0 || skipped != 0 { t.Errorf("%s: rows=%d skipped=%d, want an absent bucket to be no rows", c.name, rows, skipped) }
    }
}

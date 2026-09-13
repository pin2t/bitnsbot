package main

import "database/sql"
import "testing"

import "go.etcd.io/bbolt"

// seedEverything fills one bucket of every kind, so a validate run has all eight
// mappings to walk rather than only the ones a single test cares about.
func seedEverything(t *testing.T, tx *bbolt.Tx) error {
    t.Helper()
    put(t, tx, "blocks", itob(963268), blockInfo{Height: 963268, Hash: "0000abc", Miner: "Foundry USA",
        FeesOK: true, FeeMin: 1, FeeAvg: 4, FeeMax: 900, Reward: 312500000, Total: 320000000, Difficulty: 1.4e14})
    put(t, tx, "blocks", itob(963269), blockInfo{Height: 963269, Hash: "0000def", Miner: "Unknown"})
    put(t, tx, "market", itob(1756771200), marketRecord{Timestamp: 1756771200, Price: 66223.5, MarketCap: 1.33e12, Volume24h: 3.191e10})
    put(t, tx, "miners", []byte("AntPool"), poolRecord{
        minerStat: minerStat{Blocks: 6, Reward: 1950000000, Fees: 40000000, Work: 3.6e24, LastWork: 6.0e23},
        Addresses: []string{"aAnt1", "aAnt2"}, Tags: []string{"Mined by AntPool"},
    })
    put(t, tx, "rates", itob(1756771200), rateRecord{Cents: 6622350})
    put(t, tx, "watches", []byte("42,bc1qwatched"), watchRecord{Created: 1700000000, Alias: "Cold"})
    put(t, tx, "addrstat", []byte("34xp4vRoCGJym3xR7yCVPFHoCNxv4Twseo"), addrStat{Type: "p2sh",
        Balance: 24859759000000, Recv: 30000000000000, Sent: 5140241000000, Flow: 35140241000000,
        Fees: 1234567, Txs: 4447003, First: 1231006505, Last: 1788220322})
    put(t, tx, "addrstat", []byte("bc1qsmall"), addrStat{Type: "segwit", Balance: 500, Recv: 500, Flow: 500, Txs: 1, First: 1, Last: 2})
    put(t, tx, "cursors", []byte("blocks"), []byte("963269"))
    var b, err = tx.CreateBucketIfNotExists([]byte("addrindex"))
    if err != nil { return err }
    return b.Put([]byte{0x00, 0x01, 0x00, 0x00, 0x00, 0x00}, []byte{0xde, 0xad, 0xbe, 0xef})
}

// migrateAll runs every mapping into the target the way main does.
func migrateAll(t *testing.T, source *bbolt.DB, target *sql.DB) {
    t.Helper()
    for _, tb := range tables {
        if _, _, err := tb.copy(source, writerFor(target, tb.name)); err != nil {
            t.Fatalf("%s: %v", tb.name, err)
        }
    }
    for _, s := range indexes {
        if _, err := target.Exec(s); err != nil { t.Fatalf("%s: %v", s, err) }
    }
}

// A migration this tool made validates against the source it was made from.
func TestValidateAcceptsAFaithfulMigration(t *testing.T) {
    var source, target = setup(t, func(tx *bbolt.Tx) error { return seedEverything(t, tx) })
    migrateAll(t, source, target)
    if !validate(source, target) { t.Error("a faithful migration was reported as differing") }
}

// A column holding something other than what the record does.
func TestValidateCatchesAChangedValue(t *testing.T) {
    var source, target = setup(t, func(tx *bbolt.Tx) error { return seedEverything(t, tx) })
    migrateAll(t, source, target)
    if _, err := target.Exec("update addrstat set balance = balance + 1 where addr = 'bc1qsmall'"); err != nil {
        t.Fatal(err)
    }
    if validate(source, target) { t.Error("a changed balance was not caught") }
}

// A row that never reached the table — the shape a failed batch leaves.
func TestValidateCatchesAMissingRow(t *testing.T) {
    var source, target = setup(t, func(tx *bbolt.Tx) error { return seedEverything(t, tx) })
    migrateAll(t, source, target)
    if _, err := target.Exec("delete from blocks where height = 963269"); err != nil { t.Fatal(err) }
    if validate(source, target) { t.Error("a missing block was not caught") }
}

// And a row the bucket has nothing to say about, which no per-record comparison
// can see — only the counts can.
func TestValidateCatchesAnExtraRow(t *testing.T) {
    var source, target = setup(t, func(tx *bbolt.Tx) error { return seedEverything(t, tx) })
    migrateAll(t, source, target)
    if _, err := target.Exec(`insert into rates (ts, cents) values (1, 1)`); err != nil { t.Fatal(err) }
    if validate(source, target) { t.Error("a row with no record behind it was not caught") }
}

// The cursors table is migrated but not compared: a cursor is a scan's place and
// the bot moves it, so a database used since the migration would report a
// difference that is not an error.
func TestValidateSkipsCursors(t *testing.T) {
    var source, target = setup(t, func(tx *bbolt.Tx) error { return seedEverything(t, tx) })
    migrateAll(t, source, target)
    if _, err := target.Exec("update cursors set place = 1 where name = 'blocks'"); err != nil { t.Fatal(err) }
    if _, err := target.Exec("insert into cursors (name, place) values ('miners', 7)"); err != nil { t.Fatal(err) }
    if !validate(source, target) { t.Error("cursors were compared; they are the one table that moves") }
}

// An empty source and an empty destination agree.
func TestValidateEmpty(t *testing.T) {
    var source, target = setup(t, func(tx *bbolt.Tx) error { return nil })
    migrateAll(t, source, target)
    if !validate(source, target) { t.Error("two empty databases were reported as differing") }
}

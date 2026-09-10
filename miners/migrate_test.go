package miners

import "encoding/hex"
import "encoding/json"
import "path/filepath"
import "reflect"
import "testing"

import "go.etcd.io/bbolt"

// seedOldFormat writes the three buckets the way they used to be: miners holding
// address → pool name, miners-tag holding tag → pool name, and miners-stat
// holding the aggregates under the pool's name.
func seedOldFormat(t *testing.T, path string) {
    t.Helper()
    var handle, err = bbolt.Open(path, 0600, nil)
    if err != nil { t.Fatalf("open: %v", err) }
    defer handle.Close()
    if err := handle.Update(func(tx *bbolt.Tx) error {
        var b, berr = tx.CreateBucketIfNotExists([]byte("miners"))
        if berr != nil { return berr }
        for addr, name := range map[string]string{"aAnt1": "AntPool", "aAnt2": "AntPool", "aF2": "F2Pool"} {
            if err := b.Put([]byte(addr), []byte(name)); err != nil { return err }
        }
        var tb, terr = tx.CreateBucketIfNotExists([]byte("miners-tag"))
        if terr != nil { return terr }
        for tag, name := range map[string]string{"Mined by AntPool": "AntPool", "/Foundry USA Pool/": "Foundry USA"} {
            if err := tb.Put([]byte(tag), []byte(name)); err != nil { return err }
        }
        var sb, serr = tx.CreateBucketIfNotExists([]byte("miners-stat"))
        if serr != nil { return serr }
        for name, s := range map[string]record{
            "AntPool":     {Blocks: 6, Reward: 1950000000, Fees: 40000000, Work: 3.6e24, LastWork: 6.0e23},
            "Foundry USA": {Blocks: 9, Reward: 2925000000, Fees: 51000000, Work: 5.4e24, LastWork: 6.0e23},
        } {
            var data, merr = json.Marshal(s)
            if merr != nil { return merr }
            if err := sb.Put([]byte(name), data); err != nil { return err }
        }
        return nil
    }); err != nil { t.Fatalf("seed: %v", err) }
}

func open(t *testing.T, path string) *bbolt.DB {
    t.Helper()
    var handle, err = bbolt.Open(path, 0600, nil)
    if err != nil { t.Fatalf("open: %v", err) }
    if err := Init(handle); err != nil { t.Fatalf("init: %v", err) }
    t.Cleanup(func() { handle.Close(); db = nil })
    return handle
}

func recordOf(t *testing.T, handle *bbolt.DB, name string) record {
    t.Helper()
    var r record
    handle.View(func(tx *bbolt.Tx) error {
        var v = tx.Bucket([]byte("miners")).Get([]byte(name))
        if v == nil { t.Fatalf("no record for %s", name) }
        if err := json.Unmarshal(v, &r); err != nil { t.Fatalf("unmarshal %s: %v", name, err) }
        return nil
    })
    return r
}

// The addresses and tags fold into the pool records, the aggregates they are
// folded into survive, and the two buckets they came from are dropped —
// attribution then answers from the record it all landed in.
func TestMigrateFoldsAddressesAndTags(t *testing.T) {
    var path = filepath.Join(t.TempDir(), "miners.db")
    seedOldFormat(t, path)
    var handle = open(t, path)
    var ant = recordOf(t, handle, "AntPool")
    if !reflect.DeepEqual(ant.Addresses, []string{"aAnt1", "aAnt2"}) { t.Errorf("AntPool addresses = %v", ant.Addresses) }
    if !reflect.DeepEqual(ant.Tags, []string{"Mined by AntPool"}) { t.Errorf("AntPool tags = %v", ant.Tags) }
    if ant.Blocks != 6 || ant.Reward != 1950000000 || ant.LastWork != 6.0e23 {
        t.Errorf("AntPool aggregates did not survive: %+v", ant)
    }
    // a pool known only by a tag keeps its aggregates and gains no addresses
    var foundry = recordOf(t, handle, "Foundry USA")
    if len(foundry.Addresses) != 0 || !reflect.DeepEqual(foundry.Tags, []string{"/Foundry USA Pool/"}) {
        t.Errorf("Foundry USA = %+v", foundry)
    }
    if foundry.Blocks != 9 { t.Errorf("Foundry USA blocks = %d, want 9", foundry.Blocks) }
    // a pool with definitions but nothing mined is a record with no statistics
    var f2 = recordOf(t, handle, "F2Pool")
    if f2.Blocks != 0 || !reflect.DeepEqual(f2.Addresses, []string{"aF2"}) { t.Errorf("F2Pool = %+v", f2) }
    handle.View(func(tx *bbolt.Tx) error {
        if tx.Bucket([]byte("miners-tag")) != nil { t.Error("miners-tag survived the migration") }
        if tx.Bucket([]byte("miners-stat")) != nil { t.Error("miners-stat survived the migration") }
        return nil
    })
    // and the address→pool and tag→pool mappings answer from what was folded in
    if got := Name("aAnt2"); got != "AntPool" { t.Errorf("Name(aAnt2) = %q, want AntPool", got) }
    if got := Name("aAnt1x"); got != "" { t.Errorf("Name of an unknown address = %q", got) }
    var script = hex.EncodeToString([]byte("\x03abcd/Foundry USA Pool/\xfa\x01"))
    if got := Attribute([]string{"bc1qUnlisted"}, script); got != "Foundry USA" {
        t.Errorf("Attribute by tag = %q, want Foundry USA", got)
    }
    if got := Top(10); len(got) != 2 || got[0].Name != "Foundry USA" || got[1].Name != "AntPool" {
        t.Errorf("Top = %+v, want Foundry USA then AntPool", got)
    }
}

// Init runs on every start, so a second one must find nothing to do — and a
// third, on a database that never had the old buckets at all.
func TestMigrateRunsOnce(t *testing.T) {
    var path = filepath.Join(t.TempDir(), "miners.db")
    seedOldFormat(t, path)
    var handle = open(t, path)
    for i := 0; i < 3; i++ {
        if err := Init(handle); err != nil { t.Fatalf("init %d: %v", i, err) }
    }
    var ant = recordOf(t, handle, "AntPool")
    if !reflect.DeepEqual(ant.Addresses, []string{"aAnt1", "aAnt2"}) || ant.Blocks != 6 {
        t.Errorf("a repeated Init changed the record: %+v", ant)
    }
    if got := Name("aF2"); got != "F2Pool" { t.Errorf("Name(aF2) = %q after re-running Init", got) }
}

// The two stages are separate transactions, so a crash between them is a real
// state: the addresses are folded in but the miners bucket still holds the pairs
// they came from. The next start must fold them onto the same result rather than
// doubling them, and then finish the move.
func TestMigrateResumesBetweenStages(t *testing.T) {
    var path = filepath.Join(t.TempDir(), "miners.db")
    seedOldFormat(t, path)
    var handle, err = bbolt.Open(path, 0600, nil)
    if err != nil { t.Fatalf("open: %v", err) }
    db = handle
    if err := db.Update(func(tx *bbolt.Tx) error {
        var _, _, _, ferr = fold(tx)
        return ferr
    }); err != nil { t.Fatalf("fold: %v", err) }
    handle.Close()
    db = nil
    var reopened = open(t, path)
    var ant = recordOf(t, reopened, "AntPool")
    if !reflect.DeepEqual(ant.Addresses, []string{"aAnt1", "aAnt2"}) { t.Errorf("addresses doubled: %v", ant.Addresses) }
    if ant.Blocks != 6 { t.Errorf("aggregates lost on the second fold: %+v", ant) }
    reopened.View(func(tx *bbolt.Tx) error {
        if tx.Bucket([]byte("miners-stat")) != nil { t.Error("the interrupted migration did not finish") }
        return nil
    })
}

// A fresh install has none of the old buckets and must not grow them.
func TestFreshInstallHasOneBucket(t *testing.T) {
    var handle = open(t, filepath.Join(t.TempDir(), "miners.db"))
    var names []string
    handle.View(func(tx *bbolt.Tx) error {
        return tx.ForEach(func(name []byte, _ *bbolt.Bucket) error {
            names = append(names, string(name))
            return nil
        })
    })
    if !reflect.DeepEqual(names, []string{"cursors", "miners"}) { t.Errorf("buckets = %v", names) }
}

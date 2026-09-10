package miners

import "encoding/json"

import "go.etcd.io/bbolt"
import "bitnsbot/logging"

// The two buckets the definitions used to live in: miners-tag mapped a coinbase
// tag to a pool name, and the miners bucket itself mapped a coinbase address to
// one, so a pool's name was written down once per address it had ever used. The
// aggregated statistics were keyed by pool name in miners-stat, which is the
// shape everything now has.
var oldTagBucket = []byte("miners-tag")
var oldStatBucket = []byte("miners-stat")

// migrate folds three buckets into one, in the two stages below, and is a no-op
// on every start after the first — the old buckets being gone is the whole
// signal that there is nothing left to fold.
//
// The stages are separate transactions rather than one because fold is written
// to be repeatable: a crash between them leaves the addresses and tags where
// they were, so the next start folds them again onto the same result and then
// finishes the move.
func migrate() error {
    var stale bool
    var err = db.View(func(tx *bbolt.Tx) error {
        stale = tx.Bucket(oldStatBucket) != nil || tx.Bucket(oldTagBucket) != nil
        return nil
    })
    if err != nil { return err }
    if !stale { return nil }
    var pools, addrs, tags int
    if err := db.Update(func(tx *bbolt.Tx) error {
        var ferr error
        pools, addrs, tags, ferr = fold(tx)
        return ferr
    }); err != nil { return err }
    if err := db.Update(move); err != nil { return err }
    logging.Status("miners: folded %d addresses and %d tags into %d pool records", addrs, tags, pools)
    return nil
}

// fold writes each pool's coinbase addresses and tags into its miners-stat
// record, creating one for a pool that has definitions but has mined no block
// this has counted. The lists are *set* from what the two old buckets hold
// rather than appended to, which is what makes running it twice harmless.
//
// A record that does not decode starts from zero, losing its aggregates — the
// same thing that already happens to one on the collector's next flush, since it
// cannot be read to be added to either.
func fold(tx *bbolt.Tx) (pools, addrs, tags int, err error) {
    var stats, aerr = tx.CreateBucketIfNotExists(oldStatBucket)
    if aerr != nil { return 0, 0, 0, aerr }
    var records = map[string]*record{}
    var unreadable int
    var at = func(name string) *record {
        if r := records[name]; r != nil { return r }
        var r = &record{}
        if v := stats.Get([]byte(name)); v != nil && json.Unmarshal(v, r) != nil {
            *r = record{}
            unreadable++
        }
        // both lists are about to be set from the two old buckets, which still
        // hold every address and tag until the second stage empties them — so a
        // fold that already ran is redone rather than appended to
        r.Addresses, r.Tags = nil, nil
        records[name] = r
        return r
    }
    // ForEach walks in key order, so each list comes out sorted with no sorting
    if b := tx.Bucket(bucket); b != nil {
        if err := b.ForEach(func(k, v []byte) error {
            var r = at(string(v))
            r.Addresses = append(r.Addresses, string(k))
            addrs++
            return nil
        }); err != nil { return 0, 0, 0, err }
    }
    if b := tx.Bucket(oldTagBucket); b != nil {
        if err := b.ForEach(func(k, v []byte) error {
            var r = at(string(v))
            r.Tags = append(r.Tags, string(k))
            tags++
            return nil
        }); err != nil { return 0, 0, 0, err }
    }
    for name, r := range records {
        var data, merr = json.Marshal(r)
        if merr != nil { return 0, 0, 0, merr }
        if err := stats.Put([]byte(name), data); err != nil { return 0, 0, 0, err }
    }
    if unreadable > 0 { logging.Warn("miners: %d aggregates could not be read and start again from zero", unreadable) }
    return len(records), addrs, tags, nil
}

// move empties the miners bucket of the address→name pairs it held, refills it
// with the pool records, and drops the two buckets that are now folded into it.
func move(tx *bbolt.Tx) error {
    if err := tx.DeleteBucket(bucket); err != nil { return err }
    var b, err = tx.CreateBucket(bucket)
    if err != nil { return err }
    if stats := tx.Bucket(oldStatBucket); stats != nil {
        if err := stats.ForEach(func(k, v []byte) error { return b.Put(k, v) }); err != nil { return err }
        if err := tx.DeleteBucket(oldStatBucket); err != nil { return err }
    }
    if tx.Bucket(oldTagBucket) != nil { return tx.DeleteBucket(oldTagBucket) }
    return nil
}

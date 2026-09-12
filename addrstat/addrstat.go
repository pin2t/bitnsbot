// Package addrstat keeps on-chain statistics for a fixed set of addresses, so
// /info and the Mini App answer for them without resolving a history per
// request. The set is the three ranked lists the Addresses tab shows — active,
// rich and abandoned — because those are the addresses a reader can reach by
// tapping, and they are exactly the ones whose histories are too long to walk:
// the busiest holds millions of transactions, where the live path caps at ten
// thousand and marks the answer with a "+".
//
// One bucket, addrstat, keyed by address, holding one JSON record each. A
// goroutine walks the chain from genesis filling them in, resuming from its
// place in the shared cursors bucket exactly as the block cache and the miner
// statistics do.
package addrstat

import "context"
import "encoding/json"
import "sync"
import "time"

import "go.etcd.io/bbolt"
import "bitnsbot/addrindex"
import "bitnsbot/cursors"
import "bitnsbot/logging"

var db *bbolt.DB
var bucket = []byte("addrstat")

// interval is how often the collector looks for new blocks once it has caught
// up. A package var so tests can shrink it.
var interval = 10 * time.Minute

// chunkSize bounds how many blocks are accumulated in memory before one
// database flush, the same reasoning as the miners collector: catching up the
// whole chain must not build one giant transaction, and a crash mid-catch-up
// resumes from the last flushed chunk.
var chunkSize int64 = 1000

// Stat is one address's record. Balance and Flow are derived from Recv and Sent
// rather than accumulated on their own, so they cannot drift out of step with
// them; they are stored because a reader of the bucket should not have to know
// which fields are sums and which are arithmetic.
type Stat struct {
    Type    string `json:"type"`
    Balance int64  `json:"balance"`
    Recv    int64  `json:"recv"`
    Sent    int64  `json:"sent"`
    Flow    int64  `json:"flow"`
    Fees    int64  `json:"fees"`
    Txs     int64  `json:"txs"`
    First   int64  `json:"first"`
    Last    int64  `json:"last"`
}

// watched maps the key a script matches by (see addrindex.Key) to the address
// the record is under. It is built once from the bucket and read by the scan for
// every output and every spent prevout on the chain, so the match is a map
// lookup on a short byte string rather than an address encoding.
var watchedMu sync.RWMutex
var watched = map[string]string{}

// Init stores the shared bbolt handle, ensures the bucket exists, and builds the
// lookup the scan matches scripts against.
//
// **The set is whatever the bucket holds.** A key is an address, and the record
// under it is what the scan has gathered about it; a record with no key is not
// created here, so putting an address in — through the database UI, or an
// import — is what adds it to the set. An address added after the scan has
// already passed its history gathers nothing until the scan runs again, which
// means clearing this scan's place in the `cursors` bucket by hand. That is an
// operator's job because the alternative is an automatic rescan of the whole
// chain, which is hours, triggered by a signal nothing can tell apart from an
// address the chain has simply never seen.
func Init(handle *bbolt.DB) error {
    db = handle
    if err := cursors.Init(handle); err != nil { return err }
    var err = db.Update(func(tx *bbolt.Tx) error {
        var _, berr = tx.CreateBucketIfNotExists(bucket)
        if berr != nil { return berr }
        return nil
    })
    if err != nil { return err }
    if err := load(); err != nil { return err }
    return nil
}

// load builds the script lookup from the bucket.
func load() error {
    if db == nil { return nil }
    var index = map[string]string{}
    var err = db.View(func(tx *bbolt.Tx) error {
        return tx.Bucket(bucket).ForEach(func(k, _ []byte) error {
            if key, _, ok := addrindex.Decode(string(k)); ok { index[key] = string(k) }
            return nil
        })
    })
    if err != nil { return err }
    watchedMu.Lock()
    watched = index
    watchedMu.Unlock()
    return nil
}

// Get returns an address's statistics, and false when there are none to give:
// the address is not in the set, or the scan has not reached it yet. A record
// with no transactions in it is the second of those — an address is in the set
// because somebody expects it to have a history, so answering with the empty
// record would present "0 transactions" as a fact about an address the scan has
// simply not got to. The live path answers it instead, as it does for every
// address outside the set.
func Get(addr string) (Stat, bool) {
    if db == nil { return Stat{}, false }
    var s Stat
    var found bool
    db.View(func(tx *bbolt.Tx) error {
        var b = tx.Bucket(bucket)
        if b == nil { return nil }
        var v = b.Get([]byte(addr))
        if v != nil && json.Unmarshal(v, &s) == nil { found = s.Txs > 0 }
        return nil
    })
    return s, found
}

// ForEach walks every record, which is what the Addresses tab's three rankings
// are built from.
func ForEach(fn func(addr string, s Stat)) error {
    if db == nil { return nil }
    return db.View(func(tx *bbolt.Tx) error {
        var b = tx.Bucket(bucket)
        if b == nil { return nil }
        return b.ForEach(func(k, v []byte) error {
            var s Stat
            if json.Unmarshal(v, &s) != nil { return nil }
            fn(string(k), s)
            return nil
        })
    })
}

// Count is how many addresses are watched.
func Count() int {
    watchedMu.RLock()
    defer watchedMu.RUnlock()
    return len(watched)
}

// Start runs the collector: a catch-up to the tip, then again every interval.
// src is the same REST-backed chain source the address index is built from —
// one block and its spent outputs is 1.95 MB against getblock verbosity 3's
// 13.7 MB of JSON, which is what makes a full-chain pass affordable at all.
func Start(src addrindex.Blockchain) {
    go func() {
        for {
            if err := Collect(src); err != nil { logging.Warn("addrstat: %v", err) }
            time.Sleep(interval)
        }
    }()
}

// Collect walks the chain from the cursor to the tip once and returns. Start is
// this on a loop; it is exported for the same reason addrindex.Build is, so a
// caller that wants one pass and an exit code can have one.
func Collect(src addrindex.Blockchain) error {
    if Count() == 0 { return nil } // no addresses to gather for
    var ctx, cancel = context.WithTimeout(context.Background(), 6*time.Hour)
    defer cancel()
    var tip, err = src.Tip(ctx)
    if err != nil { return err }
    var last, ok = cursors.Get(cursors.AddrStat)
    var from int64
    if ok { from = last + 1 }
    var began = from
    for from <= int64(tip) {
        var to = from + chunkSize - 1
        if to > int64(tip) { to = int64(tip) }
        var deltas = map[string]*Stat{}
        for h := from; h <= to; h++ {
            var blk, berr = src.BlockAt(ctx, int(h))
            if berr != nil { return berr }
            apply(deltas, blk)
        }
        if err := flush(deltas, to); err != nil { return err }
        from = to + 1
    }
    if from > began {
        logging.Info("addrstat: collected in blocks %d..%d for %d addresses", began, from-1, Count())
    }
    return nil
}

// apply adds one block's movements to the running chunk. A transaction is
// counted once per address however many of its outputs or inputs name it, which
// is the same dedup the index itself does — an address paid twice by one
// transaction was in one transaction.
//
// The fee is the whole transaction's, charged to every address that spends in
// it: the chain records who funded a transaction, not which of them paid for it,
// and this is what the live path already does.
func apply(deltas map[string]*Stat, blk addrindex.Block) {
    var outputs, ok1 = addrindex.OutputsByTx(blk.Raw)
    var spent, ok2 = addrindex.SpentByTx(blk.Spent)
    var when, ok3 = addrindex.BlockTime(blk.Raw)
    if !ok1 || !ok2 || !ok3 || len(outputs) != len(spent) {
        logging.Warn("addrstat: could not parse block %s", blk.Hash)
        return
    }
    watchedMu.RLock()
    defer watchedMu.RUnlock()
    for i := range outputs {
        var fee int64
        if len(spent[i]) > 0 { // a coinbase spends nothing and pays no fee
            for _, p := range spent[i] { fee += p.Sat }
            for _, p := range outputs[i] { fee -= p.Sat }
        }
        var seen = map[string]bool{}
        var paid = map[string]bool{}
        for _, p := range outputs[i] {
            var s, addr = at(deltas, p.Script)
            if s == nil { continue }
            s.Recv += p.Sat
            touch(s, seen, addr, when)
        }
        for _, p := range spent[i] {
            var s, addr = at(deltas, p.Script)
            if s == nil { continue }
            s.Sent += p.Sat
            if fee > 0 && !paid[addr] {
                paid[addr] = true
                s.Fees += fee
            }
            touch(s, seen, addr, when)
        }
    }
}

// at is the watched record a script belongs to, or nil when the script pays an
// address this does not follow — which is nearly every script on the chain, so
// it is the one lookup that has to stay cheap.
func at(deltas map[string]*Stat, script []byte) (*Stat, string) {
    var key, ok = addrindex.Key(script)
    if !ok { return nil, "" }
    var addr = watched[key]
    if addr == "" { return nil, "" }
    var s = deltas[addr]
    if s == nil {
        s = &Stat{}
        deltas[addr] = s
    }
    return s, addr
}

// touch counts the transaction once for this address and moves the first and
// last dates. Both are a min and a max rather than the first and last seen: a
// block's timestamp may legitimately run up to two hours behind the block before
// it, so the latest block an address appears in is not always the latest time.
func touch(s *Stat, seen map[string]bool, addr string, when int64) {
    if !seen[addr] {
        seen[addr] = true
        s.Txs++
    }
    if when <= 0 { return }
    if s.First == 0 || when < s.First { s.First = when }
    if when > s.Last { s.Last = when }
}

// flush merges a chunk into the records and advances the cursor in one
// transaction, so an interrupted catch-up resumes from the last flushed chunk
// rather than counting a block twice.
func flush(deltas map[string]*Stat, last int64) error {
    return db.Update(func(tx *bbolt.Tx) error {
        var b = tx.Bucket(bucket)
        for addr, d := range deltas {
            var s Stat
            if v := b.Get([]byte(addr)); v != nil {
                if json.Unmarshal(v, &s) != nil { s = Stat{} }
            }
            s.Recv += d.Recv
            s.Sent += d.Sent
            s.Fees += d.Fees
            s.Txs += d.Txs
            if d.First > 0 && (s.First == 0 || d.First < s.First) { s.First = d.First }
            if d.Last > s.Last { s.Last = d.Last }
            s.Balance, s.Flow = s.Recv-s.Sent, s.Recv+s.Sent
            var data, err = json.Marshal(s)
            if err != nil { return err }
            if err := b.Put([]byte(addr), data); err != nil { return err }
        }
        return cursors.Set(tx, cursors.AddrStat, last)
    })
}

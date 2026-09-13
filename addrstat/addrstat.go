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
import "sync"
import "time"

import "database/sql"
import "bitnsbot/addrindex"
import "bitnsbot/cursors"
import "bitnsbot/logging"
import "bitnsbot/signals"

var db *sql.DB

// interval is the longest the collector waits once it has caught up; a block
// notification cuts it short. A package var so tests can shrink it.
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

// Init stores the shared handle and builds the lookup the scan matches scripts
// against.
//
// **The set is whatever the addrstat table holds.** A row's key is an address and
// the rest of it is what the scan has gathered, so inserting an address is what
// adds it to the set — a `where txs = 0` is what asks which of them the scan has
// still to reach. An address added after the scan has passed its history gathers
// nothing until the scan runs again, which means clearing this scan's place in the
// cursors table by hand. That is an operator's job because the alternative is an
// automatic rescan of the whole chain, hours of it, triggered by a signal nothing
// can tell apart from an address the chain has simply never seen.
func Init(handle *sql.DB) error {
    db = handle
    return load()
}

// load builds the script lookup from the bucket.
func load() error {
    if db == nil { return nil }
    var index = map[string]string{}
    var rows, err = db.Query("select addr from addrstat")
    if err != nil { return err }
    for rows.Next() {
        var addr string
        if err := rows.Scan(&addr); err != nil {
            rows.Close()
            return err
        }
        if key, _, ok := addrindex.Decode(addr); ok { index[key] = addr }
    }
    rows.Close()
    if err := rows.Err(); err != nil { return err }
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
    var err = db.QueryRow(`select type, balance, recv, sent, flow, fees, txs, first, last
        from addrstat where addr = ?`, addr).Scan(&s.Type, &s.Balance, &s.Recv, &s.Sent, &s.Flow,
        &s.Fees, &s.Txs, &s.First, &s.Last)
    if err != nil { return Stat{}, false }
    return s, s.Txs > 0
}

// ForEach walks every record, which is what the Addresses tab's three rankings
// are built from.
func ForEach(fn func(addr string, s Stat)) error {
    if db == nil { return nil }
    var rows, err = db.Query(`select addr, type, balance, recv, sent, flow, fees, txs, first, last from addrstat`)
    if err != nil { return err }
    defer rows.Close()
    for rows.Next() {
        var addr string
        var s Stat
        if err := rows.Scan(&addr, &s.Type, &s.Balance, &s.Recv, &s.Sent, &s.Flow, &s.Fees,
            &s.Txs, &s.First, &s.Last); err != nil { return err }
        fn(addr, s)
    }
    return rows.Err()
}

// Count is how many addresses are watched.
func Count() int {
    watchedMu.RLock()
    defer watchedMu.RUnlock()
    return len(watched)
}

// Start runs the collector: a catch-up to the tip, then again on every block
// notification and every interval, whichever comes first.
// src is the same REST-backed chain source the address index is built from —
// one block and its spent outputs is 1.95 MB against getblock verbosity 3's
// 13.7 MB of JSON, which is what makes a full-chain pass affordable at all.
func Start(src addrindex.Blockchain) {
    go func() {
        var wake = signals.Subscribe(signals.Block)
        for {
            if err := Collect(src); err != nil { logging.Warn("addrstat: %v", err) }
            select {
            case <-time.After(interval):
            case <-wake:
            }
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
        // the three ranked address lists are built from these records, so they
        // are told the moment the figures they rank have moved rather than
        // waiting out their own interval. Only when a block was actually
        // scanned: a pass that found nothing to do has nothing to announce.
        signals.Send(signals.AddrStat)
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
// The delta is added to the row in SQL rather than read, added to in Go and
// written back: one statement per address instead of a query and an update, and
// the arithmetic is where the row is. Balance and Flow are derived in the same
// statement from the sums they follow from, so they cannot drift out of step with
// them. `first` takes the earlier of the two only when there is one — a fresh row
// has 0 there, which is not earlier than anything.
//
// An address is only ever here because a row exists for it, so this updates and
// never inserts: a delta for an address the table does not hold is one the scan
// should not have gathered, and creating a row for it would quietly add it to the
// set.
func flush(deltas map[string]*Stat, last int64) error {
    if db == nil { return nil }
    var tx, err = db.Begin()
    if err != nil { return err }
    var stmt, perr = tx.Prepare(`update addrstat set
        recv = recv + ?, sent = sent + ?, fees = fees + ?, txs = txs + ?,
        first = case when ? > 0 and (first = 0 or ? < first) then ? else first end,
        last = max(last, ?),
        balance = recv + ? - (sent + ?), flow = recv + ? + sent + ?
        where addr = ?`)
    if perr != nil {
        tx.Rollback()
        return perr
    }
    for addr, d := range deltas {
        if _, err := stmt.Exec(d.Recv, d.Sent, d.Fees, d.Txs,
            d.First, d.First, d.First, d.Last,
            d.Recv, d.Sent, d.Recv, d.Sent, addr); err != nil {
            stmt.Close()
            tx.Rollback()
            return err
        }
    }
    if err := stmt.Close(); err != nil {
        tx.Rollback()
        return err
    }
    if err := cursors.Set(tx, cursors.AddrStat, last); err != nil {
        tx.Rollback()
        return err
    }
    return tx.Commit()
}

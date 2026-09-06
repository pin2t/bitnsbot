package main

import "database/sql"
import "strconv"
import "strings"

// abaStore is the SQLite database ababuild writes. `abandoned` is the table that
// was asked for — the addresses holding coins whose owner has been silent
// longest — and `balances` is the state it is selected from: every funded
// script, what it holds, and the last block time it was spent from and paid at.
//
// The state is what makes the answer exact as well as resumable. One address can
// be paid by more than one script, and those scripts hash to different shards,
// so no shard can decide on its own whether an address belongs in the top ten
// thousand; every funded script is written down first and the ranking is one
// group-by over the lot. Carrying on from a stored height comes free with it.
//
// `meta` records what the two tables mean: `height` is the block the balances
// stand at, `abandoned` the height that table was last selected at, and `min`
// and `top` the thresholds it was selected with — so changing either rebuilds it
// rather than leaving rows that do not match the flags.
type abaStore struct {
    store
}

// abandonedDDL is the table as it was asked for, and the only place it is
// written down: shortlist drops and recreates it from this same line, so a
// rebuild cannot leave a different table than a first run created.
//
// It was asked for as `create or replace table`, which SQLite does not have —
// it has that form for views and triggers only. Dropping and recreating is the
// same thing said in the dialect, and is what a rebuild has to do anyway.
const abandonedDDL = `create table abandoned (addr text primary key, balance integer not null, lastTx integer not null)`

// abaBalancesDDL is used twice, since a run builds the new state beside the old
// one and swaps it in at the end. spent and paid are block times, zero meaning
// it never happened — a script that has never been spent from is the ordinary
// case here, and the one this command exists to find.
func abaBalancesDDL(name string) string {
    return `create table ` + name + ` (script blob not null, addr text not null,
        balance integer not null, spent integer not null, paid integer not null)`
}

func openAba(path string) (*abaStore, error) {
    var s, err = openStore(path, abandonedDDL, abaBalancesDDL("balances"), metaDDL)
    if err != nil { return nil, err }
    return &abaStore{store: s}, nil
}

// each hands over every balance the last run stored, which is how a new run
// starts from that height rather than from genesis. The two times come back with
// it, or a carried-forward script would look like one first seen today.
func (s *abaStore) each(f func(script []byte, balance, spent, paid int64) error) error {
    var rows, err = s.db.Query("select script, balance, spent, paid from balances")
    if err != nil { return err }
    defer rows.Close()
    for rows.Next() {
        var script []byte
        var balance, spent, paid int64
        if err := rows.Scan(&script, &balance, &spent, &paid); err != nil { return err }
        if err := f(script, balance, spent, paid); err != nil { return err }
    }
    return rows.Err()
}

// abaRow is one funded script on its way into the state, built by whichever
// worker summed the shard it came from.
type abaRow struct {
    script []byte
    addr   string
    m      move
}

// abaState accumulates the new balances table. It is written under a different
// name and swapped in at the end, so a run that dies partway leaves the previous
// state — and the height that describes it — exactly as it was.
//
// It is written from several goroutines, one per shard being summed, so every
// method here is called under the caller's lock; see combine in ababuild.go.
type abaState struct {
    store   *abaStore
    tx      *sql.Tx
    pending []interface{}
    rows    int
    since   int
}

func (s *abaStore) newState() (*abaState, error) {
    if _, err := s.db.Exec(`drop table if exists balances_new`); err != nil { return nil, err }
    if _, err := s.db.Exec(abaBalancesDDL("balances_new")); err != nil { return nil, err }
    var st = &abaState{store: s}
    var err = st.begin()
    return st, err
}

func (s *abaState) begin() error {
    var tx, err = s.store.db.Begin()
    s.tx = tx
    return err
}

// add records one script. addr is what it pays to, or "" for a script that is no
// address at all — kept anyway, since it holds coins the next run has to carry
// forward, and left out of the answer table by the select that builds it.
func (s *abaState) add(r abaRow) error {
    s.pending = append(s.pending, r.script, r.addr, r.m.sat, r.m.spent, r.m.paid)
    s.rows++
    if s.rows%rowsPerStatement != 0 { return nil }
    return s.write()
}

func (s *abaState) write() error {
    if len(s.pending) == 0 { return nil }
    var n = len(s.pending) / 5
    var values = strings.TrimSuffix(strings.Repeat("(?,?,?,?,?),", n), ",")
    var _, err = s.tx.Exec(
        "insert into balances_new(script, addr, balance, spent, paid) values "+values, s.pending...)
    s.pending = s.pending[:0]
    s.since += n
    if err != nil { return err }
    if s.since < rowsPerTransaction { return nil }
    s.since = 0
    if err := s.tx.Commit(); err != nil { return err }
    return s.begin()
}

// commit finishes the new state and swaps it in, together with the height and
// block it was built to, in one transaction — they are only meaningful together.
// The hash is what a later run checks the chain against before carrying these
// balances forward, since a block that was reorged away since would leave them
// quietly counting coins that no longer exist.
func (s *abaState) commit(height int, hash string) error {
    if err := s.write(); err != nil { return err }
    if err := s.tx.Commit(); err != nil { return err }
    var tx, err = s.store.db.Begin()
    if err != nil { return err }
    defer tx.Rollback()
    if _, err := tx.Exec(`drop table balances`); err != nil { return err }
    if _, err := tx.Exec(`alter table balances_new rename to balances`); err != nil { return err }
    if err := setMeta(tx, "height", strconv.Itoa(height)); err != nil { return err }
    if err := setMeta(tx, "hash", hash); err != nil { return err }
    return tx.Commit()
}

func (s *abaState) rollback() {
    if s.tx != nil { s.tx.Rollback() }
    s.store.db.Exec(`drop table if exists balances_new`)
}

// shortlist rebuilds abandoned from the stored state: the addresses holding at
// least min satoshi, ranked by how long ago each last moved coins, and the first
// top of them kept.
//
// lastTx is the address's last spend, and where it has never spent at all, the
// last time it was paid. A spend is the only evidence anybody still holds the
// key — an address can be paid by a stranger long after its owner is gone — so
// silence on the spending side is what the ranking measures, and an address that
// has never spent has been silent since the coins arrived.
//
// The balances are summed per address rather than read row by row, because one
// address can be paid by more than one script: an early miner paid by `<pubkey>
// OP_CHECKSIG` and later by an ordinary pay-to-pubkey-hash holds both under the
// same address, and there are hundreds of thousands of them on mainnet. Those
// are also exactly the addresses this list is about, so taking the later of the
// two scripts' dates matters as much as adding their coins.
func (s *abaStore) shortlist(min int64, top, height int) (int, error) {
    if _, err := s.db.Exec(`drop table if exists abandoned`); err != nil { return 0, err }
    if _, err := s.db.Exec(abandonedDDL); err != nil { return 0, err }
    var res, err = s.db.Exec(`insert into abandoned(addr, balance, lastTx)
        select addr, sum(balance) as bal,
               case when max(spent) > 0 then max(spent) else max(paid) end as last
        from balances where addr <> ''
        group by addr having bal >= ?
        order by last, addr limit ?`, min, top)
    if err != nil { return 0, err }
    var rows, rerr = res.RowsAffected()
    if rerr != nil { return 0, rerr }
    var tx, terr = s.db.Begin()
    if terr != nil { return int(rows), terr }
    defer tx.Rollback()
    if err := setMeta(tx, "abandoned", strconv.Itoa(height)); err != nil { return int(rows), err }
    // the thresholds the table was built with, so changing either and running
    // again rebuilds it instead of reporting that there is nothing to do
    if err := setMeta(tx, "min", strconv.FormatInt(min, 10)); err != nil { return int(rows), err }
    if err := setMeta(tx, "top", strconv.Itoa(top)); err != nil { return int(rows), err }
    return int(rows), tx.Commit()
}

// totals is what the run reports at the end, read back from what it wrote rather
// than from what it thinks it wrote. The two dates are the span of the list: the
// oldest is the most abandoned address on the chain, and the newest is how far
// down the ranking top reaches.
func (s *abaStore) totals() (rows, sat, oldest, newest int64, err error) {
    err = s.db.QueryRow(`select count(*), coalesce(sum(balance), 0),
        coalesce(min(lastTx), 0), coalesce(max(lastTx), 0) from abandoned`).Scan(&rows, &sat, &oldest, &newest)
    return rows, sat, oldest, newest, err
}

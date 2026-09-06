package main

import "database/sql"
import "strconv"
import "strings"

// richStore is the SQLite database richbuild writes. `rich` is the table that
// was asked for — one row per address that holds coins — and `balances` is the
// state it is built from: the same figures keyed by the scriptPubKey they were
// summed against, so a later run carries on from this run's height instead of
// walking the chain again, and so a script with no address (a bare multisig, an
// OP_RETURN) still keeps its coins where the next run can find them.
//
// `meta` records what those two tables mean: `height` is the block the balances
// stand at, `rich` the height rich was last built from. They are separate
// because rich is rebuilt from balances rather than accumulated, so a run
// interrupted between the two finishes the job next time instead of rescanning.
//
// Neither table is indexed beyond what it needs. rich carries the primary key
// the schema names; balances carries none at all, because nothing ever looks a
// row up in it — it is written once and read start to finish.
type richStore struct {
    store
}

// richDDL is the table as it was asked for, and the only place it is written
// down: materialize drops and recreates rich from this same line, so the table a
// rebuild leaves cannot differ from the one a first run created.
const richDDL = `create table rich (addr text primary key, balance integer not null)`

// balancesDDL is used twice, since a run builds the new state beside the old one
// and swaps it in at the end.
func balancesDDL(name string) string {
    return `create table ` + name + ` (script blob not null, addr text not null, balance integer not null)`
}

func openRich(path string) (*richStore, error) {
    var s, err = openStore(path, richDDL, balancesDDL("balances"), metaDDL)
    if err != nil { return nil, err }
    return &richStore{store: s}, nil
}

// each hands over every balance the last run stored, which is how a new run
// starts from that height rather than from genesis.
func (s *richStore) each(f func(script []byte, balance int64) error) error {
    var rows, err = s.db.Query("select script, balance from balances")
    if err != nil { return err }
    defer rows.Close()
    for rows.Next() {
        var script []byte
        var balance int64
        if err := rows.Scan(&script, &balance); err != nil { return err }
        if err := f(script, balance); err != nil { return err }
    }
    return rows.Err()
}

// rowsPerStatement is how many rows one insert carries. Measured here, the
// driver's cost per row falls from 9.1 µs to 5.2 µs going from one row per
// statement to five hundred, and no further after that.
const rowsPerStatement = 500

// rowsPerTransaction bounds how much a single commit has to write, so the WAL
// does not grow to the size of the whole table before it is checkpointed.
const rowsPerTransaction = 500000

// state accumulates the new balances table. It is written under a different name
// and swapped in at the end, so a run that dies partway leaves the previous
// state — and the height that describes it — exactly as it was.
type state struct {
    store   *richStore
    tx      *sql.Tx
    pending []interface{}
    rows    int
    since   int
}

func (s *richStore) newState() (*state, error) {
    if _, err := s.db.Exec(`drop table if exists balances_new`); err != nil { return nil, err }
    if _, err := s.db.Exec(balancesDDL("balances_new")); err != nil { return nil, err }
    var st = &state{store: s}
    var err = st.begin()
    return st, err
}

func (s *state) begin() error {
    var tx, err = s.store.db.Begin()
    s.tx = tx
    return err
}

// add records one script's balance. addr is what it pays to, or "" for a script
// that is no address at all — kept anyway, since it holds coins that the next
// run has to carry forward.
func (s *state) add(script []byte, addr string, balance int64) error {
    s.pending = append(s.pending, append([]byte(nil), script...), addr, balance)
    s.rows++
    if s.rows%rowsPerStatement != 0 { return nil }
    return s.write()
}

func (s *state) write() error {
    if len(s.pending) == 0 { return nil }
    var n = len(s.pending) / 3
    var values = strings.TrimSuffix(strings.Repeat("(?,?,?),", n), ",")
    var _, err = s.tx.Exec("insert into balances_new(script, addr, balance) values "+values, s.pending...)
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
func (s *state) commit(height int, hash string) error {
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

func (s *state) rollback() {
    if s.tx != nil { s.tx.Rollback() }
    s.store.db.Exec(`drop table if exists balances_new`)
}

// materialize rebuilds rich from the stored balances: every address holding at
// least min satoshi, and nothing that is not an address at all.
//
// The balances are summed per address rather than copied row for row, because
// one address can be paid by more than one script — an early miner paid by
// `<pubkey> OP_CHECKSIG` and later by an ordinary pay-to-pubkey-hash holds both
// under the same address, and there are hundreds of thousands of them on
// mainnet. Copying instead of summing collides on rich's primary key, which is
// how this was found: a real run to block 200000 failed on it.
//
// The rows go in in address order, because rich is keyed by address and an
// ordered insert appends to that key's index instead of seeking around it — over
// tens of millions of rows that is the difference between minutes and an
// afternoon. Dropping the table rather than deleting from it costs nothing and
// leaves no rows behind to be checked.
func (s *richStore) materialize(min int64, height int) (int, error) {
    if _, err := s.db.Exec(`drop table if exists rich`); err != nil { return 0, err }
    if _, err := s.db.Exec(richDDL); err != nil { return 0, err }
    var res, err = s.db.Exec(`insert into rich(addr, balance)
        select addr, sum(balance) from balances where addr <> ''
        group by addr having sum(balance) >= ? order by addr`, min)
    if err != nil { return 0, err }
    var rows, rerr = res.RowsAffected()
    if rerr != nil { return 0, rerr }
    var tx, terr = s.db.Begin()
    if terr != nil { return int(rows), terr }
    defer tx.Rollback()
    if err := setMeta(tx, "rich", strconv.Itoa(height)); err != nil { return int(rows), err }
    // the threshold rich was built with, so changing -min and running again
    // rebuilds it instead of reporting that there is nothing to do
    if err := setMeta(tx, "min", strconv.FormatInt(min, 10)); err != nil { return int(rows), err }
    return int(rows), tx.Commit()
}

// totals is what the run reports at the end, read back from what it wrote rather
// than from what it thinks it wrote.
func (s *richStore) totals() (rows int64, sat int64, err error) {
    err = s.db.QueryRow("select count(*), coalesce(sum(balance), 0) from rich").Scan(&rows, &sat)
    return rows, sat, err
}

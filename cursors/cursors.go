// Package cursors is the one table every scan over the chain keeps its place in.
// A scan resumes where it stopped, so its place is the only thing standing
// between a restart and starting over.
//
// Every one of them is the same thing — a block height — so they share one table,
// keyed by the scan's own name:
//
//	blocks     the block-info cache's backfill (blocks.go)
//	miners     the per-pool statistics collector (miners/stats.go)
//	addrstat   the per-address statistics collector (addrstat/)
//
// The address index keeps its place in its own bbolt file, beside the touches it
// describes, rather than here — see the addrindex package.
package cursors

import "database/sql"

// The scans that keep a place here. Constants rather than strings at the call
// sites: a name that does not match is not an error, it is a scan that silently
// starts from the beginning, which for a chain scan is hours of work.
const Blocks = "blocks"
const Miners = "miners"
const AddrStat = "addrstat"

var db *sql.DB

// Init is called from openDB, before the packages that keep a place here.
func Init(handle *sql.DB) error {
    db = handle
    return nil
}

// Get reads one scan's place. Not found is not zero: a scan that has never run
// starts somewhere of its own choosing — genesis for the block cache, height 1
// for the miner statistics — which is not where a scan that stopped at height 0
// resumes.
func Get(name string) (int64, bool) {
    if db == nil { return 0, false }
    var v int64
    var err = db.QueryRow("select place from cursors where name = ?", name).Scan(&v)
    if err != nil { return 0, false }
    return v, true
}

// Set writes one inside the caller's transaction, which is the whole point of
// taking a tx: a scan advances its place in the same commit as the batch that
// reached it, so a crash between the two cannot skip work or repeat it.
func Set(tx *sql.Tx, name string, v int64) error {
    var _, err = tx.Exec("insert into cursors (name, place) values (?, ?) "+
        "on conflict(name) do update set place = excluded.place", name, v)
    return err
}

// Delete forgets a scan's place inside the caller's transaction, so the scan
// starts from its own beginning again.
func Delete(tx *sql.Tx, name string) error {
    var _, err = tx.Exec("delete from cursors where name = ?", name)
    return err
}

package main

import "database/sql"
import "fmt"
import "os"
import "path/filepath"
import "strconv"
import "strings"

import _ "modernc.org/sqlite"

// store is what the two SQLite-writing commands keep the same way: one
// connection, the pragmas that shape it for a bulk load that has to survive the
// run, and a meta table saying what the rest of the database means. richbuild
// and ababuild each embed it and add their own tables — see richdb.go and
// abadb.go for what those hold.
type store struct {
    db *sql.DB
}

// pragmas prepare the database for this job's shape: a few tens of millions of
// rows written in bulk, and — unlike tosqlite's one-shot migration — a state
// that is meant to survive the run, so the journal stays on. WAL with
// synchronous=NORMAL is what makes a committed height durable without paying an
// fsync per batch; page_size leads, because it is fixed when the file header is
// written and a later one is silently ignored.
var pragmas = []string{
    "PRAGMA page_size=16384",
    "PRAGMA journal_mode=WAL",
    "PRAGMA synchronous=NORMAL",
    "PRAGMA cache_size=-262144",
}

const metaDDL = `create table meta (key text primary key, value text not null)`

// openStore opens the database and creates each table it is given that is not
// there yet, so a first run builds the file and a later one carries on with it.
func openStore(path string, ddls ...string) (store, error) {
    // Rebuilding the answer table sorts every stored balance by address, which
    // for a mainnet run is gigabytes of scratch. SQLite would put that in /tmp,
    // which on this repo's own machine is a memory-backed filesystem — so it
    // goes beside the database, on whatever disk was big enough to hold it. This
    // has to happen before SQLite is first used, not merely before the sort:
    // modernc's libc copies the environment once, on the first call into it, and
    // never looks again.
    if dir := filepath.Dir(path); dir != "" { os.Setenv("SQLITE_TMPDIR", dir) }
    var db, err = sql.Open("sqlite", "file:"+path)
    if err != nil { return store{}, err }
    // one connection, so the pragmas and the transactions below apply to the
    // same handle rather than to whichever of a pool a statement lands on
    db.SetMaxOpenConns(1)
    for _, p := range pragmas {
        if _, err := db.Exec(p); err != nil {
            db.Close()
            return store{}, fmt.Errorf("%s: %w", p, err)
        }
    }
    for _, ddl := range ddls {
        if err := ensure(db, ddl); err != nil {
            db.Close()
            return store{}, fmt.Errorf("%s: %w", ddl, err)
        }
    }
    return store{db: db}, nil
}

// ensure creates a table only when it is missing, out of the same DDL a rebuild
// uses — the alternative is a second copy of each schema wearing "if not
// exists", free to drift from the first.
func ensure(db *sql.DB, ddl string) error {
    var _, err = db.Exec(strings.Replace(ddl, "create table ", "create table if not exists ", 1))
    return err
}

func (s *store) close() error { return s.db.Close() }

// meta reads one marker, reporting whether it is there at all — an absent height
// is a database that has never been built, which is not the same as one built up
// to block 0, and an absent hash is one written before this run's code kept one.
func (s *store) meta(key string) (string, bool, error) {
    var text string
    var err = s.db.QueryRow("select value from meta where key = ?", key).Scan(&text)
    if err == sql.ErrNoRows { return "", false, nil }
    if err != nil { return "", false, err }
    return text, true, nil
}

func (s *store) height(key string) (int, bool, error) {
    var text, ok, err = s.meta(key)
    if err != nil || !ok { return 0, ok, err }
    var h, cerr = strconv.Atoi(text)
    if cerr != nil { return 0, false, cerr }
    return h, true, nil
}

func setMeta(tx *sql.Tx, key, value string) error {
    var _, err = tx.Exec(`insert into meta(key, value) values(?, ?)
        on conflict(key) do update set value = excluded.value`, key, value)
    return err
}

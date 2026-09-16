// Command csvimport loads CSV files into a SQLite database, one table per file —
// the way a table built elsewhere (a rich list, an abandoned-coins ranking) is
// handed to the bot.
//
// Usage:
//
//	csvimport -db bitnsbot.sqlite active.csv abandoned.csv rich.csv
//
// Each file lands in the table its own name gives — active.csv into active — and
// its header row names the columns the values go into. A table that is not there
// yet is created with those columns, the first of them its primary key, so a
// key,value export becomes a key,value table. An existing table is merged into
// rather than replaced: a row whose key the file names is overwritten, one it
// does not name is left alone.
package main

import "database/sql"
import "encoding/csv"
import "flag"
import "fmt"
import "io"
import "os"
import "path/filepath"
import "strings"
import _ "modernc.org/sqlite"
import "bitnsbot/logging"

var dbPath = flag.String("db", "", "path to the SQLite database (required; created when missing)")
var batch = flag.Int("batch", 100000, "rows per write transaction")

// every file is checked before anything is written, so a mistyped name is
// reported instead of leaving half the import done
//
// busy_timeout, so importing into the database of a bot that is running waits
// for its writes rather than failing on them
func main() {
    flag.Usage = func() {
        fmt.Fprintf(flag.CommandLine.Output(), "Usage: %s -db <database> <file.csv> [file.csv ...]\n", os.Args[0])
        flag.PrintDefaults()
    }
    flag.Parse()
    if *dbPath == "" { logging.Fatal("-db is required") }
    if flag.NArg() == 0 { logging.Fatal("at least one CSV file is required") }
    if *batch < 1 { logging.Fatal("-batch must be at least 1") }
    for _, path := range flag.Args() {
        if _, err := os.Stat(path); err != nil { logging.Fatal("%v", err) }
    }
    var db, err = sql.Open("sqlite", "file:"+*dbPath+"?_pragma=busy_timeout(10000)")
    if err != nil { logging.Fatal("open %s: %v", *dbPath, err) }
    defer db.Close()
    for _, path := range flag.Args() {
        var name = tableName(path)
        var put, over, ierr = importFile(db, path, name)
        if ierr != nil { logging.Fatal("%s: %v", path, ierr) }
        logging.Status("%s: %d rows into %s (%d overwritten)", path, put, name, over)
    }
}

// tableName is the file's own name without its extension, so active.csv names
// the active table and a path says nothing about where the rows go.
func tableName(path string) string {
    var base = filepath.Base(path)
    return strings.TrimSuffix(base, filepath.Ext(base))
}

// quote makes a name from a file or a header safe to put in a statement: SQLite
// quotes an identifier in double quotes and escapes one inside by doubling it.
func quote(name string) string {
    return `"` + strings.ReplaceAll(name, `"`, `""`) + `"`
}

// importFile reads one file into one table, committing every -batch rows. That
// is not atomic, and deliberately: a chain-sized table does not fit in one
// transaction, and since a row is written by key the whole file can simply be
// imported again after a failure.
//
// The header is required, and a bad one is fatal: a column with no name, or the
// same one twice, is a file whose meaning is not known. A header naming a column
// an existing table does not have fails when the statement is prepared, before a
// row is written.
//
// The table is created up front, so an empty file still leaves it behind rather
// than depending on there being a batch to write. Its columns carry no type, so a
// value is stored as the CSV's text, exactly as the file has it; an existing
// table's own column types convert it the way SQLite converts anything inserted.
//
// The update names every column but the key, and a header with nothing but the
// key has nothing to update, so an existing row is left as it is.
func importFile(db *sql.DB, path, name string) (int, int, error) {
    var f, err = os.Open(path)
    if err != nil { return 0, 0, err }
    defer f.Close()
    var r = csv.NewReader(f)
    var head, herr = r.Read()
    if herr == io.EOF { return 0, 0, fmt.Errorf("empty file, expected a header naming the columns") }
    if herr != nil { return 0, 0, herr }
    var cols = make([]string, len(head))
    var seen = map[string]bool{}
    for i, h := range head {
        if h == "" { return 0, 0, fmt.Errorf("column %d of the header has no name", i+1) }
        if seen[strings.ToLower(h)] { return 0, 0, fmt.Errorf("the header names %q twice", h) }
        seen[strings.ToLower(h)] = true
        cols[i] = quote(h)
    }
    var create = "create table if not exists " + quote(name) + " (" + cols[0] + " PRIMARY KEY"
    if len(cols) > 1 { create += ", " + strings.Join(cols[1:], ", ") }
    if _, err := db.Exec(create + ")"); err != nil { return 0, 0, err }
    var insert = "insert into " + quote(name) + " (" + strings.Join(cols, ", ") + ") values (?" +
        strings.Repeat(", ?", len(cols)-1) + ")"
    var sets []string
    for _, c := range cols[1:] { sets = append(sets, c+" = excluded."+c) }
    if len(sets) == 0 {
        insert += " on conflict do nothing"
    } else {
        insert += " on conflict do update set " + strings.Join(sets, ", ")
    }
    var rows [][]string
    var put, over int
    for {
        var rec, rerr = r.Read()
        if rerr != nil && rerr != io.EOF { return put, over, rerr }
        if rerr == nil {
            if rec[0] == "" {
                var line, _ = r.FieldPos(0)
                return put, over, fmt.Errorf("line %d: empty key", line)
            }
            rows = append(rows, rec)
        }
        if len(rows) == *batch || (rerr == io.EOF && len(rows) > 0) {
            var n, werr = write(db, name, insert, rows)
            if werr != nil { return put, over, werr }
            put += len(rows)
            over += n
            rows = rows[:0]
        }
        if rerr == io.EOF { return put, over, nil }
    }
}

// write commits one batch and reports how many of its rows landed on a key the
// table already held — the count is what tells a merge into a populated table
// apart from a fresh load, which is the one thing this tool can silently get
// wrong. It is what the batch did not add: an upsert reports a change for an
// insert and an update alike, so the table's size before and after is what tells
// them apart.
func write(db *sql.DB, name, insert string, rows [][]string) (int, error) {
    var tx, err = db.Begin()
    if err != nil { return 0, err }
    defer tx.Rollback()
    var before, after int
    var count = "select count(*) from " + quote(name)
    if err := tx.QueryRow(count).Scan(&before); err != nil { return 0, err }
    var stmt, perr = tx.Prepare(insert)
    if perr != nil { return 0, perr }
    defer stmt.Close()
    var args = make([]any, len(rows[0]))
    for _, row := range rows {
        for i, v := range row { args[i] = v }
        if _, err := stmt.Exec(args...); err != nil { return 0, err }
    }
    if err := tx.QueryRow(count).Scan(&after); err != nil { return 0, err }
    return len(rows) - (after - before), tx.Commit()
}

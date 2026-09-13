package main

import "bytes"
import "database/sql"
import "fmt"
import "strings"
import "time"

import "go.etcd.io/bbolt"
import "bitnsbot/logging"

// validate walks the bbolt source again and checks that the SQLite database holds
// exactly what it says: the same rows, with the same values in them.
//
// It is the migration's mappings that do the walking — the same `copy` functions,
// handed a sink that compares instead of inserting — so the two cannot disagree
// about what a record should have become. What that buys is a check of the *file*:
// that every record reached it, that nothing was lost to a failed batch, and that
// no row holds something other than what the bucket does. What it cannot catch is
// a mapping that is wrong in the same way twice; that is what the tests pin.
//
// The cost is a second read of the whole source, which for the address index is
// hours — the same hours the migration took.
func validate(source *bbolt.DB, target *sql.DB) bool {
    var began = time.Now()
    var ok = true
    for _, t := range tables {
        if !t.check {
            logging.Status("%s: not compared", t.name)
            continue
        }
        var c = &comparer{db: target, table: t.name, cols: t.cols, keys: t.keys}
        var rows, skipped, err = t.copy(source, c)
        if err != nil {
            logging.Err("%s: %v", t.name, err)
            ok = false
            continue
        }
        // Rows in the table that the source has nothing to say about: a record
        // deleted from the bucket since the migration, or a row from somewhere
        // else entirely. Counted rather than listed, there being no key to name.
        var stored int
        if err := target.QueryRow("select count(*) from " + t.name).Scan(&stored); err != nil {
            logging.Err("%s: count: %v", t.name, err)
            ok = false
            continue
        }
        // Every record the bucket holds either matched, differed, or was not
        // there at all, so the rows those account for is rows-missing — and
        // whatever the table holds beyond that came from somewhere else.
        var extra = stored - (rows - c.missing)
        if c.missing == 0 && c.differing == 0 && extra == 0 {
            var note string
            if skipped > 0 { note = fmt.Sprintf(", %d unreadable records skipped on both sides", skipped) }
            logging.Status("%s: %d rows match%s", t.name, rows, note)
            continue
        }
        ok = false
        logging.Err("%s: %d records in the bucket, %d rows in the table — %d missing, %d differing, %d with no record behind them",
            t.name, rows, stored, c.missing, c.differing, extra)
    }
    logging.Status("validated %s against %s in %s", *dst, *src, time.Since(began).Round(time.Millisecond))
    return ok
}

// comparer is the sink validate drives a copy with: every row the mapping emits
// is looked up by its primary key and compared column by column.
type comparer struct {
    db        *sql.DB
    table     string
    cols      []string
    keys      int
    stmt      *sql.Stmt
    missing   int
    differing int
    reported  int
}

// reportLimit is how many differences are named before the rest are only counted.
// A table that is wrong is usually wrong in bulk, and the first few say what kind
// of wrong it is.
const reportLimit = 5

func (c *comparer) add(args ...any) error {
    if c.stmt == nil {
        var where = make([]string, c.keys)
        for i := 0; i < c.keys; i++ { where[i] = c.cols[i] + " = ?" }
        var stmt, err = c.db.Prepare("select " + strings.Join(c.cols, ", ") + " from " + c.table +
            " where " + strings.Join(where, " and "))
        if err != nil { return err }
        c.stmt = stmt
    }
    var got = make([]any, len(c.cols))
    var into = make([]any, len(c.cols))
    for i := range got { into[i] = &got[i] }
    var err = c.stmt.QueryRow(args[:c.keys]...).Scan(into...)
    if err == sql.ErrNoRows {
        c.missing++
        c.report("missing row for %s", c.key(args))
        return nil
    }
    if err != nil { return err }
    for i := range c.cols {
        if same(got[i], args[i]) { continue }
        c.differing++
        c.report("%s: %s is %v in the table, %v in the bucket", c.key(args), c.cols[i], got[i], args[i])
        return nil
    }
    return nil
}

func (c *comparer) flush() error {
    if c.stmt != nil { return c.stmt.Close() }
    return nil
}

func (c *comparer) key(args []any) string {
    var parts = make([]string, c.keys)
    for i := 0; i < c.keys; i++ { parts[i] = fmt.Sprintf("%v=%v", c.cols[i], args[i]) }
    return strings.Join(parts, " ")
}

func (c *comparer) report(format string, args ...any) {
    if c.reported >= reportLimit { return }
    c.reported++
    logging.Err("%s: %s", c.table, fmt.Sprintf(format, args...))
    if c.reported == reportLimit { logging.Err("%s: further differences are counted, not listed", c.table) }
}

// same compares a value read back out of SQLite with the one the mapping emitted.
// They are never the same Go type: a driver hands back int64, float64, string or
// []byte, where a mapping emits whatever the record held — an int32 size, a bool
// flag, a string. So both sides are reduced to one of three forms first.
func same(stored, emitted any) bool {
    switch e := emitted.(type) {
    case bool:
        var n, ok = asInt(stored)
        return ok && (n != 0) == e
    case string:
        return asString(stored) == e
    case []byte:
        var s, ok = stored.([]byte)
        return ok && bytes.Equal(s, e)
    case float64:
        var f, ok = stored.(float64)
        return ok && f == e
    }
    var want, ok1 = asInt(emitted)
    var got, ok2 = asInt(stored)
    return ok1 && ok2 && want == got
}

func asInt(v any) (int64, bool) {
    switch n := v.(type) {
    case int64:  return n, true
    case int32:  return int64(n), true
    case int:    return int64(n), true
    case uint64: return int64(n), true
    case uint32: return int64(n), true
    case bool:
        if n { return 1, true }
        return 0, true
    }
    return 0, false
}

func asString(v any) string {
    switch s := v.(type) {
    case string: return s
    case []byte: return string(s)
    }
    return fmt.Sprintf("%v", v)
}

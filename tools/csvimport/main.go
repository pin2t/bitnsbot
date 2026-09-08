// Command csvimport loads CSV files into the bot's bbolt database, one bucket
// per file — the reverse of the database UI's export, and the way a table built
// elsewhere (a rich list, an abandoned-coins ranking) is handed to the bot.
//
// Usage:
//
//	csvimport -db bitnsbot.db active.csv abandoned.csv rich.csv
//
// Each file carries a key,value header and one row per entry, and lands in the
// bucket its own name gives — active.csv into active — created when it is not
// there yet. An existing bucket is merged into rather than replaced: a key the
// file names is overwritten, and one it does not is left alone.
package main

import "encoding/csv"
import "flag"
import "fmt"
import "io"
import "os"
import "path/filepath"
import "strings"

import "go.etcd.io/bbolt"
import "bitnsbot/logging"

var dbPath = flag.String("db", "", "path to the bbolt database (required; created when missing)")
var batch = flag.Int("batch", 100000, "rows per write transaction")

func main() {
    flag.Usage = func() {
        fmt.Fprintf(flag.CommandLine.Output(), "Usage: %s -db <database> <file.csv> [file.csv ...]\n", os.Args[0])
        flag.PrintDefaults()
    }
    flag.Parse()
    if *dbPath == "" { logging.Fatal("-db is required") }
    if flag.NArg() == 0 { logging.Fatal("at least one CSV file is required") }
    if *batch < 1 { logging.Fatal("-batch must be at least 1") }
    // every file is checked before anything is written, so a mistyped name is
    // reported instead of leaving half the import done
    for _, path := range flag.Args() {
        if _, err := os.Stat(path); err != nil { logging.Fatal("%v", err) }
    }
    db, err := bbolt.Open(*dbPath, 0600, nil)
    if err != nil { logging.Fatal("open %s: %v", *dbPath, err) }
    defer db.Close()
    for _, path := range flag.Args() {
        var name = bucketName(path)
        var put, over, ierr = importFile(db, path, name)
        if ierr != nil { logging.Fatal("%s: %v", path, ierr) }
        logging.Status("%s: %d rows into %s (%d overwritten)", path, put, name, over)
    }
}

// bucketName is the file's own name without its extension, so active.csv names
// the active bucket and a path says nothing about where the rows go.
func bucketName(path string) string {
    var base = filepath.Base(path)
    return strings.TrimSuffix(base, filepath.Ext(base))
}

// importFile reads one file into one bucket, committing every -batch rows. That
// is not atomic, and deliberately: a chain-sized table does not fit in one
// transaction, and since a row is written by key the whole file can simply be
// imported again after a failure.
func importFile(db *bbolt.DB, path, name string) (int, int, error) {
    var f, err = os.Open(path)
    if err != nil { return 0, 0, err }
    defer f.Close()
    var r = csv.NewReader(f)
    r.FieldsPerRecord = 2
    var head, herr = r.Read()
    if herr == io.EOF { return 0, 0, fmt.Errorf("empty file, expected a key,value header") }
    if herr != nil { return 0, 0, herr }
    if !strings.EqualFold(head[0], "key") || !strings.EqualFold(head[1], "value") {
        return 0, 0, fmt.Errorf("header is %q,%q, expected key,value", head[0], head[1])
    }
    var bucket = []byte(name)
    // up front, so an empty file still leaves the bucket behind rather than
    // depending on there being a batch to write
    if err := db.Update(func(tx *bbolt.Tx) error {
        var _, berr = tx.CreateBucketIfNotExists(bucket)
        return berr
    }); err != nil { return 0, 0, err }
    var rows [][2]string
    var put, over int
    for {
        var rec, rerr = r.Read()
        if rerr != nil && rerr != io.EOF { return put, over, rerr }
        if rerr == nil {
            if rec[0] == "" {
                var line, _ = r.FieldPos(0)
                return put, over, fmt.Errorf("line %d: empty key", line)
            }
            rows = append(rows, [2]string{rec[0], rec[1]})
        }
        if len(rows) == *batch || (rerr == io.EOF && len(rows) > 0) {
            var n, werr = write(db, bucket, rows)
            if werr != nil { return put, over, werr }
            put += len(rows)
            over += n
            rows = rows[:0]
        }
        if rerr == io.EOF { return put, over, nil }
    }
}

// write commits one batch and reports how many of its keys the bucket already
// held — the count is what tells a merge into a populated bucket apart from a
// fresh load, which is the one thing this tool can silently get wrong.
func write(db *bbolt.DB, bucket []byte, rows [][2]string) (int, error) {
    var over int
    var err = db.Update(func(tx *bbolt.Tx) error {
        over = 0
        var b = tx.Bucket(bucket)
        for _, row := range rows {
            var key = []byte(row[0])
            if b.Get(key) != nil { over++ }
            if err := b.Put(key, []byte(row[1])); err != nil { return err }
        }
        return nil
    })
    return over, err
}

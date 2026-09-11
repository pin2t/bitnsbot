// Package dbui is a small web interface for inspecting and editing the bot's
// bbolt database by hand. It exposes list/get/put over the shared handle plus a
// single self-contained HTML page, and is meant to run bound to localhost only —
// it can write any bucket, so it must never face the network.
package dbui

import "bytes"
import _ "embed"
import "encoding/csv"
import "encoding/hex"
import "encoding/json"
import "errors"
import "fmt"
import "io"
import "net/http"
import "strconv"
import "strings"
import "unicode/utf8"
import "go.etcd.io/bbolt"
import "bitnsbot/logging"

//go:embed index.html
var indexHTML []byte

var errNoBucket = errors.New("no such bucket")

// defaultPageSize is how many key/value rows the Data table loads at once when
// the client doesn't ask for a specific size.
const defaultPageSize = 50
const maxPageSize = 500

// Start serves the UI on addr in a background goroutine and returns the server so
// the caller can shut it down before closing the database (a request mid-flight
// against a closed handle would otherwise error). Bind addr to localhost.
func Start(db *bbolt.DB, addr string) *http.Server {
    var srv = &http.Server{Addr: addr, Handler: handler(db)}
    go func() {
        logging.Status("database UI listening on %s", addr)
        if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
            logging.Err("database UI: %v", err)
        }
    }()
    return srv
}

func handler(db *bbolt.DB) http.Handler {
    var mux = http.NewServeMux()
    mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
        if r.URL.Path != "/" {
            http.NotFound(w, r)
            return
        }
        w.Header().Set("Content-Type", "text/html; charset=utf-8")
        w.Write(indexHTML)
    })
    mux.HandleFunc("/api/buckets", func(w http.ResponseWriter, r *http.Request) { buckets(db, w) })
    mux.HandleFunc("/api/view", func(w http.ResponseWriter, r *http.Request) { view(db, w, r) })
    mux.HandleFunc("/api/get", func(w http.ResponseWriter, r *http.Request) { get(db, w, r) })
    mux.HandleFunc("/api/put", func(w http.ResponseWriter, r *http.Request) { put(db, w, r) })
    mux.HandleFunc("/api/delete", func(w http.ResponseWriter, r *http.Request) { del(db, w, r) })
    mux.HandleFunc("/api/createbucket", func(w http.ResponseWriter, r *http.Request) { createBucket(db, w, r) })
    mux.HandleFunc("/api/clearbucket", func(w http.ResponseWriter, r *http.Request) { clearBucket(db, w, r) })
    mux.HandleFunc("/api/export", func(w http.ResponseWriter, r *http.Request) { exportBucket(db, w, r) })
    mux.HandleFunc("/api/import", func(w http.ResponseWriter, r *http.Request) { importBucket(db, w, r) })
    return mux
}

func buckets(db *bbolt.DB, w http.ResponseWriter) {
    var names = []string{}
    db.View(func(tx *bbolt.Tx) error {
        return tx.ForEach(func(name []byte, _ *bbolt.Bucket) error {
            names = append(names, string(name))
            return nil
        })
    })
    writeJSON(w, map[string]any{"buckets": names})
}

type row struct {
    Key   string `json:"key"`
    Value string `json:"value"`
}

// view lists a page of a bucket, or of the keys under a prefix. The prefix goes
// through decodeField like every other key here, so a binary one is given as
// "hex:0000", and the scan is a Seek to it rather than a walk from the start —
// which is what makes a prefix on a large bucket cheap.
func view(db *bbolt.DB, w http.ResponseWriter, r *http.Request) {
    var q = r.URL.Query()
    var page, _ = strconv.Atoi(q.Get("page"))
    if page < 0 { page = 0 }
    var size, _ = strconv.Atoi(q.Get("size"))
    if size <= 0 { size = defaultPageSize }
    if size > maxPageSize { size = maxPageSize }
    var prefix, perr = decodeField(q.Get("prefix"))
    if perr != nil {
        http.Error(w, "bad prefix: "+perr.Error(), http.StatusBadRequest)
        return
    }
    var rows = []row{}
    var hasNext bool
    var err = db.View(func(tx *bbolt.Tx) error {
        var b = tx.Bucket([]byte(q.Get("bucket")))
        if b == nil { return errNoBucket }
        var c = b.Cursor()
        var k, v = c.First()
        // Seek lands on the first key at or above the prefix, so everything
        // matching runs from there until a key stops carrying it.
        if len(prefix) > 0 { k, v = c.Seek(prefix) }
        var matches = func() bool { return k != nil && bytes.HasPrefix(k, prefix) }
        for i := 0; i < page*size && matches(); i++ {
            k, v = c.Next()
        }
        for n := 0; n < size && matches(); n++ {
            rows = append(rows, row{encodeField(k), encodeField(v)})
            k, v = c.Next()
        }
        hasNext = matches()
        return nil
    })
    if err != nil {
        http.Error(w, err.Error(), http.StatusNotFound)
        return
    }
    writeJSON(w, map[string]any{"rows": rows, "page": page, "hasNext": hasNext})
}

func get(db *bbolt.DB, w http.ResponseWriter, r *http.Request) {
    var q = r.URL.Query()
    var key, err = decodeField(q.Get("key"))
    if err != nil {
        http.Error(w, "bad key: "+err.Error(), http.StatusBadRequest)
        return
    }
    var value []byte
    var found bool
    err = db.View(func(tx *bbolt.Tx) error {
        var b = tx.Bucket([]byte(q.Get("bucket")))
        if b == nil { return errNoBucket }
        if v := b.Get(key); v != nil {
            value = append([]byte(nil), v...)
            found = true
        }
        return nil
    })
    if err != nil {
        http.Error(w, err.Error(), http.StatusNotFound)
        return
    }
    if !found {
        http.Error(w, "key not found", http.StatusNotFound)
        return
    }
    writeJSON(w, map[string]any{"value": encodeField(value)})
}

func put(db *bbolt.DB, w http.ResponseWriter, r *http.Request) {
    if r.Method != http.MethodPost {
        http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
        return
    }
    var body struct {
        Bucket string `json:"bucket"`
        Key    string `json:"key"`
        Value  string `json:"value"`
    }
    if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
        http.Error(w, "bad request", http.StatusBadRequest)
        return
    }
    var key, kerr = decodeField(body.Key)
    if kerr != nil {
        http.Error(w, "bad key: "+kerr.Error(), http.StatusBadRequest)
        return
    }
    var value, verr = decodeField(body.Value)
    if verr != nil {
        http.Error(w, "bad value: "+verr.Error(), http.StatusBadRequest)
        return
    }
    var err = db.Update(func(tx *bbolt.Tx) error {
        var b = tx.Bucket([]byte(body.Bucket))
        if b == nil { return errNoBucket }
        return b.Put(key, value)
    })
    if err != nil {
        var code = http.StatusBadRequest
        if err == errNoBucket { code = http.StatusNotFound }
        http.Error(w, err.Error(), code)
        return
    }
    logging.Info("database UI: put %d bytes to %s/%s", len(value), body.Bucket, encodeField(key))
    writeJSON(w, map[string]any{"ok": true})
}

func del(db *bbolt.DB, w http.ResponseWriter, r *http.Request) {
    if r.Method != http.MethodPost {
        http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
        return
    }
    var body struct {
        Bucket string `json:"bucket"`
        Key    string `json:"key"`
    }
    if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
        http.Error(w, "bad request", http.StatusBadRequest)
        return
    }
    var key, kerr = decodeField(body.Key)
    if kerr != nil {
        http.Error(w, "bad key: "+kerr.Error(), http.StatusBadRequest)
        return
    }
    var err = db.Update(func(tx *bbolt.Tx) error {
        var b = tx.Bucket([]byte(body.Bucket))
        if b == nil { return errNoBucket }
        return b.Delete(key)
    })
    if err != nil {
        var code = http.StatusBadRequest
        if err == errNoBucket { code = http.StatusNotFound }
        http.Error(w, err.Error(), code)
        return
    }
    logging.Info("database UI: deleted %s/%s", body.Bucket, encodeField(key))
    writeJSON(w, map[string]any{"ok": true})
}

// createBucket makes an empty top-level bucket. bbolt reports one that is
// already there as an error rather than a no-op, which is what the UI wants to
// say out loud: a Create that silently did nothing would look like it worked.
func createBucket(db *bbolt.DB, w http.ResponseWriter, r *http.Request) {
    if r.Method != http.MethodPost {
        http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
        return
    }
    var body struct {
        Bucket string `json:"bucket"`
    }
    if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
        http.Error(w, "bad request", http.StatusBadRequest)
        return
    }
    if body.Bucket == "" {
        http.Error(w, "bucket is required", http.StatusBadRequest)
        return
    }
    var err = db.Update(func(tx *bbolt.Tx) error {
        var _, berr = tx.CreateBucket([]byte(body.Bucket))
        return berr
    })
    if err != nil {
        http.Error(w, err.Error(), http.StatusBadRequest)
        return
    }
    logging.Info("database UI: created bucket %s", body.Bucket)
    writeJSON(w, map[string]any{"ok": true})
}

func clearBucket(db *bbolt.DB, w http.ResponseWriter, r *http.Request) {
    if r.Method != http.MethodPost {
        http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
        return
    }
    var body struct {
        Bucket string `json:"bucket"`
    }
    if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
        http.Error(w, "bad request", http.StatusBadRequest)
        return
    }
    if body.Bucket == "" {
        http.Error(w, "bucket is required", http.StatusBadRequest)
        return
    }
    var count int
    var err = db.Update(func(tx *bbolt.Tx) error {
        var b = tx.Bucket([]byte(body.Bucket))
        if b == nil { return errNoBucket }
        var c = b.Cursor()
        for k, _ := c.First(); k != nil; k, _ = c.Next() {
            if err := b.Delete(k); err != nil { return err }
            count++
        }
        return nil
    })
    if err != nil {
        var code = http.StatusBadRequest
        if err == errNoBucket { code = http.StatusNotFound }
        http.Error(w, err.Error(), code)
        return
    }
    logging.Info("database UI: cleared bucket %s (%d keys)", body.Bucket, count)
    writeJSON(w, map[string]any{"ok": true, "deleted": count})
}

// importBatch is how many rows one write transaction carries. Streaming is the
// point of this pair, so neither side may hold the file: the reader keeps a
// batch, not a table.
const importBatch = 1000

// exportBucket streams a bucket out as CSV, a row at a time, straight into the
// response — the table is never assembled anywhere, so exporting a bucket of any
// size costs the same memory as exporting one row.
//
// The headers go out only once the bucket is known to exist, because after the
// first byte there is no status line left to change: a failure partway can only
// be logged, and the client is left with a truncated file. The alternative — a
// counting pass first — reads the whole bucket twice to be able to say 500 on a
// failure nothing recovers from anyway.
//
// It holds one read transaction open for the whole download. bbolt readers do
// not block the writer, but they do hold back page reuse, so a very large export
// to a very slow client grows the file for as long as it runs. That is a trade
// this tool can afford: it is a localhost admin page, and the alternative is
// buffering the bucket, which is what this replaces.
func exportBucket(db *bbolt.DB, w http.ResponseWriter, r *http.Request) {
    var bucket = r.URL.Query().Get("bucket")
    if bucket == "" {
        http.Error(w, "bucket is required", http.StatusBadRequest)
        return
    }
    var started bool
    var rows int
    var err = db.View(func(tx *bbolt.Tx) error {
        var b = tx.Bucket([]byte(bucket))
        if b == nil { return errNoBucket }
        w.Header().Set("Content-Type", "text/csv; charset=utf-8")
        w.Header().Set("Content-Disposition", `attachment; filename="`+filename(bucket)+`"`)
        started = true
        var cw = csv.NewWriter(w)
        if err := cw.Write([]string{"key", "value"}); err != nil { return err }
        var ferr = b.ForEach(func(k, v []byte) error {
            rows++
            return cw.Write([]string{encodeField(k), encodeField(v)})
        })
        if ferr != nil { return ferr }
        cw.Flush()
        return cw.Error()
    })
    if err == nil {
        logging.Info("database UI: exported %d keys from %s", rows, bucket)
        return
    }
    if !started {
        var code = http.StatusInternalServerError
        if err == errNoBucket { code = http.StatusNotFound }
        http.Error(w, err.Error(), code)
        return
    }
    logging.Err("database UI: export %s failed after %d rows: %v", bucket, rows, err)
}

// filename makes a bucket name safe to put in a Content-Disposition header.
// Buckets can now be created from this UI under any name, and a quote would end
// the quoted string the header is built from.
func filename(bucket string) string {
    var safe = strings.Map(func(r rune) rune {
        if r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '.' || r == '-' || r == '_' {
            return r
        }
        return '_'
    }, bucket)
    return safe + ".csv"
}

// importBucket reads CSV straight off the request body and writes it as it
// arrives, so an import is bounded by importBatch rather than by the size of the
// file. bucket and strategy ride in the query, which is what lets the body be
// the file itself and nothing else — the browser hands `fetch` the File and
// never reads it into the page.
//
// The write is therefore not one transaction. That is the same trade
// tools/csvimport makes and safe for the same reason: every row is written by
// key, so an import that fails partway is recovered by running it again.
func importBucket(db *bbolt.DB, w http.ResponseWriter, r *http.Request) {
    if r.Method != http.MethodPost {
        http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
        return
    }
    var q = r.URL.Query()
    var bucket = q.Get("bucket")
    var strategy = q.Get("strategy")
    if bucket == "" {
        http.Error(w, "bucket is required", http.StatusBadRequest)
        return
    }
    if strategy != "skip" && strategy != "replace" {
        http.Error(w, "strategy must be 'skip' or 'replace'", http.StatusBadRequest)
        return
    }
    var cr = csv.NewReader(r.Body)
    cr.FieldsPerRecord = 2
    var batch [][2][]byte
    var imported, skipped int
    var flush = func() error {
        if len(batch) == 0 { return nil }
        var put, skip int
        var err = db.Update(func(tx *bbolt.Tx) error {
            put, skip = 0, 0
            var b = tx.Bucket([]byte(bucket))
            if b == nil { return errNoBucket }
            for _, row := range batch {
                if b.Get(row[0]) != nil && strategy == "skip" {
                    skip++
                    continue
                }
                if err := b.Put(row[0], row[1]); err != nil { return err }
                put++
            }
            return nil
        })
        if err != nil { return err }
        imported += put
        skipped += skip
        batch = batch[:0]
        return nil
    }
    for first := true; ; first = false {
        var rec, rerr = cr.Read()
        if rerr == io.EOF { break }
        if rerr != nil {
            http.Error(w, rerr.Error(), http.StatusBadRequest)
            return
        }
        // The export writes a header; a file that has one starts with it, and a
        // file that does not starts with data. Only the first record can be one.
        if first && strings.EqualFold(rec[0], "key") && strings.EqualFold(rec[1], "value") {
            continue
        }
        var key, kerr = decodeField(rec[0])
        if kerr != nil {
            http.Error(w, "bad key: "+kerr.Error(), http.StatusBadRequest)
            return
        }
        if len(key) == 0 {
            var line, _ = cr.FieldPos(0)
            http.Error(w, fmt.Sprintf("line %d: empty key", line), http.StatusBadRequest)
            return
        }
        var value, verr = decodeField(rec[1])
        if verr != nil {
            http.Error(w, "bad value: "+verr.Error(), http.StatusBadRequest)
            return
        }
        batch = append(batch, [2][]byte{key, value})
        if len(batch) == importBatch {
            if err := flush(); err != nil {
                importFailed(w, err)
                return
            }
        }
    }
    if err := flush(); err != nil {
        importFailed(w, err)
        return
    }
    logging.Info("database UI: imported %d keys into %s (%d skipped)", imported, bucket, skipped)
    writeJSON(w, map[string]any{"ok": true, "imported": imported, "skipped": skipped})
}

func importFailed(w http.ResponseWriter, err error) {
    var code = http.StatusBadRequest
    if err == errNoBucket { code = http.StatusNotFound }
    http.Error(w, err.Error(), code)
}

func writeJSON(w http.ResponseWriter, v any) {
    w.Header().Set("Content-Type", "application/json")
    json.NewEncoder(w).Encode(v)
}

// encodeField renders a stored byte slice for display. Most of the bot's data is
// JSON or plain text (miner names, tags) and shows as-is; the binary keys and
// values (big-endian heights, the packed address index) are hex-encoded behind a
// "hex:" marker so they survive a round trip through the text UI. The marker
// can't collide in practice — no bucket stores a text value beginning "hex:".
func encodeField(b []byte) string {
    if !utf8.Valid(b) { return "hex:" + hex.EncodeToString(b) }
    for _, r := range string(b) {
        if r < 0x20 && r != '\n' && r != '\r' && r != '\t' {
            return "hex:" + hex.EncodeToString(b)
        }
    }
    return string(b)
}

// decodeField is encodeField's inverse: a "hex:"-prefixed string is decoded from
// hex, anything else is taken as its literal UTF-8 bytes.
func decodeField(s string) ([]byte, error) {
    if strings.HasPrefix(s, "hex:") {
        return hex.DecodeString(s[len("hex:"):])
    }
    return []byte(s), nil
}
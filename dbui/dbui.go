// Package dbui is a small web interface for inspecting and editing the bot's
// bbolt database by hand. It exposes list/get/put over the shared handle plus a
// single self-contained HTML page, and is meant to run bound to localhost only —
// it can write any bucket, so it must never face the network.
package dbui

import (
    _ "embed"
    "encoding/hex"
    "encoding/json"
    "errors"
    "net/http"
    "strconv"
    "strings"
    "unicode/utf8"

    "go.etcd.io/bbolt"
    "github.com/vmihailenco/msgpack/v5"
    "bitnsbot/logging"
)

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
    mux.HandleFunc("/api/clearbucket", func(w http.ResponseWriter, r *http.Request) { clearBucket(db, w, r) })
    mux.HandleFunc("/api/export", func(w http.ResponseWriter, r *http.Request) { exportBucket(db, w, r) })
    mux.HandleFunc("/api/import", func(w http.ResponseWriter, r *http.Request) { importBucket(db, w, r) })
    mux.HandleFunc("/api/msgpack/decode", func(w http.ResponseWriter, r *http.Request) { msgpackDecode(w, r) })
    mux.HandleFunc("/api/msgpack/encode", func(w http.ResponseWriter, r *http.Request) { msgpackEncode(w, r) })
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

type kvRow struct {
    Key   string `json:"key"`
    Value string `json:"value"`
}

func view(db *bbolt.DB, w http.ResponseWriter, r *http.Request) {
    var q = r.URL.Query()
    var page, _ = strconv.Atoi(q.Get("page"))
    if page < 0 { page = 0 }
    var size, _ = strconv.Atoi(q.Get("size"))
    if size <= 0 { size = defaultPageSize }
    if size > maxPageSize { size = maxPageSize }
    var rows = []kvRow{}
    var hasNext bool
    var err = db.View(func(tx *bbolt.Tx) error {
        var b = tx.Bucket([]byte(q.Get("bucket")))
        if b == nil { return errNoBucket }
        var c = b.Cursor()
        var k, v = c.First()
        for i := 0; i < page*size && k != nil; i++ {
            k, v = c.Next()
        }
        for n := 0; n < size && k != nil; n++ {
            rows = append(rows, kvRow{encodeField(k), encodeField(v)})
            k, v = c.Next()
        }
        hasNext = k != nil
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

func exportBucket(db *bbolt.DB, w http.ResponseWriter, r *http.Request) {
    var bucket = r.URL.Query().Get("bucket")
    if bucket == "" {
        http.Error(w, "bucket is required", http.StatusBadRequest)
        return
    }
    var rows = []kvRow{}
    var err = db.View(func(tx *bbolt.Tx) error {
        var b = tx.Bucket([]byte(bucket))
        if b == nil { return errNoBucket }
        return b.ForEach(func(k, v []byte) error {
            rows = append(rows, kvRow{encodeField(k), encodeField(v)})
            return nil
        })
    })
    if err != nil {
        if err == errNoBucket { http.Error(w, err.Error(), http.StatusNotFound); return }
        http.Error(w, err.Error(), http.StatusInternalServerError)
        return
    }
    writeJSON(w, map[string]any{"rows": rows})
}

func importBucket(db *bbolt.DB, w http.ResponseWriter, r *http.Request) {
    if r.Method != http.MethodPost {
        http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
        return
    }
    var body struct {
        Bucket   string  `json:"bucket"`
        Strategy string  `json:"strategy"` // "skip" or "replace"
        Rows     []kvRow `json:"rows"`
    }
    if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
        http.Error(w, "bad request", http.StatusBadRequest)
        return
    }
    if body.Bucket == "" {
        http.Error(w, "bucket is required", http.StatusBadRequest)
        return
    }
    if body.Strategy != "skip" && body.Strategy != "replace" {
        http.Error(w, "strategy must be 'skip' or 'replace'", http.StatusBadRequest)
        return
    }
    var imported, skipped int
    var err = db.Update(func(tx *bbolt.Tx) error {
        var b = tx.Bucket([]byte(body.Bucket))
        if b == nil { return errNoBucket }
        for _, row := range body.Rows {
            var key, kerr = decodeField(row.Key)
            if kerr != nil { return kerr }
            var value, verr = decodeField(row.Value)
            if verr != nil { return verr }
            var exists = b.Get(key) != nil
            if exists && body.Strategy == "skip" {
                skipped++
                continue
            }
            if err := b.Put(key, value); err != nil { return err }
            imported++
        }
        return nil
    })
    if err != nil {
        var code = http.StatusBadRequest
        if err == errNoBucket { code = http.StatusNotFound }
        http.Error(w, err.Error(), code)
        return
    }
    logging.Info("database UI: imported %d keys into %s (%d skipped)", imported, body.Bucket, skipped)
    writeJSON(w, map[string]any{"ok": true, "imported": imported, "skipped": skipped})
}

func msgpackDecode(w http.ResponseWriter, r *http.Request) {
    if r.Method != http.MethodPost {
        http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
        return
    }
    var body struct {
        Value string `json:"value"`
    }
    if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
        http.Error(w, "bad request", http.StatusBadRequest)
        return
    }
    raw, err := decodeField(body.Value)
    if err != nil {
        http.Error(w, "bad value: "+err.Error(), http.StatusBadRequest)
        return
    }
    var decoded any
    if err := msgpack.Unmarshal(raw, &decoded); err != nil {
        http.Error(w, "msgpack decode error: "+err.Error(), http.StatusBadRequest)
        return
    }
    out, err := json.MarshalIndent(decoded, "", "  ")
    if err != nil {
        http.Error(w, "json marshal error: "+err.Error(), http.StatusInternalServerError)
        return
    }
    writeJSON(w, map[string]any{"decoded": string(out)})
}

func msgpackEncode(w http.ResponseWriter, r *http.Request) {
    if r.Method != http.MethodPost {
        http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
        return
    }
    var body struct {
        Text string `json:"text"`
    }
    if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
        http.Error(w, "bad request", http.StatusBadRequest)
        return
    }
    var val any
    if err := json.Unmarshal([]byte(body.Text), &val); err != nil {
        http.Error(w, "json parse error: "+err.Error(), http.StatusBadRequest)
        return
    }
    encoded, err := msgpack.Marshal(val)
    if err != nil {
        http.Error(w, "msgpack encode error: "+err.Error(), http.StatusInternalServerError)
        return
    }
    writeJSON(w, map[string]any{"encoded": "hex:" + hex.EncodeToString(encoded)})
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
    if isText(b) { return string(b) }
    return "hex:" + hex.EncodeToString(b)
}

// decodeField is encodeField's inverse: a "hex:"-prefixed string is decoded from
// hex, anything else is taken as its literal UTF-8 bytes.
func decodeField(s string) ([]byte, error) {
    if strings.HasPrefix(s, "hex:") {
        return hex.DecodeString(s[len("hex:"):])
    }
    return []byte(s), nil
}

// isText reports whether b is safe to show directly: valid UTF-8 with no control
// characters other than the whitespace that appears in formatted JSON.
func isText(b []byte) bool {
    if !utf8.Valid(b) { return false }
    for _, r := range string(b) {
        if r < 0x20 && r != '\n' && r != '\r' && r != '\t' { return false }
    }
    return true
}

// MigrateMsgpack converts JSON values to msgpack in the five buckets that the bot
// reads and writes as structured data. It is called once on startup before any
// goroutine reads or writes them. Each value that begins with '{' (a JSON object)
// and passes json.Unmarshal is re-encoded with msgpack. Values already in msgpack
// format (binary, not starting with '{') are left alone, so a second run is a
// no-op.
func MigrateMsgpack(db *bbolt.DB) error {
    var buckets = []string{"blocks-stat", "market", "miners-stat", "rates", "watches"}
    var total int
    for _, name := range buckets {
        var count int
        var err = db.Update(func(tx *bbolt.Tx) error {
            var b = tx.Bucket([]byte(name))
            if b == nil { return nil } // bucket doesn't exist yet — nothing to migrate
            var c = b.Cursor()
            for k, v := c.First(); k != nil; k, v = c.Next() {
                if len(v) == 0 || v[0] != '{' { continue }
                var val any
                if json.Unmarshal(v, &val) != nil { continue }
                var encoded, merr = msgpack.Marshal(val)
                if merr != nil { return merr }
                if err := b.Put(k, encoded); err != nil { return err }
                count++
            }
            return nil
        })
        if err != nil { return err }
        if count > 0 {
            logging.Info("database UI: migrated %s: %d keys JSON → msgpack", name, count)
            total += count
        }
    }
    if total > 0 {
        logging.Info("database UI: migration complete: %d keys total converted", total)
    }
    return nil
}

package dbui

import "bytes"
import "encoding/json"
import "fmt"
import "io"
import "net/http"
import "net/http/httptest"
import "net/url"
import "path/filepath"
import "strings"
import "testing"

import "go.etcd.io/bbolt"

func testDB(t *testing.T) *bbolt.DB {
    var db, err = bbolt.Open(filepath.Join(t.TempDir(), "t.db"), 0600, nil)
    if err != nil { t.Fatalf("open: %v", err) }
    t.Cleanup(func() { db.Close() })
    if err := db.Update(func(tx *bbolt.Tx) error {
        var b, _ = tx.CreateBucket([]byte("miners"))
        b.Put([]byte("addrA"), []byte("PoolA"))
        b.Put([]byte("addrB"), []byte("PoolB"))
        var raw, _ = tx.CreateBucket([]byte("addrindex"))
        raw.Put([]byte{0x00, 0x01, 0x00, 0x00, 0x00, 0x00}, []byte{0xde, 0xad, 0xbe, 0xef})
        return nil
    }); err != nil {
        t.Fatalf("seed: %v", err)
    }
    return db
}

func TestBuckets(t *testing.T) {
    var srv = httptest.NewServer(handler(testDB(t)))
    defer srv.Close()
    var resp, err = http.Get(srv.URL + "/api/buckets")
    if err != nil { t.Fatal(err) }
    defer resp.Body.Close()
    var out struct{ Buckets []string }
    json.NewDecoder(resp.Body).Decode(&out)
    if len(out.Buckets) != 2 || out.Buckets[0] != "addrindex" || out.Buckets[1] != "miners" {
        t.Fatalf("buckets = %v, want [addrindex miners] in sorted order", out.Buckets)
    }
}

func TestView(t *testing.T) {
    var srv = httptest.NewServer(handler(testDB(t)))
    defer srv.Close()
    var resp, _ = http.Get(srv.URL + "/api/view?bucket=miners")
    defer resp.Body.Close()
    var out struct {
        Rows    []kvRow
        HasNext bool
    }
    json.NewDecoder(resp.Body).Decode(&out)
    if len(out.Rows) != 2 {
        t.Fatalf("rows = %d, want 2", len(out.Rows))
    }
    if out.Rows[0].Key != "addrA" || out.Rows[0].Value != "PoolA" {
        t.Fatalf("first row = %+v", out.Rows[0])
    }
    if out.HasNext {
        t.Fatal("a two-key bucket in one page must not report a next page")
    }
    // an unknown bucket is a 404, not a 200 with empty rows
    var missing, _ = http.Get(srv.URL + "/api/view?bucket=nope")
    if missing.StatusCode != http.StatusNotFound {
        t.Fatalf("missing bucket returned %d, want 404", missing.StatusCode)
    }
}

// A bigger bucket must page: each page returns `size` rows and flags whether more
// follow, and the second page continues where the first stopped.
func TestViewPaginates(t *testing.T) {
    var db = testDB(t)
    db.Update(func(tx *bbolt.Tx) error {
        var b, _ = tx.CreateBucket([]byte("big"))
        for i := 0; i < 5; i++ {
            b.Put([]byte{byte('a' + i)}, []byte{byte('0' + i)})
        }
        return nil
    })
    var srv = httptest.NewServer(handler(db))
    defer srv.Close()
    var p0 = viewPage(t, srv.URL, "big", 0, 2)
    if len(p0.Rows) != 2 || !p0.HasNext || p0.Rows[0].Key != "a" {
        t.Fatalf("page 0 = %+v", p0)
    }
    var p2 = viewPage(t, srv.URL, "big", 2, 2)
    if len(p2.Rows) != 1 || p2.HasNext || p2.Rows[0].Key != "e" {
        t.Fatalf("page 2 (last) = %+v", p2)
    }
}

type viewOut struct {
    Rows    []kvRow
    HasNext bool
    Page    int
}

func viewPage(t *testing.T, base, bucket string, page, size int) viewOut {
    var resp, err = http.Get(base + "/api/view?bucket=" + bucket + "&page=" +
        itoa(page) + "&size=" + itoa(size))
    if err != nil { t.Fatal(err) }
    defer resp.Body.Close()
    var out viewOut
    json.NewDecoder(resp.Body).Decode(&out)
    return out
}

func itoa(n int) string { return string(rune('0' + n)) }

func TestGetAndPut(t *testing.T) {
    var db = testDB(t)
    var srv = httptest.NewServer(handler(db))
    defer srv.Close()
    var resp, _ = http.Get(srv.URL + "/api/get?bucket=miners&key=addrA")
    defer resp.Body.Close()
    var got struct{ Value string }
    json.NewDecoder(resp.Body).Decode(&got)
    if got.Value != "PoolA" {
        t.Fatalf("get = %q, want PoolA", got.Value)
    }
    // edit it back
    var body = `{"bucket":"miners","key":"addrA","value":"Renamed"}`
    var put, _ = http.Post(srv.URL+"/api/put", "application/json", strings.NewReader(body))
    if put.StatusCode != http.StatusOK {
        t.Fatalf("put returned %d", put.StatusCode)
    }
    var stored string
    db.View(func(tx *bbolt.Tx) error {
        stored = string(tx.Bucket([]byte("miners")).Get([]byte("addrA")))
        return nil
    })
    if stored != "Renamed" {
        t.Fatalf("stored value = %q, want Renamed", stored)
    }
    // a key that doesn't exist is a 404
    var miss, _ = http.Get(srv.URL + "/api/get?bucket=miners&key=nope")
    if miss.StatusCode != http.StatusNotFound {
        t.Fatalf("missing key returned %d, want 404", miss.StatusCode)
    }
}

// Binary keys and values survive a round trip through the text UI via the "hex:"
// convention — without it a Put would corrupt the packed address index.
func TestBinaryRoundTrip(t *testing.T) {
    var db = testDB(t)
    var srv = httptest.NewServer(handler(db))
    defer srv.Close()
    var resp, _ = http.Get(srv.URL + "/api/view?bucket=addrindex")
    defer resp.Body.Close()
    var out struct{ Rows []kvRow }
    json.NewDecoder(resp.Body).Decode(&out)
    if len(out.Rows) != 1 || out.Rows[0].Key != "hex:000100000000" || out.Rows[0].Value != "hex:deadbeef" {
        t.Fatalf("binary row = %+v", out.Rows)
    }
    // put a new binary value back using the same encoding the view returned
    var body = `{"bucket":"addrindex","key":"hex:000100000000","value":"hex:cafe"}`
    http.Post(srv.URL+"/api/put", "application/json", strings.NewReader(body))
    var stored []byte
    db.View(func(tx *bbolt.Tx) error {
        stored = append(stored, tx.Bucket([]byte("addrindex")).Get([]byte{0, 1, 0, 0, 0, 0})...)
        return nil
    })
    if len(stored) != 2 || stored[0] != 0xca || stored[1] != 0xfe {
        t.Fatalf("stored binary = %x, want cafe", stored)
    }
}

func TestEncodeField(t *testing.T) {
    var cases = []struct {
        in   []byte
        want string
    }{
        {[]byte("PoolA"), "PoolA"},
        {[]byte(`{"a":1}`), `{"a":1}`},
        {[]byte{0x00, 0x01}, "hex:0001"},          // control bytes
        {[]byte{0xde, 0xad, 0xbe, 0xef}, "hex:deadbeef"}, // invalid UTF-8
        {[]byte{0xff, 0xfe}, "hex:fffe"},          // invalid UTF-8
        {[]byte("newlines\nand\ttabs are text"), "newlines\nand\ttabs are text"},
    }
    for _, c := range cases {
        if got := encodeField(c.in); got != c.want {
            t.Errorf("encodeField(%x) = %q, want %q", c.in, got, c.want)
        }
        var back, err = decodeField(encodeField(c.in))
        if err != nil || string(back) != string(c.in) {
            t.Errorf("round trip of %x failed: %x, %v", c.in, back, err)
        }
    }
}

func TestPutRejectsWrongMethod(t *testing.T) {
    var srv = httptest.NewServer(handler(testDB(t)))
    defer srv.Close()
    var resp, _ = http.Get(srv.URL + "/api/put")
    if resp.StatusCode != http.StatusMethodNotAllowed {
        t.Fatalf("GET /api/put returned %d, want 405", resp.StatusCode)
    }
}

func TestDelete(t *testing.T) {
    var db = testDB(t)
    var srv = httptest.NewServer(handler(db))
    defer srv.Close()
    var body = `{"bucket":"miners","key":"addrA"}`
    var resp, _ = http.Post(srv.URL+"/api/delete", "application/json", strings.NewReader(body))
    if resp.StatusCode != http.StatusOK {
        t.Fatalf("delete returned %d", resp.StatusCode)
    }
    var gone, kept bool
    db.View(func(tx *bbolt.Tx) error {
        var b = tx.Bucket([]byte("miners"))
        gone = b.Get([]byte("addrA")) == nil
        kept = b.Get([]byte("addrB")) != nil
        return nil
    })
    if !gone {
        t.Fatal("addrA is still in the bucket after delete")
    }
    if !kept {
        t.Fatal("delete removed a key it was not asked to")
    }
    // bbolt treats deleting a missing key as success, and so does the UI — the
    // row is gone either way, which is all the caller wanted
    var again, _ = http.Post(srv.URL+"/api/delete", "application/json", strings.NewReader(body))
    if again.StatusCode != http.StatusOK {
        t.Fatalf("deleting an absent key returned %d, want 200", again.StatusCode)
    }
    var missing = `{"bucket":"nope","key":"addrA"}`
    var badBucket, _ = http.Post(srv.URL+"/api/delete", "application/json", strings.NewReader(missing))
    if badBucket.StatusCode != http.StatusNotFound {
        t.Fatalf("delete on an unknown bucket returned %d, want 404", badBucket.StatusCode)
    }
}

// A binary key must be deletable by the same "hex:" form the table displays,
// otherwise the trash icon would silently fail on the packed address index.
func TestDeleteBinaryKey(t *testing.T) {
    var db = testDB(t)
    var srv = httptest.NewServer(handler(db))
    defer srv.Close()
    var body = `{"bucket":"addrindex","key":"hex:000100000000"}`
    var resp, _ = http.Post(srv.URL+"/api/delete", "application/json", strings.NewReader(body))
    if resp.StatusCode != http.StatusOK {
        t.Fatalf("delete returned %d", resp.StatusCode)
    }
    var gone bool
    db.View(func(tx *bbolt.Tx) error {
        gone = tx.Bucket([]byte("addrindex")).Get([]byte{0, 1, 0, 0, 0, 0}) == nil
        return nil
    })
    if !gone {
        t.Fatal("the binary key survived a hex-encoded delete")
    }
}

func TestDeleteRejectsWrongMethod(t *testing.T) {
    var srv = httptest.NewServer(handler(testDB(t)))
    defer srv.Close()
    var resp, _ = http.Get(srv.URL + "/api/delete")
    if resp.StatusCode != http.StatusMethodNotAllowed {
        t.Fatalf("GET /api/delete returned %d, want 405", resp.StatusCode)
    }
}

func TestClearBucket(t *testing.T) {
    var db = testDB(t)
    var srv = httptest.NewServer(handler(db))
    defer srv.Close()
    var body = `{"bucket":"miners"}`
    var resp, _ = http.Post(srv.URL+"/api/clearbucket", "application/json", strings.NewReader(body))
    if resp.StatusCode != http.StatusOK {
        t.Fatalf("clear bucket returned %d", resp.StatusCode)
    }
    var out struct {
        OK      bool
        Deleted int
    }
    json.NewDecoder(resp.Body).Decode(&out)
    if !out.OK || out.Deleted != 2 {
        t.Fatalf("clear bucket response = %+v, want ok with 2 deleted", out)
    }
    // verify the bucket still exists but is empty
    var exists, empty bool
    db.View(func(tx *bbolt.Tx) error {
        var b = tx.Bucket([]byte("miners"))
        exists = b != nil
        if exists {
            var c = b.Cursor()
            _, v := c.First()
            empty = v == nil
        }
        return nil
    })
    if !exists {
        t.Fatal("bucket should still exist after clear")
    }
    if !empty {
        t.Fatal("bucket should be empty after clear")
    }
    // unknown bucket is 404
    var missing = `{"bucket":"nope"}`
    var bad, _ = http.Post(srv.URL+"/api/clearbucket", "application/json", strings.NewReader(missing))
    if bad.StatusCode != http.StatusNotFound {
        t.Fatalf("clear on unknown bucket returned %d, want 404", bad.StatusCode)
    }
    // GET is rejected
    var getResp, _ = http.Get(srv.URL + "/api/clearbucket")
    if getResp.StatusCode != http.StatusMethodNotAllowed {
        t.Fatalf("GET /api/clearbucket returned %d, want 405", getResp.StatusCode)
    }
}

// Create makes an empty bucket, and says so when there is already one of that
// name — bbolt treats that as an error, and a Create that silently did nothing
// would look like it had worked.
func TestCreateBucket(t *testing.T) {
    var db = testDB(t)
    var srv = httptest.NewServer(handler(db))
    defer srv.Close()
    var post = func(body string) int {
        var resp, err = http.Post(srv.URL+"/api/createbucket", "application/json", strings.NewReader(body))
        if err != nil { t.Fatal(err) }
        defer resp.Body.Close()
        return resp.StatusCode
    }
    if code := post(`{"bucket":"newthing"}`); code != 200 {
        t.Fatalf("create = %d, want 200", code)
    }
    var found bool
    db.View(func(tx *bbolt.Tx) error {
        found = tx.Bucket([]byte("newthing")) != nil
        return nil
    })
    if !found { t.Fatal("the bucket was not created") }
    // and it is in the list the dropdown is built from
    var resp, err = http.Get(srv.URL + "/api/buckets")
    if err != nil { t.Fatal(err) }
    defer resp.Body.Close()
    var out struct{ Buckets []string }
    json.NewDecoder(resp.Body).Decode(&out)
    if strings.Join(out.Buckets, ",") != "addrindex,miners,newthing" {
        t.Errorf("buckets = %v, want the new one listed", out.Buckets)
    }
    if code := post(`{"bucket":"newthing"}`); code != 400 {
        t.Errorf("creating it twice = %d, want 400", code)
    }
    if code := post(`{"bucket":""}`); code != 400 {
        t.Errorf("empty name = %d, want 400", code)
    }
    if code := post(`not json`); code != 400 {
        t.Errorf("malformed body = %d, want 400", code)
    }
    var g, gerr = http.Get(srv.URL + "/api/createbucket")
    if gerr != nil { t.Fatal(gerr) }
    defer g.Body.Close()
    if g.StatusCode != 405 {
        t.Errorf("GET = %d, want 405 — creating a bucket is not a read", g.StatusCode)
    }
}

// A prefix narrows the listing to the keys under it, and pages within them:
// Seek goes straight to the first match, so the rest of the bucket is not walked.
func TestViewPrefix(t *testing.T) {
    var db = testDB(t)
    if err := db.Update(func(tx *bbolt.Tx) error {
        var b = tx.Bucket([]byte("miners"))
        for _, k := range []string{"bc1a", "bc1b", "bc1c", "zz"} {
            if err := b.Put([]byte(k), []byte("Pool")); err != nil { return err }
        }
        return nil
    }); err != nil { t.Fatal(err) }
    var srv = httptest.NewServer(handler(db))
    defer srv.Close()
    var list = func(query string) (keys []string, hasNext bool) {
        var resp, err = http.Get(srv.URL + "/api/view?bucket=miners&" + query)
        if err != nil { t.Fatal(err) }
        defer resp.Body.Close()
        if resp.StatusCode != 200 { t.Fatalf("%s = %d", query, resp.StatusCode) }
        var out struct {
            Rows    []kvRow
            HasNext bool `json:"hasNext"`
        }
        json.NewDecoder(resp.Body).Decode(&out)
        for _, r := range out.Rows { keys = append(keys, r.Key) }
        return keys, out.HasNext
    }
    var got, _ = list("prefix=bc1")
    if strings.Join(got, ",") != "bc1a,bc1b,bc1c" {
        t.Errorf("prefix bc1 gave %v", got)
    }
    // an empty prefix is the whole bucket, exactly as before
    got, _ = list("")
    if len(got) != 6 { t.Errorf("no prefix gave %d rows, want all 6", len(got)) }
    // paging stays inside the prefix, and hasNext must not point outside it
    got, hasNext := list("prefix=bc1&size=2")
    if strings.Join(got, ",") != "bc1a,bc1b" || !hasNext {
        t.Errorf("first page = %v hasNext=%v", got, hasNext)
    }
    got, hasNext = list("prefix=bc1&size=2&page=1")
    if strings.Join(got, ",") != "bc1c" || hasNext {
        t.Errorf("second page = %v hasNext=%v; zz is outside the prefix", got, hasNext)
    }
    // one that matches nothing is an empty listing, not the whole bucket
    got, _ = list("prefix=nothing")
    if len(got) != 0 { t.Errorf("unmatched prefix gave %v", got) }
}

// Keys are binary in places, so a prefix goes through the same hex: marker the
// rest of the UI uses — without it the packed buckets could not be scanned.
func TestViewPrefixHex(t *testing.T) {
    var srv = httptest.NewServer(handler(testDB(t)))
    defer srv.Close()
    var resp, err = http.Get(srv.URL + "/api/view?bucket=addrindex&prefix=hex:0001")
    if err != nil { t.Fatal(err) }
    defer resp.Body.Close()
    var out struct{ Rows []kvRow }
    json.NewDecoder(resp.Body).Decode(&out)
    if len(out.Rows) != 1 || out.Rows[0].Key != "hex:000100000000" {
        t.Fatalf("rows = %+v, want the one packed key", out.Rows)
    }
    var miss, merr = http.Get(srv.URL + "/api/view?bucket=addrindex&prefix=hex:ffff")
    if merr != nil { t.Fatal(merr) }
    defer miss.Body.Close()
    json.NewDecoder(miss.Body).Decode(&out)
    if len(out.Rows) != 0 { t.Errorf("a prefix past the end gave %+v", out.Rows) }
    // a prefix that is not hex at all is refused rather than taken literally
    var bad, berr = http.Get(srv.URL + "/api/view?bucket=addrindex&prefix=hex:zz")
    if berr != nil { t.Fatal(berr) }
    defer bad.Body.Close()
    if bad.StatusCode != 400 { t.Errorf("bad hex prefix = %d, want 400", bad.StatusCode) }
}

func httpGet(t *testing.T, url string) *http.Response {
    t.Helper()
    var resp, err = http.Get(url)
    if err != nil { t.Fatal(err) }
    return resp
}

// Export writes CSV straight into the response: a header row, then one row per
// key, with binary fields behind the hex: marker the rest of the UI uses.
func TestExportStreamsCSV(t *testing.T) {
    var srv = httptest.NewServer(handler(testDB(t)))
    defer srv.Close()
    var resp = httpGet(t, srv.URL+"/api/export?bucket=miners")
    defer resp.Body.Close()
    if resp.StatusCode != 200 { t.Fatalf("export = %d", resp.StatusCode) }
    if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/csv") {
        t.Errorf("Content-Type = %q, want text/csv", ct)
    }
    if cd := resp.Header.Get("Content-Disposition"); cd != `attachment; filename="miners.csv"` {
        t.Errorf("Content-Disposition = %q", cd)
    }
    var body, _ = io.ReadAll(resp.Body)
    var want = "key,value\naddrA,PoolA\naddrB,PoolB\n"
    if string(body) != want {
        t.Errorf("body =\n%q\nwant\n%q", body, want)
    }
    // a binary bucket comes out hex-encoded, which is what makes it importable
    var bin = httpGet(t, srv.URL+"/api/export?bucket=addrindex")
    defer bin.Body.Close()
    body, _ = io.ReadAll(bin.Body)
    if string(body) != "key,value\nhex:000100000000,hex:deadbeef\n" {
        t.Errorf("binary export = %q", body)
    }
}

// The status can only be set before the first byte, so a bucket that is not
// there must be caught before anything is written.
func TestExportMissingBucket(t *testing.T) {
    var srv = httptest.NewServer(handler(testDB(t)))
    defer srv.Close()
    var resp = httpGet(t, srv.URL+"/api/export?bucket=nosuch")
    defer resp.Body.Close()
    if resp.StatusCode != 404 { t.Errorf("missing bucket = %d, want 404", resp.StatusCode) }
    if cd := resp.Header.Get("Content-Disposition"); cd != "" {
        t.Errorf("a failed export set %q; the headers must wait for the bucket", cd)
    }
    var none = httpGet(t, srv.URL+"/api/export")
    defer none.Body.Close()
    if none.StatusCode != 400 { t.Errorf("no bucket = %d, want 400", none.StatusCode) }
}

// Buckets can be created from this UI under any name, and the name goes into a
// quoted header value.
func TestExportFilenameIsSafe(t *testing.T) {
    var db = testDB(t)
    var nasty = `we"ird name`
    if err := db.Update(func(tx *bbolt.Tx) error {
        var _, err = tx.CreateBucket([]byte(nasty))
        return err
    }); err != nil { t.Fatal(err) }
    var srv = httptest.NewServer(handler(db))
    defer srv.Close()
    var resp = httpGet(t, srv.URL+"/api/export?bucket="+url.QueryEscape(nasty))
    defer resp.Body.Close()
    if cd := resp.Header.Get("Content-Disposition"); cd != `attachment; filename="we_ird_name.csv"` {
        t.Errorf("Content-Disposition = %q; the quote must not escape the header", cd)
    }
}

func postCSV(t *testing.T, url, body string) *http.Response {
    t.Helper()
    var resp, err = http.Post(url, "text/csv", strings.NewReader(body))
    if err != nil { t.Fatal(err) }
    return resp
}

// Import reads the CSV off the request body, with the bucket and the strategy in
// the query — so the body is the file and nothing else.
func TestImportStreamsCSV(t *testing.T) {
    var db = testDB(t)
    var srv = httptest.NewServer(handler(db))
    defer srv.Close()
    var resp = postCSV(t, srv.URL+"/api/import?bucket=miners&strategy=skip",
        "key,value\naddrA,Changed\naddrC,PoolC\n")
    defer resp.Body.Close()
    if resp.StatusCode != 200 { t.Fatalf("import = %d", resp.StatusCode) }
    var out struct{ Imported, Skipped int }
    json.NewDecoder(resp.Body).Decode(&out)
    if out.Imported != 1 || out.Skipped != 1 {
        t.Errorf("imported %d skipped %d, want 1 and 1", out.Imported, out.Skipped)
    }
    db.View(func(tx *bbolt.Tx) error {
        var b = tx.Bucket([]byte("miners"))
        if string(b.Get([]byte("addrA"))) != "PoolA" { t.Error("skip overwrote an existing key") }
        if string(b.Get([]byte("addrC"))) != "PoolC" { t.Error("the new key was not written") }
        return nil
    })
    // replace does overwrite
    var rep = postCSV(t, srv.URL+"/api/import?bucket=miners&strategy=replace", "key,value\naddrA,Changed\n")
    defer rep.Body.Close()
    json.NewDecoder(rep.Body).Decode(&out)
    if out.Imported != 1 || out.Skipped != 0 { t.Errorf("replace imported %d skipped %d", out.Imported, out.Skipped) }
    db.View(func(tx *bbolt.Tx) error {
        if string(tx.Bucket([]byte("miners")).Get([]byte("addrA"))) != "Changed" {
            t.Error("replace did not overwrite")
        }
        return nil
    })
    // a file with no header row is data from its first line
    var noHead = postCSV(t, srv.URL+"/api/import?bucket=miners&strategy=replace", "addrD,PoolD\n")
    defer noHead.Body.Close()
    json.NewDecoder(noHead.Body).Decode(&out)
    if out.Imported != 1 { t.Errorf("headerless import took %d rows", out.Imported) }
}

// The batching loop is the whole point of streaming, so a file longer than one
// batch has to land in full.
func TestImportBatches(t *testing.T) {
    var db = testDB(t)
    var srv = httptest.NewServer(handler(db))
    defer srv.Close()
    var body strings.Builder
    body.WriteString("key,value\n")
    var n = importBatch*2 + 137
    for i := 0; i < n; i++ {
        fmt.Fprintf(&body, "k%06d,v%06d\n", i, i)
    }
    var resp = postCSV(t, srv.URL+"/api/import?bucket=miners&strategy=replace", body.String())
    defer resp.Body.Close()
    if resp.StatusCode != 200 { t.Fatalf("import = %d", resp.StatusCode) }
    var out struct{ Imported, Skipped int }
    json.NewDecoder(resp.Body).Decode(&out)
    if out.Imported != n { t.Errorf("imported %d of %d rows", out.Imported, n) }
    db.View(func(tx *bbolt.Tx) error {
        var b = tx.Bucket([]byte("miners"))
        for _, i := range []int{0, importBatch - 1, importBatch, n - 1} {
            var k = fmt.Sprintf("k%06d", i)
            if string(b.Get([]byte(k))) != fmt.Sprintf("v%06d", i) {
                t.Errorf("row %d (%s) is missing", i, k)
            }
        }
        return nil
    })
}

func TestImportRejectsBadRequests(t *testing.T) {
    var srv = httptest.NewServer(handler(testDB(t)))
    defer srv.Close()
    var cases = []struct {
        name, query, body string
        want              int
    }{
        {"no bucket", "strategy=skip", "key,value\na,1\n", 400},
        {"no strategy", "bucket=miners", "key,value\na,1\n", 400},
        {"bad strategy", "bucket=miners&strategy=merge", "key,value\na,1\n", 400},
        {"unknown bucket", "bucket=nosuch&strategy=skip", "key,value\na,1\n", 404},
        {"ragged csv", "bucket=miners&strategy=skip", "key,value\na,1,extra\n", 400},
        {"empty key", "bucket=miners&strategy=skip", "key,value\n,1\n", 400},
        {"bad hex key", "bucket=miners&strategy=skip", "key,value\nhex:zz,1\n", 400},
    }
    for _, c := range cases {
        var resp = postCSV(t, srv.URL+"/api/import?"+c.query, c.body)
        if resp.StatusCode != c.want {
            t.Errorf("%s = %d, want %d", c.name, resp.StatusCode, c.want)
        }
        resp.Body.Close()
    }
    var g = httpGet(t, srv.URL+"/api/import?bucket=miners&strategy=skip")
    defer g.Body.Close()
    if g.StatusCode != 405 { t.Errorf("GET = %d, want 405", g.StatusCode) }
}

// The two halves have to agree, binary fields included: what export writes,
// import must read back into an identical bucket.
func TestExportImportRoundTrip(t *testing.T) {
    var db = testDB(t)
    var srv = httptest.NewServer(handler(db))
    defer srv.Close()
    var resp = httpGet(t, srv.URL+"/api/export?bucket=addrindex")
    var csvText, _ = io.ReadAll(resp.Body)
    resp.Body.Close()
    if err := db.Update(func(tx *bbolt.Tx) error {
        var _, err = tx.CreateBucket([]byte("copy"))
        return err
    }); err != nil { t.Fatal(err) }
    var imp = postCSV(t, srv.URL+"/api/import?bucket=copy&strategy=replace", string(csvText))
    defer imp.Body.Close()
    if imp.StatusCode != 200 { t.Fatalf("import = %d", imp.StatusCode) }
    db.View(func(tx *bbolt.Tx) error {
        var src, dst = tx.Bucket([]byte("addrindex")), tx.Bucket([]byte("copy"))
        if src.Stats().KeyN != dst.Stats().KeyN {
            t.Fatalf("copy has %d keys, source has %d", dst.Stats().KeyN, src.Stats().KeyN)
        }
        return src.ForEach(func(k, v []byte) error {
            if !bytes.Equal(dst.Get(k), v) {
                t.Errorf("key %x came back as %x, want %x", k, dst.Get(k), v)
            }
            return nil
        })
    })
}

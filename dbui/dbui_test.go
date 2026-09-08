package dbui

import "encoding/json"
import "net/http"
import "net/http/httptest"
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

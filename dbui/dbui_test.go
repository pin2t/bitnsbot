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

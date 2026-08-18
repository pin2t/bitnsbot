package main

import "crypto/hmac"
import "crypto/sha256"
import "encoding/hex"
import "net/http/httptest"
import "net/url"
import "sort"
import "strconv"
import "strings"
import "testing"
import "time"

// signInitData produces the payload Telegram would hand the webview, so the
// tests exercise the real signature rather than a stub.
func signInitData(token string, fields map[string]string) string {
    var keys = make([]string, 0, len(fields))
    for k := range fields { keys = append(keys, k) }
    sort.Strings(keys)
    var pairs = make([]string, 0, len(keys))
    for _, k := range keys { pairs = append(pairs, k+"="+fields[k]) }
    var mac = hmac.New(sha256.New, []byte("WebAppData"))
    mac.Write([]byte(token))
    var secret = mac.Sum(nil)
    mac = hmac.New(sha256.New, secret)
    mac.Write([]byte(strings.Join(pairs, "\n")))
    var v = url.Values{}
    for k, val := range fields { v.Set(k, val) }
    v.Set("hash", hex.EncodeToString(mac.Sum(nil)))
    return v.Encode()
}

func freshInitData(token string) string {
    return signInitData(token, map[string]string{
        "auth_date": strconv.FormatInt(time.Now().Unix(), 10),
        "user":      `{"id":42,"first_name":"Pin"}`,
        "query_id":  "AAF",
    })
}

func TestAppServesPage(t *testing.T) {
    var w = httptest.NewRecorder()
    appHandler().ServeHTTP(w, httptest.NewRequest("GET", "/", nil))
    if w.Code != 200 {
        t.Fatalf("GET / = %d, want 200", w.Code)
    }
    var body = w.Body.String()
    for _, want := range []string{"telegram-web-app.js", "htmx.min.js", `hx-get="fees"`,
        "X-Telegram-Init-Data", `data-panel="home"`, `data-panel="blocks"`,
        `data-panel="addresses"`, `data-panel="miners"`, `id="q"`} {
        if !strings.Contains(body, want) {
            t.Errorf("page is missing %q", want)
        }
    }
}

// HTMX is served from the binary, not a CDN, so the page stays self-contained
// apart from Telegram's own SDK.
func TestAppServesHtmx(t *testing.T) {
    var w = httptest.NewRecorder()
    appHandler().ServeHTTP(w, httptest.NewRequest("GET", "/htmx.min.js", nil))
    if w.Code != 200 || !strings.Contains(w.Body.String(), "htmx") {
        t.Fatalf("GET /htmx.min.js = %d, %d bytes", w.Code, w.Body.Len())
    }
}

func TestAppRejectsOtherPaths(t *testing.T) {
    var w = httptest.NewRecorder()
    appHandler().ServeHTTP(w, httptest.NewRequest("GET", "/nope", nil))
    if w.Code != 404 {
        t.Fatalf("GET /nope = %d, want 404", w.Code)
    }
}

// The whole point of the validation: without a signature there is no data.
func TestAppFeesNeedsInitData(t *testing.T) {
    *botToken = "TESTTOKEN"
    var cases = []struct {
        name string
        data string
    }{
        {"absent", ""},
        {"garbage", "hash=deadbeef&auth_date=1"},
        {"signed with another token", freshInitData("SOMEONE-ELSES-TOKEN")},
    }
    for _, c := range cases {
        var r = httptest.NewRequest("GET", "/fees", nil)
        if c.data != "" { r.Header.Set("X-Telegram-Init-Data", c.data) }
        var w = httptest.NewRecorder()
        appHandler().ServeHTTP(w, r)
        if w.Code != 401 {
            t.Errorf("%s: got %d, want 401", c.name, w.Code)
        }
    }
}

// A tampered field must fail even though the hash itself is well-formed —
// otherwise the signature would be decoration.
func TestAppFeesRejectsTamperedField(t *testing.T) {
    *botToken = "TESTTOKEN"
    var data = freshInitData("TESTTOKEN")
    var v, _ = url.ParseQuery(data)
    v.Set("user", `{"id":999,"first_name":"Mallory"}`)
    var r = httptest.NewRequest("GET", "/fees", nil)
    r.Header.Set("X-Telegram-Init-Data", v.Encode())
    var w = httptest.NewRecorder()
    appHandler().ServeHTTP(w, r)
    if w.Code != 401 {
        t.Fatalf("tampered user field accepted with %d, want 401", w.Code)
    }
}

// A signature stays valid forever on its own, so auth_date is what stops a
// payload lifted from a log or a shared link from working indefinitely.
func TestAppFeesRejectsStaleInitData(t *testing.T) {
    *botToken = "TESTTOKEN"
    var old = signInitData("TESTTOKEN", map[string]string{
        "auth_date": strconv.FormatInt(time.Now().Add(-48*time.Hour).Unix(), 10),
        "user":      `{"id":42}`,
    })
    if _, ok := checkInitData(old, "TESTTOKEN"); ok {
        t.Fatal("a 48h-old payload was accepted; auth_date is not being checked")
    }
    if _, ok := checkInitData(freshInitData("TESTTOKEN"), "TESTTOKEN"); !ok {
        t.Fatal("a fresh payload was rejected")
    }
}

// With a valid signature the fragment is HTML for HTMX to swap in — not JSON —
// carrying the same three tiers /fees prints, read from the cache.
func TestAppFeesRendersHTML(t *testing.T) {
    *botToken = "TESTTOKEN"
    feesMu.Lock()
    cachedFees = recommendedFees{fastest: 12, halfHour: 8, hour: 4, economy: 2, minimum: 1}
    cachedFeesOK, cachedFeesCount = true, 36552
    feesMu.Unlock()
    defer func() { feesMu.Lock(); cachedFeesOK = false; feesMu.Unlock() }()
    var r = httptest.NewRequest("GET", "/fees", nil)
    r.Header.Set("X-Telegram-Init-Data", freshInitData("TESTTOKEN"))
    var w = httptest.NewRecorder()
    appHandler().ServeHTTP(w, r)
    if w.Code != 200 {
        t.Fatalf("GET /fees = %d, want 200", w.Code)
    }
    if ct := w.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/html") {
        t.Errorf("Content-Type = %q, want text/html", ct)
    }
    var body = w.Body.String()
    for _, want := range []string{"<h2>Network fees</h2>", ">Fast<", ">1 hour<", ">2+ hours<",
        ">12 <", ">4 <", ">1 <", "36 552"} {
        if !strings.Contains(body, want) {
            t.Errorf("fragment is missing %q in: %s", want, body)
        }
    }
}

// A cold cache says so rather than rendering zeros as if they were estimates.
func TestAppFeesColdCache(t *testing.T) {
    *botToken = "TESTTOKEN"
    feesMu.Lock()
    cachedFeesOK = false
    feesMu.Unlock()
    var r = httptest.NewRequest("GET", "/fees", nil)
    r.Header.Set("X-Telegram-Init-Data", freshInitData("TESTTOKEN"))
    var w = httptest.NewRecorder()
    appHandler().ServeHTTP(w, r)
    if !strings.Contains(w.Body.String(), "fees unavailable") {
        t.Fatalf("cold cache rendered %q", w.Body.String())
    }
}

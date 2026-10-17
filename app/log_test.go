package app

import "bytes"
import "log"
import "net/http"
import "net/http/httptest"
import "os"
import "regexp"
import "strings"
import "testing"
import "bitnsbot/logging"

// The reader is named by Cloudflare's header, then by True-Client-IP, and only
// without either by the connection — which behind the tunnel is localhost.
func TestClientIP(t *testing.T) {
    var cases = []struct {
        cf, tci, remote, want string
    }{
        {"123.4.5.6", "7.7.7.7", "127.0.0.1:5000", "123.4.5.6"},
        {"", "7.7.7.7", "127.0.0.1:5000", "7.7.7.7"},
        {" 2001:db8::1 ", "", "127.0.0.1:5000", "2001:db8::1"},
        {"", "", "127.0.0.1:5000", "127.0.0.1"},
        {"", "", "[::1]:5000", "::1"},
        {"", "", "pipe", "pipe"},
    }
    for _, c := range cases {
        var r = httptest.NewRequest("GET", "/", nil)
        r.RemoteAddr = c.remote
        if c.cf != "" { r.Header.Set("CF-Connecting-IP", c.cf) }
        if c.tci != "" { r.Header.Set("True-Client-IP", c.tci) }
        if got := ClientIP(r); got != c.want { t.Errorf("CF %q, True-Client-IP %q, from %q: %q, want %q", c.cf, c.tci, c.remote, got, c.want) }
    }
}

// Every request is one INFO line once its handler returns, carrying the address,
// the language and the time, and [cached] only when the handler said so. A
// rejected request is logged as well.
func TestLogRequest(t *testing.T) {
    var buf bytes.Buffer
    log.SetOutput(&buf)
    defer log.SetOutput(os.Stderr)
    logging.SetVerbose(1)
    defer logging.SetVerbose(0)
    var h = Log("mini app", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
        if r.URL.Path == "/hit" { Hit(w) }
        if r.URL.Path == "/no" { http.Error(w, "no", http.StatusUnauthorized) }
    }), func(*http.Request) string { return "ru" })
    for _, path := range []string{"/addresses?kind=rich&to=watches", "/hit", "/no"} {
        var r = httptest.NewRequest("GET", path, nil)
        r.Header.Set("CF-Connecting-IP", "123.4.5.6")
        h.ServeHTTP(httptest.NewRecorder(), r)
    }
    var lines = strings.Split(strings.TrimSpace(buf.String()), "\n")
    var want = []string{
        `\[INFO\] mini app: GET /addresses\?kind=rich&to=watches \[123\.4\.5\.6\] \[ru\] \[\d+\.\d\d ms\]$`,
        `\[INFO\] mini app: GET /hit \[123\.4\.5\.6\] \[ru\] \[\d+\.\d\d ms\] \[cached\]$`,
        `\[INFO\] mini app: GET /no \[123\.4\.5\.6\] \[ru\] \[\d+\.\d\d ms\]$`,
    }
    if len(lines) != len(want) { t.Fatalf("logged %d lines, want %d:\n%s", len(lines), len(want), buf.String()) }
    for i, re := range want {
        if !regexp.MustCompile(re).MatchString(lines[i]) { t.Errorf("line %d: %q, want %s", i, lines[i], re) }
    }
    buf.Reset()
    logging.SetVerbose(0)
    h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("GET", "/hit", nil))
    if buf.Len() != 0 { t.Errorf("logged below INFO: %q", buf.String()) }
}

// The event streams flush through http.NewResponseController, which finds the
// server's writer only through Unwrap — without it a stream behind Log could
// not start.
func TestLogKeepsFlush(t *testing.T) {
    var flushErr error
    var h = Log("mini app", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
        flushErr = http.NewResponseController(w).Flush()
    }), func(*http.Request) string { return "en" })
    var w = httptest.NewRecorder()
    h.ServeHTTP(w, httptest.NewRequest("GET", "/events", nil))
    if flushErr != nil || !w.Flushed { t.Errorf("flush through Log: %v, flushed %v", flushErr, w.Flushed) }
}

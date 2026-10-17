package max

import "bytes"
import "log"
import "net/http/httptest"
import "os"
import "regexp"
import "strings"
import "testing"
import "bitnsbot/logging"

// A request to the MAX Mini App is logged with the reader's address and its one
// language, and the second request for a card says it was served from the
// cache. The handler is driven directly rather than over a socket, so the line
// is written by the time it returns.
func TestRequestsAreLogged(t *testing.T) {
    var rec, _, closing = serveApp(t)
    var h = routes(testToken, rec.options(), closing)
    var buf bytes.Buffer
    log.SetOutput(&buf)
    defer log.SetOutput(os.Stderr)
    logging.SetVerbose(1)
    defer logging.SetVerbose(0)
    for i := 0; i < 2; i++ {
        var r = httptest.NewRequest("GET", "/api/fees", nil)
        r.Header.Set(initHeader, launch(1))
        r.Header.Set("True-Client-IP", "123.4.5.6")
        h.ServeHTTP(httptest.NewRecorder(), r)
    }
    var lines = strings.Split(strings.TrimSpace(buf.String()), "\n")
    var want = []string{
        `\[INFO\] max app: GET /api/fees \[123\.4\.5\.6\] \[ru\] \[\d+\.\d\d ms\]$`,
        `\[INFO\] max app: GET /api/fees \[123\.4\.5\.6\] \[ru\] \[\d+\.\d\d ms\] \[cached\]$`,
    }
    if len(lines) != len(want) { t.Fatalf("logged %d lines, want %d:\n%s", len(lines), len(want), buf.String()) }
    for i, re := range want {
        if !regexp.MustCompile(re).MatchString(lines[i]) { t.Errorf("line %d: %q, want %s", i, lines[i], re) }
    }
}

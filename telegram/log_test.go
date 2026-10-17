package telegram

import "bytes"
import "log"
import "net/http/httptest"
import "os"
import "regexp"
import "strconv"
import "strings"
import "sync"
import "testing"
import "time"
import "bitnsbot/logging"

// lockedLog is a log destination a test can read while the server's own
// goroutine may still be writing its status line to it.
type lockedLog struct {
    mu  sync.Mutex
    buf bytes.Buffer
}

func (l *lockedLog) Write(p []byte) (int, error) {
    l.mu.Lock()
    defer l.mu.Unlock()
    return l.buf.Write(p)
}

// lines is what has been logged that contains marker, one entry a line.
func (l *lockedLog) lines(marker string) []string {
    l.mu.Lock()
    defer l.mu.Unlock()
    var out []string
    for _, line := range strings.Split(l.buf.String(), "\n") {
        if strings.Contains(line, marker) { out = append(out, line) }
    }
    return out
}

// A request to the Mini App is logged with the reader's address and the language
// the page answered in, and the second request for a card says it was served
// from the cache — which is what pins that cached() reaches the writer Log
// handed it, rather than one a wrapper in between put there.
func TestRequestsAreLogged(t *testing.T) {
    var h = handler(t, "TESTTOKEN", fakeSource{f: liveFees()}.options())
    var data = signInitData("TESTTOKEN", map[string]string{
        "auth_date": strconv.FormatInt(time.Now().Unix(), 10),
        "user":      `{"id":42,"language_code":"ru"}`,
    })
    var buf lockedLog
    log.SetOutput(&buf)
    defer log.SetOutput(os.Stderr)
    logging.SetVerbose(1)
    defer logging.SetVerbose(0)
    for i := 0; i < 2; i++ {
        var r = httptest.NewRequest("GET", "/fees", nil)
        r.Header.Set("X-Telegram-Init-Data", data)
        r.Header.Set("CF-Connecting-IP", "123.4.5.6")
        h.ServeHTTP(httptest.NewRecorder(), r)
    }
    var lines = buf.lines("[INFO] mini app:")
    var want = []string{
        `\[INFO\] mini app: GET /fees \[123\.4\.5\.6\] \[ru\] \[\d+\.\d\d ms\]$`,
        `\[INFO\] mini app: GET /fees \[123\.4\.5\.6\] \[ru\] \[\d+\.\d\d ms\] \[cached\]$`,
    }
    if len(lines) != len(want) { t.Fatalf("logged %d lines, want %d:\n%s", len(lines), len(want), strings.Join(lines, "\n")) }
    for i, re := range want {
        if !regexp.MustCompile(re).MatchString(lines[i]) { t.Errorf("line %d: %q, want %s", i, lines[i], re) }
    }
}

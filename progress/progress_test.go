package progress

import "bytes"
import "log"
import "os"
import "strings"
import "testing"
import "time"
import "bitnsbot/logging"

// capture collects what the package logs while fn runs.
func capture(t *testing.T, fn func()) string {
    t.Helper()
    var buf bytes.Buffer
    log.SetOutput(&buf)
    defer log.SetOutput(os.Stderr)
    logging.SetVerbose(0)
    fn()
    return buf.String()
}

// A chunk short of the tip is what the line is for: it names the blocks built,
// how much is left and a forecast of the rest.
func TestChunkReportsProgressAndETA(t *testing.T) {
    var out = capture(t, func() {
        var p = New("addrindex", 1, 1000)
        p.began = time.Now().Add(-10 * time.Second)
        p.Chunk(1, 100)
    })
    for _, want := range []string{"addrindex", "blocks 1..100 of 1000", "10.0%", "900 to go", "ETA"} {
        if !strings.Contains(out, want) { t.Errorf("%q missing from %q", want, out) }
    }
}

// A chunk that reaches the tip says nothing: the catch-up is over, and a rate
// and an ETA for finished work is a line an operator has to read to ignore.
func TestChunkAtTheTipIsSilent(t *testing.T) {
    var out = capture(t, func() { New("blocks", 1, 100).Chunk(1, 100) })
    if out != "" { t.Errorf("logged %q at the tip", out) }
}

// The two largest units and no more, and a rate that keeps a decimal only while
// one changes the forecast.
func TestWaitAndTrim(t *testing.T) {
    for _, c := range []struct{ d time.Duration; want string }{
        {90 * time.Minute, "1h30m"},
        {3*time.Hour + 5*time.Minute, "3h05m"},
        {90 * time.Second, "1m30s"},
        {5 * time.Second, "5s"},
    } {
        if got := wait(c.d); got != c.want { t.Errorf("wait(%s) = %s, want %s", c.d, got, c.want) }
    }
    if got := trim(0.42); got != "0.4" { t.Errorf("trim(0.42) = %s", got) }
    if got := trim(1234.5); got != "1234" { t.Errorf("trim(1234.5) = %s", got) }
}

// A chunk one block short of the tip is not the whole catch-up, however it
// rounds: a share reading 100% with work left is a line that says the scan is
// over when it is not.
func TestShareStaysUnderWholeWhileBlocksRemain(t *testing.T) {
    var out = capture(t, func() { New("addrstat", 0, 20000).Chunk(19000, 19999) })
    if !strings.Contains(out, "99.9%") { t.Errorf("want a share under the whole, got %q", out) }
}

package logging

import "bytes"
import "log"
import "os"
import "path/filepath"
import "strconv"
import "strings"
import "syscall"
import "testing"

// A stream is the journal only when JOURNAL_STREAM names the very file it is:
// the right device and inode are, and a variable that is unset, malformed, or
// names some other file — what a child process whose output was redirected
// inherits — is not.
func TestToJournal(t *testing.T) {
    var f, err = os.Create(filepath.Join(t.TempDir(), "stream"))
    if err != nil { t.Fatal(err) }
    defer f.Close()
    var st syscall.Stat_t
    if statErr := syscall.Fstat(int(f.Fd()), &st); statErr != nil { t.Fatal(statErr) }
    var dev = strconv.FormatUint(uint64(st.Dev), 10)
    var ino = strconv.FormatUint(st.Ino, 10)
    var cases = []struct {
        stream string
        want   bool
    }{
        {dev + ":" + ino, true},
        {dev + ":" + strconv.FormatUint(st.Ino+1, 10), false},
        {"", false},
        {"garbage", false},
        {":", false},
    }
    for _, c := range cases {
        if got := toJournal(c.stream, f); got != c.want { t.Errorf("toJournal(%q) = %v, want %v", c.stream, got, c.want) }
    }
}

// Writing to the journal, each line carries its syslog priority ahead of its
// tag; NET and DB are still cut at maxChars, the prefix not counting. Anywhere
// else no line carries one.
func TestJournalPriorities(t *testing.T) {
    var buf bytes.Buffer
    log.SetOutput(&buf)
    defer log.SetOutput(os.Stderr)
    log.SetFlags(0)
    defer log.SetFlags(log.LstdFlags)
    SetVerbose(2)
    defer SetVerbose(0)
    defer func(was bool) { journal = was }(journal)
    var emit = func() string {
        buf.Reset()
        Err("e %d", 1)
        Warn("w")
        Status("s")
        Info("i")
        Net("%s", strings.Repeat("n", 151))
        Db("select\n    1")
        return buf.String()
    }
    journal = true
    var want = "<3>[ERR] e 1\n<4>[WARN] w\n<5>s\n<6>[INFO] i\n<7>[NET] " + strings.Repeat("n", 150) + "...\n<7>[DB] select 1\n"
    if got := emit(); got != want { t.Errorf("to the journal logged\n%q\nwant\n%q", got, want) }
    journal = false
    want = "[ERR] e 1\n[WARN] w\ns\n[INFO] i\n[NET] " + strings.Repeat("n", 150) + "...\n[DB] select 1\n"
    if got := emit(); got != want { t.Errorf("elsewhere logged\n%q\nwant\n%q", got, want) }
}

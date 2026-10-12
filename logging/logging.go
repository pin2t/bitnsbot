package logging

import "fmt"
import "log"
import "os"
import "strconv"
import "strings"
import "syscall"

// verbose gates the leveled helpers; set once at startup from the -verbose
// flag via SetVerbose. 0 = ERR/WARN/status, 1 = +INFO, 2 = +NET/DB.
var verbose int

// journal is whether the log goes straight to the systemd journal. When it
// does, every line starts with its syslog priority — <3> an error, <4> a
// warning, <5> a status line, <6> INFO, <7> NET and DB — which journald takes
// off the line and stores as the entry's priority, so journalctl -p err finds
// the errors; and the date and time are left off, since the journal stamps
// every entry itself.
var journal = toJournal(os.Getenv("JOURNAL_STREAM"), os.Stderr)

func init() {
    if journal { log.SetFlags(0) }
}

// toJournal reports whether f is the journal stream systemd describes in
// JOURNAL_STREAM ("device:inode"). The variable alone is not enough: a child
// process inherits it with its output going somewhere else — go test run by a
// service, its output piped back to the go command — so it is compared with
// what f actually is, which is how systemd says to use it.
func toJournal(stream string, f *os.File) bool {
    var dev, ino, ok = strings.Cut(stream, ":")
    if !ok { return false }
    var st syscall.Stat_t
    if err := syscall.Fstat(int(f.Fd()), &st); err != nil { return false }
    return dev == strconv.FormatUint(uint64(st.Dev), 10) && ino == strconv.FormatUint(st.Ino, 10)
}

// pri is the syslog priority prefix for level when the log is the journal,
// and nothing otherwise.
func pri(level string) string {
    if journal { return level }
    return ""
}

func SetVerbose(v int) { verbose = v }
func Err(format string, args ...any)    { log.Printf(pri("<3>")+"[ERR] "+format, args...) }
func Warn(format string, args ...any)   { log.Printf(pri("<4>")+"[WARN] "+format, args...) }
func Status(format string, args ...any) { log.Printf(pri("<5>")+format, args...) }
func Fatal(format string, args ...any)  { log.Fatalf(pri("<3>")+"[ERR] "+format, args...) }

func Info(format string, args ...any) {
    if verbose >= 1 { log.Printf(pri("<6>")+"[INFO] "+format, args...) }
}

// maxChars is the most of a NET or DB message that is logged: a Core reply can
// be megabytes of JSON and a query a screenful of SQL, and the first characters
// already say which one it was.
const maxChars = 150

// cut shortens a message longer than maxChars to its first maxChars followed by
// "...", counting characters rather than bytes so a Russian message is not
// split inside one.
func cut(msg string) string {
    var n = 0
    for i := range msg {
        if n == maxChars { return msg[:i] + "..." }
        n++
    }
    return msg
}

func Net(format string, args ...any) {
    if verbose >= 2 { log.Print(pri("<7>") + "[NET] " + cut(fmt.Sprintf(format, args...))) }
}

// Db logs a message on one line, its runs of whitespace collapsed to a space,
// because a query is written across several lines in the source.
func Db(format string, args ...any) {
    if verbose >= 2 { log.Print(pri("<7>") + "[DB] " + cut(strings.Join(strings.Fields(fmt.Sprintf(format, args...)), " "))) }
}

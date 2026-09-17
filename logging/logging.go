package logging

import "fmt"
import "log"
import "strings"

// verbose gates the leveled helpers; set once at startup from the -verbose
// flag via SetVerbose. 0 = ERR/WARN/status, 1 = +INFO, 2 = +NET/DB.
var verbose int

func SetVerbose(v int) { verbose = v }
func DisableTimestamp() { log.SetFlags(0) }
func Err(format string, args ...any)    { log.Printf("[ERR] "+format, args...) }
func Warn(format string, args ...any)   { log.Printf("[WARN] "+format, args...) }
func Status(format string, args ...any) { log.Printf(format, args...) }
func Fatal(format string, args ...any)  { log.Fatalf("[ERR] "+format, args...) }

func Info(format string, args ...any) {
    if verbose >= 1 { log.Printf("[INFO] "+format, args...) }
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
    if verbose >= 2 { log.Print("[NET] " + cut(fmt.Sprintf(format, args...))) }
}

// Db logs a message on one line, its runs of whitespace collapsed to a space,
// because a query is written across several lines in the source.
func Db(format string, args ...any) {
    if verbose >= 2 { log.Print("[DB] " + cut(strings.Join(strings.Fields(fmt.Sprintf(format, args...)), " "))) }
}

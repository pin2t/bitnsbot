package logging

import "log"

// verbosity gates the leveled helpers; set once at startup from the -verbose
// flag via SetVerbosity. 0 = ERR/WARN/status, 1 = +INFO, 2 = +NET/DB.
var verbosity int

func SetVerbosity(v int) { verbosity = v }

func Err(format string, args ...any)    { log.Printf("[ERR] "+format, args...) }
func Warn(format string, args ...any)   { log.Printf("[WARN] "+format, args...) }
func Status(format string, args ...any) { log.Printf(format, args...) }
func Fatal(format string, args ...any)  { log.Fatalf("[ERR] "+format, args...) }

func Info(format string, args ...any) {
    if verbosity >= 1 { log.Printf("[INFO] "+format, args...) }
}

func Net(format string, args ...any) {
    if verbosity >= 2 { log.Printf("[NET] "+format, args...) }
}

func Db(format string, args ...any) {
    if verbosity >= 2 { log.Printf("[DB] "+format, args...) }
}

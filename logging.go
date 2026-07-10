package main

import "log"

func logErr(format string, args ...any)  { log.Printf("[ERR] "+format, args...) }
func logWarn(format string, args ...any) { log.Printf("[WARN] "+format, args...) }
func logStatus(format string, args ...any) { log.Printf(format, args...) }

func logFatal(format string, args ...any) { log.Fatalf("[ERR] "+format, args...) }

func logInfo(format string, args ...any) {
    if *verbose >= 1 { log.Printf("[INFO] "+format, args...) }
}

func logNet(format string, args ...any) {
    if *verbose >= 2 { log.Printf("[NET] "+format, args...) }
}

func logDb(format string, args ...any) {
    if *verbose >= 2 { log.Printf("[DB] "+format, args...) }
}

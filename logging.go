package main

import "log"

// verbosity gates which log levels are emitted (set from -verbose in main()):
//   0 (default) — ERR, WARN, and status lines (Listening, Shutting down, ...)
//   1           — adds INFO (messages sent, subscriptions added/removed, ...)
//   2           — adds NET (raw btcd/Telegram traffic) and DB (storage requests)
// ERR, WARN and status always print so failures and lifecycle are never hidden.
var verbosity int

func logErr(format string, args ...any)  { log.Printf("[ERR] "+format, args...) }
func logWarn(format string, args ...any) { log.Printf("[WARN] "+format, args...) }
func logStatus(format string, args ...any) { log.Printf(format, args...) }

func logFatal(format string, args ...any) { log.Fatalf("[ERR] "+format, args...) }

func logInfo(format string, args ...any) {
    if verbosity >= 1 { log.Printf("[INFO] "+format, args...) }
}

func logNet(format string, args ...any) {
    if verbosity >= 2 { log.Printf("[NET] "+format, args...) }
}

func logDb(format string, args ...any) {
    if verbosity >= 2 { log.Printf("[DB] "+format, args...) }
}

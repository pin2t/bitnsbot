package main

import "context"
import "time"

import "bitnsbot/addrindex"
import "bitnsbot/logging"

// startAddrIndex launches the address-index backfill against Core's REST
// interface, logging plainly if -rest isn't enabled rather than failing
// startup — the index is a convenience for /info <address> history, not a
// dependency anything else needs. The REST client itself lives in the addrindex
// package, so this bot and tools/addrindex build the index the same way.
func startAddrIndex(restBaseURL string) {
    var src = addrindex.NewREST(restBaseURL)
    var ctx, cancel = context.WithTimeout(context.Background(), 15*time.Second)
    defer cancel()
    if _, err := src.Tip(ctx); err != nil {
        logging.Warn("address index disabled: Core's REST interface is unreachable at %s (%v) — enable -rest=1", restBaseURL, err)
        return
    }
    addrindex.StartBackfill(src)
}

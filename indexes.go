package main

import "context"
import "time"
import "bitnsbot/addrbal"
import "bitnsbot/addrindex"
import "bitnsbot/addrstat"
import "bitnsbot/core"
import "bitnsbot/logging"
import "bitnsbot/miners"
import "bitnsbot/signals"

// indexUpdateInterval is how often the indexes catch up to the tip; a block
// notification cuts the wait short, so this is the floor under which nothing is
// missed rather than the usual cadence.
var indexUpdateInterval = 10 * time.Minute

// startIndexUpdates runs the one goroutine that keeps every chain-derived index
// current, where each used to run a goroutine of its own — the block cache, the
// miner statistics, the address index, the per-address statistics and the
// address balances. On a block notification and every indexUpdateInterval it
// fetches the tip once and walks each index up to it, sequentially, in that
// order: one tip read and one catch-up at a time, never five scans competing for
// the same node.
//
// Each Update resumes at its own cursor, so a failure stops that index alone —
// the one behind it still runs, and the next pass retries them all.
func startIndexUpdates() {
    if !core.Enabled() { return }
    go func() {
        var wake = signals.Subscribe(signals.Block)
        updateIndexes()
        var t = time.NewTicker(indexUpdateInterval)
        defer t.Stop()
        for {
            select {
            case <-t.C:
            case <-wake:
            }
            updateIndexes()
        }
    }()
}

// updateIndexes fetches the tip once and brings each index up to it, in order:
// blocks, miner statistics, addrindex, addrstat, addrbal.
func updateIndexes() {
    var ctx, cancel = context.WithTimeout(context.Background(), 15*time.Second)
    var tip, err = core.GetBlockCount(ctx)
    cancel()
    if err != nil {
        logging.Warn("indexes: tip: %v", err)
        return
    }
    updateBlocks(tip)
    miners.Update(tip)
    if err := addrindex.Update(tip); err != nil {
        logging.Warn("addrindex: %v", err)
    }
    if err := addrstat.Update(tip); err != nil {
        logging.Warn("addrstat: %v", err)
    }
    if err := addrbal.Update(tip); err != nil {
        logging.Warn("addrbal: %v", err)
    }
}

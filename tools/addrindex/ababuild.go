package main

import "context"
import "fmt"
import "sync"
import "time"

import "bitnsbot/addrindex"
import "bitnsbot/logging"

// ababuild finds the coins nobody has touched for longest: the addresses that
// hold a balance and whose owner last spent from them further back than anyone
// else's. It reads the chain exactly as richbuild does — every block and the
// prevouts its inputs spend, over Core's REST interface — and keeps one more
// thing per script as it goes: when that script was last spent from, and when it
// was last paid.
//
// A spend is the only evidence that somebody still holds the key. An address can
// be paid by a stranger at any time, so receiving says nothing about whether its
// owner is still there; signing a spend says everything. That is why the ranking
// is by last spend, and why an address that has never spent at all is ranked by
// when its coins arrived — it has been silent for the whole of its life.
//
// The shape of the run is richbuild's, and for the same reasons: movements go to
// sharded files as the chain is read, because neither the movements nor the
// running total fit in memory (see shards.go); the shards are summed
// afterwards, -sum of them at once, since each one is independent of the others;
// and every funded script is written to the database before the answer is
// selected, because the scripts of one address land in different shards and no
// shard can rank an address it holds only part of. See abadb.go for what the
// database ends up holding.
func ababuild(opt *options) error {
    if opt.dbsqlite == "" {
        return fmt.Errorf("ababuild writes SQLite: name the database with -dbsqlite")
    }
    if opt.top < 1 {
        return fmt.Errorf("-top is how many addresses to keep, so it cannot be %d", opt.top)
    }
    var store, err = openAba(opt.dbsqlite)
    if err != nil { return fmt.Errorf("open %s: %w", opt.dbsqlite, err) }
    defer store.close()
    var ctx, cancel = context.WithCancel(context.Background())
    defer cancel()
    var src = addrindex.NewREST(opt.url)
    var tipCtx, tipCancel = context.WithTimeout(ctx, 30*time.Second)
    var tip, terr = src.Tip(tipCtx)
    tipCancel()
    if terr != nil {
        return fmt.Errorf("Core REST is unreachable at %s (%v) — enable -rest=1", opt.url, terr)
    }
    if opt.to > 0 && opt.to < tip { tip = opt.to }
    var chain, size = chainFacts(ctx, opt.url)
    var at, built, herr = store.height("height")
    if herr != nil { return herr }
    var from = 0
    if built {
        from = at + 1
        var stored, _, merr = store.meta("hash")
        if merr != nil { return merr }
        if err := sameChain(ctx, opt.url, stored, at); err != nil { return err }
    }
    if from > tip {
        return alreadyBuilt(store, opt, at, tip)
    }
    var dir = opt.tmp
    if dir == "" { dir = opt.dbsqlite + ".shards" }
    var sh, serr = newTimedShards(dir, opt.shards, shardBufferKB)
    if serr != nil { return serr }
    defer sh.remove()
    fmt.Printf("Tracking balances and spends over blocks %d..%d of %s into %s (%d shards under %s)\n",
        from, tip, chain, opt.dbsqlite, opt.shards, dir)
    var started = time.Now()
    // A run that carries on from a stored height starts with what that height
    // left: every balance goes back in as one movement, carrying the dates it
    // was stored with, so the shards hold the whole history and not just this
    // run's part of it.
    //
    // The one thing a resumed run cannot recover is the spending date of a
    // script that held nothing at the height it stopped at and was paid again
    // afterwards: only funded scripts are stored, so that script comes back as
    // one this run has never seen spend, and is ranked by the payment instead.
    // Keeping the emptied ones would mean storing every script the chain has
    // ever had — over a billion rows against sixty million — to hold a date for
    // the few that are ever refunded. So a resumed run can rank such an address
    // as less abandoned than a run from genesis would; nothing else differs, and
    // TestAbaBuildCarriesStateForward pins that.
    if built {
        var seeded int
        if err := store.each(func(script []byte, balance, spent, paid int64) error {
            seeded++
            return sh.putAt(string(script), balance, spent, paid)
        }); err != nil {
            return fmt.Errorf("read stored balances: %w", err)
        }
        fmt.Printf("Carried %s balances forward from block %d\n", group(int64(seeded)), at)
    }
    var last, scanErr = track(ctx, src, sh, opt, chain, from, tip, size, started)
    if scanErr != nil { return scanErr }
    if err := sh.flush(); err != nil { return err }
    fmt.Printf("Read blocks %d..%d in %s: %s movements, %.1f GB of shards\n",
        from, tip, took(time.Since(started)), group(sh.records), float64(sh.bytes)/1e9)
    if err := combine(store, sh, opt, tip, last); err != nil { return err }
    return rank(store, opt, tip, started)
}

// move is what one script did over the stretch of chain a buffer or a shard
// covers: how much its balance changed, and the latest block time on each side.
// The times are kept apart rather than reduced to one, because which side a date
// came from is the whole ranking — a payment received does not answer for a
// spend that never happened.
type move struct {
    sat   int64
    spent int64
    paid  int64
}

// at folds one movement in, taking the later of two dates rather than the one
// from the later block. Block timestamps are not strictly ordered — a miner's
// clock may legitimately run up to two hours behind the block before it — so the
// last spend by height can carry an earlier date than one before it, and reading
// the ranking off that would make an address look more abandoned than it is.
func (m *move) at(sat, when int64, spend bool) {
    m.sat += sat
    if spend {
        if when > m.spent { m.spent = when }
        return
    }
    if when > m.paid { m.paid = when }
}

// track walks the blocks, turning each into the movements it makes and the dates
// they happened on, and buffering them until there are enough to write out.
func track(ctx context.Context, src *addrindex.REST, sh *shards, opt *options,
    chain string, from, tip int, size int64, started time.Time) (string, error) {
    var buf = make(map[string]move, opt.batch)
    var reported = time.Now()
    var read int64
    var last string
    for f := range stream(ctx, src, from, tip, opt.fetch) {
        if f.err != nil { return "", fmt.Errorf("block %d: %w", f.height, f.err) }
        var moves, ok = addrindex.Movements(f.blk)
        if !ok { return "", fmt.Errorf("could not parse block %d (%s)", f.height, f.blk.Hash) }
        var when, timed = addrindex.BlockTime(f.blk.Raw)
        if !timed { return "", fmt.Errorf("block %d (%s) is shorter than its own header", f.height, f.blk.Hash) }
        last = f.blk.Hash
        for _, m := range moves {
            var e = buf[string(m.Script)]
            e.at(m.Sat, when, m.Spend)
            buf[string(m.Script)] = e
        }
        // The outputs Core's UTXO set never held are taken back out of the
        // balance, but not out of the dates: a script whose only payment is one
        // of these ends up holding nothing and never reaches the answer table,
        // and every other script involved was paid again by the copy that did
        // survive, which is later and therefore wins the max anyway.
        for _, o := range voided(chain, f.height, f.blk.Raw) {
            var e = buf[string(o.Script)]
            e.sat -= o.Sat
            buf[string(o.Script)] = e
        }
        read += int64(len(f.blk.Raw) + len(f.blk.Spent))
        if len(buf) >= opt.batch {
            if err := spill(sh, buf); err != nil { return "", err }
        }
        if time.Since(reported) >= time.Minute {
            reported = time.Now()
            logging.Info("ababuild: block %d of %d, %s movements written, %.1f GB of shards, %s",
                f.height, tip, group(sh.records), float64(sh.bytes)/1e9, reading(started, read, size))
        }
    }
    return last, spill(sh, buf)
}

// spill writes the buffered movements to their shards and empties the buffer.
//
// Nothing is dropped for netting to zero, which is where this parts company with
// richbuild's flush: an address funded and emptied again inside one buffer adds
// nothing to anybody's balance, but it did spend, and the date it spent on is
// the thing being measured. Should it ever be paid again it will end the run
// holding coins, and its last spend has to be that date and not the payment
// that followed. The cost is a fuller shard file, which is disk.
func spill(sh *shards, buf map[string]move) error {
    for script, m := range buf {
        if err := sh.putAt(script, m.sat, m.spent, m.paid); err != nil { return err }
    }
    clear(buf)
    return nil
}

// combine adds the shards up and writes every funded script to the state. The
// shards are independent — a script's every movement is in exactly one of them —
// so -sum of them are summed at once, which is what turns a long single-threaded
// tail into a job the machine's cores share. It is also what sets the run's peak
// memory: one shard's scripts are in a map while it is being summed, so -sum
// times the largest shard is the figure to watch, and -shards is the lever that
// makes each one smaller.
//
// The database takes one writer whatever else is going on, so the rows go in
// under a lock while the summing that produced them does not.
func combine(store *abaStore, sh *shards, opt *options, tip int, hash string) error {
    var st, err = store.newState()
    if err != nil { return err }
    var started = time.Now()
    var workers = opt.sum
    if workers < 1 { workers = 1 }
    if workers > opt.shards { workers = opt.shards }
    var mu sync.Mutex
    var next, rows, negative, largest int
    var failed error
    var wg sync.WaitGroup
    for w := 0; w < workers; w++ {
        wg.Add(1)
        go func() {
            defer wg.Done()
            for {
                mu.Lock()
                if failed != nil || next >= opt.shards {
                    mu.Unlock()
                    return
                }
                var i = next
                next++
                mu.Unlock()
                var sums = map[string]move{}
                if err := sh.eachAt(i, func(script []byte, sat, spent, paid int64) error {
                    var e = sums[string(script)]
                    e.at(sat, spent, true)
                    e.at(0, paid, false)
                    sums[string(script)] = e
                    return nil
                }); err != nil {
                    mu.Lock()
                    if failed == nil { failed = err }
                    mu.Unlock()
                    return
                }
                // The addresses are encoded here rather than under the lock:
                // hashing a script is the expensive part of this loop, and there
                // are tens of millions of them, so doing it while another shard
                // is being summed is the point of summing several at once.
                var out = make([]abaRow, 0, len(sums))
                var neg int
                for script, m := range sums {
                    if m.sat == 0 { continue }
                    // A balance cannot go below zero on a chain that only ever
                    // spends outputs that exist, so one that does means this
                    // run's own arithmetic is wrong somewhere — say so rather
                    // than write it.
                    if m.sat < 0 {
                        neg++
                        logging.Warn("ababuild: %s holds %d sat, which cannot happen",
                            scriptAddress([]byte(script)), m.sat)
                        continue
                    }
                    out = append(out, abaRow{script: []byte(script), addr: scriptAddress([]byte(script)), m: m})
                }
                var scripts = len(sums)
                sums = nil
                mu.Lock()
                negative += neg
                if scripts > largest { largest = scripts }
                for _, r := range out {
                    if err := st.add(r); err != nil {
                        if failed == nil { failed = err }
                        mu.Unlock()
                        return
                    }
                    rows++
                }
                logging.Info("ababuild: shard %d of %d summed: %s scripts, %s with a balance, %s",
                    i, opt.shards-1, group(int64(scripts)), group(int64(rows)), took(time.Since(started)))
                mu.Unlock()
                if err := sh.done(i); err != nil {
                    mu.Lock()
                    if failed == nil { failed = err }
                    mu.Unlock()
                    return
                }
            }
        }()
    }
    wg.Wait()
    if failed != nil {
        st.rollback()
        return failed
    }
    if negative > 0 {
        st.rollback()
        return fmt.Errorf("%d scripts ended below zero — the scan is wrong, refusing to store it", negative)
    }
    fmt.Printf("Summed %d shards %d at a time in %s: %s funded scripts, %s in the largest shard\n",
        opt.shards, workers, took(time.Since(started)), group(int64(rows)), group(int64(largest)))
    return st.commit(tip, hash)
}

// alreadyBuilt is the run that has nothing to scan. It still rebuilds the answer
// table when that table is behind the state it comes from — which is how a run
// interrupted after the state was committed finishes the job rather than
// starting over — or when it was built with a different -min or -top.
func alreadyBuilt(store *abaStore, opt *options, at, tip int) error {
    var builtAt, ok, err = store.height("abandoned")
    if err != nil { return err }
    var same, serr = sameFlags(store, opt)
    if serr != nil { return serr }
    if ok && builtAt == at && same {
        fmt.Printf("The list is already at block %d (tip %d)\n", at, tip)
        return nil
    }
    fmt.Printf("Balances stand at block %d; ranking them again\n", at)
    return rank(store, opt, at, time.Now())
}

// sameFlags reports whether the stored answer table was selected with the
// thresholds this run was given. It is not enough to be at the right height: a
// new -top or -min asks a different question of the same state.
func sameFlags(store *abaStore, opt *options) (bool, error) {
    for key, want := range map[string]string{
        "min": fmt.Sprint(opt.min),
        "top": fmt.Sprint(opt.top),
    } {
        var have, ok, err = store.meta(key)
        if err != nil { return false, err }
        if !ok || have != want { return false, nil }
    }
    return true, nil
}

// rank rebuilds the answer table out of the stored state and prints what the run
// left, including the span of dates the list covers — which is the one figure
// that says whether the threshold reached anywhere interesting.
func rank(store *abaStore, opt *options, height int, started time.Time) error {
    var built = time.Now()
    var rows, err = store.shortlist(opt.min, opt.top, height)
    if err != nil { return err }
    var _, sat, oldest, newest, terr = store.totals()
    if terr != nil { return terr }
    fmt.Printf("Wrote %s addresses to abandoned in %s, holding %s sat (%.2f BTC), at block %d\n",
        group(int64(rows)), took(time.Since(built)), group(sat), float64(sat)/1e8, height)
    if rows > 0 {
        fmt.Printf("Last moved between %s and %s\n", date(oldest), date(newest))
    }
    fmt.Printf("Done in %s\n", took(time.Since(started)))
    return nil
}

// date renders a stored lastTx, which is a block's own timestamp.
func date(unix int64) string { return time.Unix(unix, 0).UTC().Format("2 January 2006") }

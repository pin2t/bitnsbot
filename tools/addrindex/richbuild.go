package main

import "context"
import "encoding/json"
import "fmt"
import "net/http"
import "strconv"
import "time"

import "bitnsbot/addrindex"
import "bitnsbot/logging"

// richbuild adds up what every address on the chain currently holds and writes
// it to SQLite, by reading each block and the prevouts its inputs spend: an
// output pays its script, spending one takes the same amount back out, and
// nothing else moves value. Summed from genesis to the tip that is the UTXO set
// aggregated by address, which is what the rich table holds.
//
// The scan reads Core's REST interface, the same two endpoints the address index
// is built from — /rest/block and /rest/spenttxouts, which is the only place a
// spend's script and amount are written down. RPC would answer the same
// questions in JSON at roughly thirty times the size.
//
// Nothing chain-sized is held in memory. Movements go to sharded files as they
// are read, each shard is summed on its own afterwards, and only the surviving
// balances — tens of millions of rows rather than billions of movements — are
// written to the database. See shards.go for why not SQLite the whole way, and
// richdb.go for what the database ends up holding.
func richbuild(opt *options) error {
    if opt.dbsqlite == "" {
        return fmt.Errorf("richbuild writes SQLite: name the database with -dbsqlite")
    }
    var store, err = openRich(opt.dbsqlite)
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
        if err := sameChain(ctx, opt.url, store, at); err != nil { return err }
    }
    if from > tip {
        return atTip(store, opt, at, tip)
    }
    var dir = opt.tmp
    if dir == "" { dir = opt.dbsqlite + ".shards" }
    var sh, serr = newShards(dir, opt.shards, shardBufferKB)
    if serr != nil { return serr }
    defer sh.remove()
    fmt.Printf("Summing balances over blocks %d..%d of %s into %s (%d shards under %s)\n",
        from, tip, chain, opt.dbsqlite, opt.shards, dir)
    var started = time.Now()
    // A run that carries on from a stored height starts with what that height
    // left: every balance goes back in as one movement, so the shards hold the
    // whole history's total and not just this run's part of it.
    if built {
        var seeded int
        if err := store.each(func(script []byte, balance int64) error {
            seeded++
            return sh.put(string(script), balance)
        }); err != nil {
            return fmt.Errorf("read stored balances: %w", err)
        }
        fmt.Printf("Carried %s balances forward from block %d\n", group(int64(seeded)), at)
    }
    var last, scanErr = scan(ctx, src, sh, opt, chain, from, tip, size, started)
    if scanErr != nil { return scanErr }
    if err := sh.flush(); err != nil { return err }
    fmt.Printf("Read blocks %d..%d in %s: %s movements, %.1f GB of shards\n",
        from, tip, took(time.Since(started)), group(sh.records), float64(sh.bytes)/1e9)
    if err := aggregate(store, sh, opt, tip, last); err != nil { return err }
    return report(store, opt, tip, started)
}

// shardBufferKB is the write buffer each shard file gets. The movements arrive
// interleaved, so this is what turns a few hundred bytes written to one of a
// hundred places into one sequential write per shard — at the cost of holding
// shards x this much memory, which is why it is small.
const shardBufferKB = 512

// scan walks the blocks, turning each into the balance movements it makes and
// buffering them until there are enough to be worth writing out.
func scan(ctx context.Context, src *addrindex.REST, sh *shards, opt *options,
    chain string, from, tip int, size int64, started time.Time) (string, error) {
    var buf = make(map[string]int64, opt.batch)
    var reported = time.Now()
    var read int64
    var last string
    for f := range stream(ctx, src, from, tip, opt.fetch) {
        if f.err != nil { return "", fmt.Errorf("block %d: %w", f.height, f.err) }
        var moves, ok = addrindex.Balances(f.blk)
        if !ok { return "", fmt.Errorf("could not parse block %d (%s)", f.height, f.blk.Hash) }
        last = f.blk.Hash
        for _, m := range moves { buf[string(m.Script)] += m.Sat }
        for _, o := range voided(chain, f.height, f.blk.Raw) { buf[string(o.Script)] -= o.Sat }
        read += int64(len(f.blk.Raw) + len(f.blk.Spent))
        if len(buf) >= opt.batch {
            if err := flush(sh, buf); err != nil { return "", err }
        }
        if time.Since(reported) >= time.Minute {
            reported = time.Now()
            logging.Info("richbuild: block %d of %d, %s movements written, %.1f GB of shards, %s",
                f.height, tip, group(sh.records), float64(sh.bytes)/1e9, reading(started, read, size))
        }
    }
    return last, flush(sh, buf)
}

// reading reports how fast the chain is coming in and how much of it is left. It
// counts bytes rather than blocks, and against what the node says it holds: a
// block from 2011 is a few hundred bytes where one from this year is well over a
// megabyte, so a share of the block count would promise the tip in minutes for
// most of a run that takes hours. Core's size_on_disk covers the block files and
// the undo data, which is exactly the pair of endpoints this reads.
func reading(started time.Time, read, size int64) string {
    var elapsed = time.Since(started)
    if elapsed <= 0 || read <= 0 { return "" }
    var out = fmt.Sprintf("%.0f MB/sec", float64(read)/1e6/elapsed.Seconds())
    if size > read {
        out += fmt.Sprintf(", %.1f%% of %.0f GB, ETA %s", 100*float64(read)/float64(size),
            float64(size)/1e9, took(time.Duration(float64(elapsed)*float64(size-read)/float64(read))))
    }
    return out
}

// flush writes the buffered movements to their shards and empties the buffer. A
// script whose movements cancel out while it is buffered — an address funded and
// emptied again before the buffer filled, which is most of what a change address
// ever does — is dropped rather than written: adding nothing to a balance is
// nothing, and there is a great deal of it.
func flush(sh *shards, buf map[string]int64) error {
    for script, sat := range buf {
        if sat == 0 { continue }
        if err := sh.put(script, sat); err != nil { return err }
    }
    clear(buf)
    return nil
}

// voided undoes the three outputs the chain carries that Core's UTXO set never
// held, all three confirmed against a live mainnet node rather than taken from
// documentation. The genesis coinbase is not in that set at all — Core
// never writes it there, and gettxout reports nothing for it. And BIP-30: the
// coinbase transactions of blocks 91722 and 91812 were mined again, byte for
// byte, in 91880 and 91842; the second copy overwrote the first, so 50 BTC of
// each pair belongs to nobody. All three are the same rule — outputs that never
// entered the set — so all three are undone the same way.
//
// Only mainnet has them, which is why the chain the node reports is checked
// first: on regtest or testnet these heights are ordinary blocks.
func voided(chain string, height int, raw []byte) []addrindex.Payment {
    if chain != "main" { return nil }
    if height != 0 && height != 91722 && height != 91812 { return nil }
    var txs, ok = addrindex.OutputsByTx(raw)
    if !ok || len(txs) == 0 { return nil }
    return txs[0]
}

// fetched is one block's raw material, or the error that stopped it.
type fetched struct {
    height int
    blk    addrindex.Block
    err    error
}

// stream hands the blocks over in height order while fetching several of them at
// once. Each block is three REST requests against a node on the same machine, so
// a sequential scan spends most of its time waiting: measured against this
// repo's own node, four fetches at a time took recent blocks from 71 to 116 a
// second and 120 to 197 MB/s. Order still matters — a balance is a running total
// — so each height gets its own one-slot channel and the results are read in the
// order the heights were queued.
func stream(ctx context.Context, src *addrindex.REST, from, to, workers int) <-chan fetched {
    if workers < 1 { workers = 1 }
    var queue = make(chan chan fetched, workers)
    go func() {
        defer close(queue)
        for h := from; h <= to; h++ {
            var height = h
            var c = make(chan fetched, 1)
            go func() {
                var blk, err = fetchBlock(ctx, src, height)
                c <- fetched{height: height, blk: blk, err: err}
            }()
            select {
            case queue <- c:
            case <-ctx.Done():
                return
            }
        }
    }()
    var out = make(chan fetched)
    go func() {
        defer close(out)
        for c := range queue {
            select {
            case out <- <-c:
            case <-ctx.Done():
                return
            }
        }
    }()
    return out
}

// fetchBlock reads one block, trying again if the node does not answer. A run
// over the whole chain is hours and a million requests long, and abandoning all
// of it because the node was busy for a moment would mean starting the scan
// again from the last stored height. Only the last error is reported, since a
// height that fails three times is failing for one reason.
func fetchBlock(ctx context.Context, src *addrindex.REST, height int) (addrindex.Block, error) {
    var blk addrindex.Block
    var err error
    for attempt := 0; attempt < fetchAttempts; attempt++ {
        if attempt > 0 {
            logging.Warn("richbuild: block %d: %v — trying again", height, err)
            select {
            case <-time.After(time.Duration(attempt) * fetchBackoff):
            case <-ctx.Done():
                return blk, ctx.Err()
            }
        }
        blk, err = src.BlockAt(ctx, height)
        if err == nil || ctx.Err() != nil { return blk, err }
    }
    return blk, err
}

const fetchAttempts = 3
const fetchBackoff = 2 * time.Second

// aggregate adds each shard up on its own and writes what survives. Only one
// shard's scripts are in memory at a time, so this is where -shards decides the
// run's peak memory: more shards, less of the chain in each.
func aggregate(store *richStore, sh *shards, opt *options, tip int, hash string) error {
    var st, err = store.newState()
    if err != nil { return err }
    var started = time.Now()
    var rows, negative int
    var largest int
    for i := 0; i < opt.shards; i++ {
        var sums = map[string]int64{}
        if err := sh.each(i, func(script []byte, sat int64) error {
            sums[string(script)] += sat
            return nil
        }); err != nil {
            st.rollback()
            return err
        }
        if len(sums) > largest { largest = len(sums) }
        for script, balance := range sums {
            if balance == 0 { continue }
            // A balance cannot go below zero on a chain that only ever spends
            // outputs that exist, so one that does means this run's own
            // arithmetic is wrong somewhere — say so rather than write it.
            if balance < 0 {
                negative++
                logging.Warn("richbuild: %s holds %d sat, which cannot happen", scriptAddress([]byte(script)), balance)
                continue
            }
            if err := st.add([]byte(script), scriptAddress([]byte(script)), balance); err != nil {
                st.rollback()
                return err
            }
            rows++
        }
        if err := sh.done(i); err != nil { return err }
        logging.Info("richbuild: shard %d of %d summed: %s scripts, %s with a balance, %s",
            i, opt.shards-1, group(int64(len(sums))), group(int64(rows)), took(time.Since(started)))
    }
    if negative > 0 {
        st.rollback()
        return fmt.Errorf("%d scripts ended below zero — the scan is wrong, refusing to store it", negative)
    }
    fmt.Printf("Summed %d shards in %s: %s funded scripts, %s in the largest shard\n",
        opt.shards, took(time.Since(started)), group(int64(rows)), group(int64(largest)))
    return st.commit(tip, hash)
}

// sameChain refuses to carry stored balances forward onto a chain they were not
// summed on. A run ends at whatever block was the tip, and a tip can be reorged
// away minutes later — after which resuming from the stored height would keep
// counting coins from a block the node no longer has, quietly and for good.
// Balances written before this check are simply not checked.
func sameChain(ctx context.Context, baseURL string, store *richStore, at int) error {
    var stored, ok, err = store.meta("hash")
    if err != nil || !ok { return err }
    var now, herr = blockHash(ctx, baseURL, at)
    if herr != nil { return fmt.Errorf("read block %d: %w", at, herr) }
    if now == stored { return nil }
    return fmt.Errorf("block %d is %s on the node but the stored balances were summed to %s — "+
        "that block was reorged away, so delete %s and build it again", at, now, stored, "the database")
}

// blockHash asks REST for the block at a height, which is the cheapest question
// the node answers about one.
func blockHash(ctx context.Context, baseURL string, height int) (string, error) {
    var req, err = http.NewRequestWithContext(ctx, http.MethodGet,
        fmt.Sprintf("%s/rest/blockhashbyheight/%d.json", baseURL, height), nil)
    if err != nil { return "", err }
    var resp, derr = http.DefaultClient.Do(req)
    if derr != nil { return "", derr }
    defer resp.Body.Close()
    if resp.StatusCode != http.StatusOK { return "", fmt.Errorf("blockhashbyheight %d: %s", height, resp.Status) }
    var body struct {
        Hash string `json:"blockhash"`
    }
    if err := json.NewDecoder(resp.Body).Decode(&body); err != nil { return "", err }
    return body.Hash, nil
}

// atTip is the run that has nothing to scan. It still rebuilds rich when that
// table is behind the balances it comes from — which is how a run interrupted
// after the state was committed finishes the job rather than starting over — or
// when it was built with a different -min.
func atTip(store *richStore, opt *options, at, tip int) error {
    var richAt, ok, err = store.height("rich")
    if err != nil { return err }
    var min, hadMin, merr = store.meta("min")
    if merr != nil { return merr }
    if ok && richAt == at && hadMin && min == strconv.FormatInt(opt.min, 10) {
        fmt.Printf("Balances are already at block %d (tip %d)\n", at, tip)
        return nil
    }
    fmt.Printf("Balances stand at block %d; rebuilding rich from them\n", at)
    return report(store, opt, at, time.Now())
}

// report rebuilds rich out of the stored balances and prints what the run left.
func report(store *richStore, opt *options, height int, started time.Time) error {
    var built = time.Now()
    var rows, err = store.materialize(int64(opt.min), height)
    if err != nil { return err }
    var _, sat, terr = store.totals()
    if terr != nil { return terr }
    fmt.Printf("Wrote %s addresses to rich in %s, holding %s sat (%.2f BTC), at block %d\n",
        group(int64(rows)), took(time.Since(built)), group(sat), float64(sat)/1e8, height)
    fmt.Printf("Done in %s\n", took(time.Since(started)))
    return nil
}

// chainFacts asks the node what it is before the scan starts: which chain, since
// the fixups above are mainnet's, and how much block data it holds, which is
// what the progress line measures against. A node that will not say is treated
// as mainnet — that is what a build against 127.0.0.1:8332 nearly always is, and
// the alternative, quietly skipping the fixups, would be wrong in the direction
// nobody would notice.
func chainFacts(ctx context.Context, baseURL string) (string, int64) {
    var req, err = http.NewRequestWithContext(ctx, http.MethodGet, baseURL+"/rest/chaininfo.json", nil)
    if err != nil { return "main", 0 }
    var resp, derr = http.DefaultClient.Do(req)
    if derr != nil { return "main", 0 }
    defer resp.Body.Close()
    var info struct {
        Chain string `json:"chain"`
        Size  int64  `json:"size_on_disk"`
    }
    if err := json.NewDecoder(resp.Body).Decode(&info); err != nil || info.Chain == "" {
        return "main", info.Size
    }
    return info.Chain, info.Size
}

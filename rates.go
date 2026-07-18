package main

import "encoding/json"
import "fmt"
import "io"
import "math"
import "net/http"
import "strconv"
import "strings"
import "time"

import "go.etcd.io/bbolt"

var ratesBucket = []byte("rates")

// a rate whose nearest stored sample is more than this far from the wanted time
// is treated as unavailable. It spans more than a day so the daily-granularity
// backfilled history (samples at 00:00 UTC) still matches a transaction at any
// time of day; recent live samples (every 5 min) match far more tightly anyway.
const rateTolerance = 36 * time.Hour

// backfilled history reaches back to bitcoin's earliest market price (2009); if
// the oldest stored sample is already this old we've backfilled before and skip
// re-downloading it. Live updates alone can't produce a sample this old on a
// newly deployed bot, so it cleanly distinguishes "backfilled" from "just started".
const rateHistoryHorizon = 3 * 365 * 24 * time.Hour

type rateRecord struct {
    Time   time.Time
    Source string
    USD    float64
}

func storeRate(r rateRecord) error {
    logDb("store rate $%.2f source=%s", r.USD, r.Source)
    var data, err = json.Marshal(r)
    if err != nil { return err }
    return db.Update(func(tx *bbolt.Tx) error {
        return tx.Bucket(ratesBucket).Put(itob(uint64(r.Time.Unix())), data)
    })
}

// storeRates writes many records in one transaction — used for the historical
// backfill, where per-record transactions would mean thousands of fsyncs.
func storeRates(records []rateRecord) error {
    logDb("store %d rates", len(records))
    return db.Update(func(tx *bbolt.Tx) error {
        var b = tx.Bucket(ratesBucket)
        for _, r := range records {
            var data, err = json.Marshal(r)
            if err != nil { return err }
            if err := b.Put(itob(uint64(r.Time.Unix())), data); err != nil { return err }
        }
        return nil
    })
}

// hasRateHistory reports whether the store already holds deep (backfilled)
// history — the oldest sample being years old, which live updates alone can't
// produce for a newly deployed bot.
func hasRateHistory() bool {
    if db == nil { return false }
    logDb("has history")
    var deep bool
    db.View(func(tx *bbolt.Tx) error {
        var k, v = tx.Bucket(ratesBucket).Cursor().First()
        if k == nil { return nil }
        var r rateRecord
        if json.Unmarshal(v, &r) == nil && time.Since(r.Time) > rateHistoryHorizon {
            deep = true
        }
        return nil
    })
    return deep
}

// lastRate returns the most recently stored rate, reading only from the
// database (never from an online API).
func lastRate() (rateRecord, bool) {
    if db == nil { return rateRecord{}, false }
    logDb("last rate")
    var r rateRecord
    var found bool
    db.View(func(tx *bbolt.Tx) error {
        var k, v = tx.Bucket(ratesBucket).Cursor().Last()
        if k != nil && json.Unmarshal(v, &r) == nil {
            found = true
        }
        return nil
    })
    return r, found
}

// rateAt returns the stored rate closest in time to t, or false if none is
// within rateTolerance (e.g. the transaction predates our rate history).
func rateAt(t time.Time) (rateRecord, bool) {
    if db == nil { return rateRecord{}, false }
    logDb("rate at %d", t.Unix())
    var target = t.Unix()
    var best rateRecord
    var found bool
    var bestDiff int64 = 1 << 62
    db.View(func(tx *bbolt.Tx) error {
        var c = tx.Bucket(ratesBucket).Cursor()
        var consider = func(k, v []byte) {
            if k == nil { return }
            var r rateRecord
            if json.Unmarshal(v, &r) != nil { return }
            var diff = r.Time.Unix() - target
            if diff < 0 { diff = -diff }
            if diff < bestDiff {
                bestDiff = diff
                best = r
                found = true
            }
        }
        var k, v = c.Seek(itob(uint64(target)))
        if k == nil {
            consider(c.Last())
        } else {
            consider(k, v)
            consider(c.Prev())
        }
        return nil
    })
    if !found || bestDiff > int64(rateTolerance.Seconds()) {
        return rateRecord{}, false
    }
    return best, true
}

// usd renders btc at the given USD/BTC rate. From $100 up the cents are dropped
// as noise ("$90,000"); below $100 they're kept, where they matter ("$5.99").
func usd(btc, rate float64) string {
    var value = btc * rate
    if value < 100 {
        return fmt.Sprintf("$%.2f", value)
    }
    var s = strconv.FormatInt(int64(math.Round(value)), 10)
    for i := len(s) - 3; i > 0; i -= 3 {
        s = s[:i] + "," + s[i:]
    }
    return "$" + s
}

// amountLine formats a BTC amount as "<sat> sats (≈ $usd)", appending the USD
// approximation only when a rate is available. current selects the rate: the
// last stored rate for mempool/notification amounts (no confirmed time), or the
// rate nearest the transaction's time otherwise — falling back to the last
// stored rate for confirmed times that predate our rate history, so an old
// transaction still shows its value at today's rate rather than nothing.
func amountLine(btc float64, at time.Time, current bool) string {
    var s = satoshi(btc) + " sats"
    var r rateRecord
    var ok bool
    if current {
        r, ok = lastRate()
    } else {
        r, ok = rateAt(at)
        if !ok {
            r, ok = lastRate()
        }
    }
    if ok {
        s += " (≈ " + usd(btc, r.USD) + ")"
    }
    return s
}

var rateHTTP = &http.Client{Timeout: 10 * time.Second}

type rateSource struct {
    name  string
    url   string
    parse func([]byte) (float64, error)
}

// rateSources are the free, no-auth BTC/USD price endpoints averaged on each
// update. Overridden in tests to point at local servers.
var rateSources = []rateSource{
    {"coingecko", "https://api.coingecko.com/api/v3/simple/price?ids=bitcoin&vs_currencies=usd", parseCoinGecko},
    {"coinbase", "https://api.coinbase.com/v2/prices/BTC-USD/spot", parseCoinbase},
    {"blockchain.info", "https://blockchain.info/ticker", parseBlockchainInfo},
}

// rateHistoryURL is blockchain.info's daily BTC/USD market-price chart — the
// whole history back to 2009 in one no-auth request. A package var so tests can
// point it at a local server.
var rateHistoryURL = "https://api.blockchain.info/charts/market-price?timespan=all&format=json&sampled=false"

func parseCoinGecko(body []byte) (float64, error) {
    var v struct {
        Bitcoin struct {
            USD float64 `json:"usd"`
        } `json:"bitcoin"`
    }
    if err := json.Unmarshal(body, &v); err != nil { return 0, err }
    return v.Bitcoin.USD, nil
}

func parseCoinbase(body []byte) (float64, error) {
    var v struct {
        Data struct {
            Amount string `json:"amount"`
        } `json:"data"`
    }
    if err := json.Unmarshal(body, &v); err != nil { return 0, err }
    return strconv.ParseFloat(v.Data.Amount, 64)
}

func parseBlockchainInfo(body []byte) (float64, error) {
    var v struct {
        USD struct {
            Last float64 `json:"last"`
        } `json:"USD"`
    }
    if err := json.Unmarshal(body, &v); err != nil { return 0, err }
    return v.USD.Last, nil
}

func fetchRate(s rateSource) (float64, error) {
    logNet("rates → GET %s", s.url)
    var resp, err = rateHTTP.Get(s.url)
    if err != nil { return 0, err }
    defer resp.Body.Close()
    var body, readErr = io.ReadAll(resp.Body)
    if readErr != nil { return 0, readErr }
    logNet("rates ← %s %s", s.name, body)
    if resp.StatusCode != http.StatusOK {
        return 0, fmt.Errorf("status %d", resp.StatusCode)
    }
    var rate, parseErr = s.parse(body)
    if parseErr != nil { return 0, parseErr }
    if rate <= 0 { return 0, fmt.Errorf("non-positive rate %v", rate) }
    return rate, nil
}

func parseRateHistory(body []byte) ([]rateRecord, error) {
    var v struct {
        Values []struct {
            X int64   `json:"x"`
            Y float64 `json:"y"`
        } `json:"values"`
    }
    if err := json.Unmarshal(body, &v); err != nil { return nil, err }
    var records []rateRecord
    for _, p := range v.Values {
        if p.Y <= 0 { continue }
        records = append(records, rateRecord{Time: time.Unix(p.X, 0), Source: "blockchain.info/history", USD: p.Y})
    }
    return records, nil
}

func fetchRateHistory() ([]rateRecord, error) {
    logNet("rates → GET %s", rateHistoryURL)
    var resp, err = rateHTTP.Get(rateHistoryURL)
    if err != nil { return nil, err }
    defer resp.Body.Close()
    var body, readErr = io.ReadAll(resp.Body)
    if readErr != nil { return nil, readErr }
    if resp.StatusCode != http.StatusOK {
        return nil, fmt.Errorf("status %d", resp.StatusCode)
    }
    var records, parseErr = parseRateHistory(body)
    if parseErr != nil { return nil, parseErr }
    logNet("rates ← history %d daily samples", len(records))
    return records, nil
}

// backfillRates loads the full daily BTC/USD history once, so /info on an old
// transaction can show a USD value from around its block time instead of
// nothing. Skipped when the store already holds backfilled history.
func backfillRates() {
    if hasRateHistory() { return }
    var records, err = fetchRateHistory()
    if err != nil {
        logWarn("rate history backfill: %v", err)
        return
    }
    if len(records) == 0 { return }
    if err := storeRates(records); err != nil {
        logErr("store rate history: %v", err)
        return
    }
    logInfo("backfilled %d historical BTC rates", len(records))
}

// updateRates fetches every source, averages the ones that succeeded, and stores
// a single averaged rate. If all sources fail nothing is stored.
func updateRates() {
    var sum float64
    var sources []string
    for _, s := range rateSources {
        var rate, err = fetchRate(s)
        if err != nil {
            logWarn("rate source %s: %v", s.name, err)
            continue
        }
        sum += rate
        sources = append(sources, s.name)
    }
    if len(sources) == 0 {
        logWarn("no BTC rate sources available")
        return
    }
    var avg = sum / float64(len(sources))
    if err := storeRate(rateRecord{Time: time.Now(), Source: strings.Join(sources, ","), USD: avg}); err != nil {
        logErr("store rate: %v", err)
        return
    }
    logInfo("updated BTC rate: $%.2f (avg of %s)", avg, strings.Join(sources, ", "))
}

// startRatesUpdater backfills the historical daily rates once, fetches an
// initial current rate, then refreshes every 5 minutes.
func startRatesUpdater() {
    go func() {
        backfillRates()
        updateRates()
        var t = time.NewTicker(5 * time.Minute)
        defer t.Stop()
        for range t.C {
            updateRates()
        }
    }()
}

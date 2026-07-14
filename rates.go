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
// is treated as unavailable — the bot simply wasn't recording rates then.
const rateTolerance = time.Hour

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

// usd renders btc at the given USD/BTC rate, e.g. "$58,234.12".
func usd(btc, rate float64) string {
    var cents = int64(math.Round(btc * rate * 100))
    var s = strconv.FormatInt(cents/100, 10)
    for i := len(s) - 3; i > 0; i -= 3 {
        s = s[:i] + "," + s[i:]
    }
    return fmt.Sprintf("$%s.%02d", s, cents%100)
}

// amountLine formats a BTC amount as "<sat> satoshi (≈ $usd)", appending the USD
// approximation only when a rate is available. current selects the rate: the
// last stored rate for mempool/notification amounts (no confirmed time), or the
// rate nearest the transaction's time otherwise.
func amountLine(btc float64, at time.Time, current bool) string {
    var s = satoshi(btc) + " satoshi"
    var r rateRecord
    var ok bool
    if current {
        r, ok = lastRate()
    } else {
        r, ok = rateAt(at)
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

// startRatesUpdater fetches an initial rate and then refreshes every 5 minutes.
func startRatesUpdater() {
    go func() {
        updateRates()
        var t = time.NewTicker(5 * time.Minute)
        defer t.Stop()
        for range t.C {
            updateRates()
        }
    }()
}

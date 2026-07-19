package rates

import "encoding/binary"
import "encoding/json"
import "fmt"
import "io"
import "net/http"
import "strconv"
import "strings"
import "time"

import "go.etcd.io/bbolt"
import "bitnsbot/logging"

var db *bbolt.DB
var bucket = []byte("rates")

// a rate whose nearest stored sample is more than this far from the wanted time
// is treated as unavailable. It spans more than a day so the daily-granularity
// backfilled history (samples at 00:00 UTC) still matches a transaction at any
// time of day; recent live samples (every 5 min) match far more tightly anyway.
const tolerance = 36 * time.Hour

// backfilled history reaches back to bitcoin's earliest market price (2009); if
// the oldest stored sample is already this old we've backfilled before and skip
// re-downloading it. Live updates alone can't produce a sample this old on a
// newly deployed bot, so it cleanly distinguishes "backfilled" from "just started".
const historyHorizon = 3 * 365 * 24 * time.Hour

// a stored sample: a USD/BTC price at a point in time. Kept unexported — callers
// only ever see the price (a float64).
type rateRecord struct {
    Time time.Time
    USD  float64
}

// Init stores the shared bbolt handle and ensures the rates bucket exists.
func Init(handle *bbolt.DB) error {
    db = handle
    return db.Update(func(tx *bbolt.Tx) error {
        var _, err = tx.CreateBucketIfNotExists(bucket)
        return err
    })
}

func itob(v uint64) []byte {
    var buf = make([]byte, 8)
    binary.BigEndian.PutUint64(buf, v)
    return buf
}

func store(r rateRecord) error {
    logging.Db("store rate $%.2f", r.USD)
    var data, err = json.Marshal(r)
    if err != nil { return err }
    return db.Update(func(tx *bbolt.Tx) error {
        return tx.Bucket(bucket).Put(itob(uint64(r.Time.Unix())), data)
    })
}

// storeMany writes many records in one transaction — used for the historical
// backfill, where per-record transactions would mean thousands of fsyncs.
func storeMany(records []rateRecord) error {
    logging.Db("store %d rates", len(records))
    return db.Update(func(tx *bbolt.Tx) error {
        var b = tx.Bucket(bucket)
        for _, r := range records {
            var data, err = json.Marshal(r)
            if err != nil { return err }
            if err := b.Put(itob(uint64(r.Time.Unix())), data); err != nil { return err }
        }
        return nil
    })
}

// Add stores a current BTC/USD rate (assumed USD), timestamped now.
func Add(usd float64) error {
    return store(rateRecord{Time: time.Now(), USD: usd})
}

// Last returns the most recently stored USD rate, reading only from the database
// (never from an online API); false if none is stored.
func Last() (float64, bool) {
    if db == nil { return 0, false }
    logging.Db("last rate")
    var usd float64
    var found bool
    db.View(func(tx *bbolt.Tx) error {
        var k, v = tx.Bucket(bucket).Cursor().Last()
        if k != nil {
            var r rateRecord
            if json.Unmarshal(v, &r) == nil { usd, found = r.USD, true }
        }
        return nil
    })
    return usd, found
}

// At returns the stored USD rate closest in time to t, or false if none is within
// tolerance (e.g. the transaction predates our rate history).
func At(t time.Time) (float64, bool) {
    if db == nil { return 0, false }
    logging.Db("rate at %d", t.Unix())
    var target = t.Unix()
    var best rateRecord
    var found bool
    var bestDiff int64 = 1 << 62
    db.View(func(tx *bbolt.Tx) error {
        var c = tx.Bucket(bucket).Cursor()
        var consider = func(k, v []byte) {
            if k == nil { return }
            var r rateRecord
            if json.Unmarshal(v, &r) != nil { return }
            var diff = r.Time.Unix() - target
            if diff < 0 { diff = -diff }
            if diff < bestDiff { bestDiff, best, found = diff, r, true }
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
    if !found || bestDiff > int64(tolerance.Seconds()) {
        return 0, false
    }
    return best.USD, true
}

// hasHistory reports whether the store already holds deep (backfilled) history —
// the oldest sample being years old, which live updates alone can't produce for
// a newly deployed bot.
func hasHistory() bool {
    if db == nil { return false }
    logging.Db("has history")
    var deep bool
    db.View(func(tx *bbolt.Tx) error {
        var k, v = tx.Bucket(bucket).Cursor().First()
        if k == nil { return nil }
        var r rateRecord
        if json.Unmarshal(v, &r) == nil && time.Since(r.Time) > historyHorizon {
            deep = true
        }
        return nil
    })
    return deep
}

var httpClient = &http.Client{Timeout: 10 * time.Second}

type source struct {
    name  string
    url   string
    parse func([]byte) (float64, error)
}

// sources are the free, no-auth BTC/USD price endpoints averaged on each update.
// Overridden in tests to point at local servers.
var sources = []source{
    {"coingecko", "https://api.coingecko.com/api/v3/simple/price?ids=bitcoin&vs_currencies=usd", parseCoinGecko},
    {"coinbase", "https://api.coinbase.com/v2/prices/BTC-USD/spot", parseCoinbase},
    {"blockchain.info", "https://blockchain.info/ticker", parseBlockchainInfo},
}

// historyURL is blockchain.info's daily BTC/USD market-price chart — the whole
// history back to 2009 in one no-auth request. A package var so tests can point
// it at a local server.
var historyURL = "https://api.blockchain.info/charts/market-price?timespan=all&format=json&sampled=false"

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

func fetch(s source) (float64, error) {
    logging.Net("rates → GET %s", s.url)
    var resp, err = httpClient.Get(s.url)
    if err != nil { return 0, err }
    defer resp.Body.Close()
    var body, readErr = io.ReadAll(resp.Body)
    if readErr != nil { return 0, readErr }
    logging.Net("rates ← %s %s", s.name, body)
    if resp.StatusCode != http.StatusOK {
        return 0, fmt.Errorf("status %d", resp.StatusCode)
    }
    var rate, parseErr = s.parse(body)
    if parseErr != nil { return 0, parseErr }
    if rate <= 0 { return 0, fmt.Errorf("non-positive rate %v", rate) }
    return rate, nil
}

func parseHistory(body []byte) ([]rateRecord, error) {
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
        records = append(records, rateRecord{Time: time.Unix(p.X, 0), USD: p.Y})
    }
    return records, nil
}

func fetchHistory() ([]rateRecord, error) {
    logging.Net("rates → GET %s", historyURL)
    var resp, err = httpClient.Get(historyURL)
    if err != nil { return nil, err }
    defer resp.Body.Close()
    var body, readErr = io.ReadAll(resp.Body)
    if readErr != nil { return nil, readErr }
    if resp.StatusCode != http.StatusOK {
        return nil, fmt.Errorf("status %d", resp.StatusCode)
    }
    var records, parseErr = parseHistory(body)
    if parseErr != nil { return nil, parseErr }
    logging.Net("rates ← history %d daily samples", len(records))
    return records, nil
}

// backfill loads the full daily BTC/USD history once, so /info on an old
// transaction can show a USD value from around its block time instead of
// nothing. Skipped when the store already holds backfilled history.
func backfill() {
    if hasHistory() { return }
    var records, err = fetchHistory()
    if err != nil {
        logging.Warn("rate history backfill: %v", err)
        return
    }
    if len(records) == 0 { return }
    if err := storeMany(records); err != nil {
        logging.Err("store rate history: %v", err)
        return
    }
    logging.Info("backfilled %d historical BTC rates", len(records))
}

// update fetches every source, averages the ones that succeeded, and stores a
// single averaged rate. If all sources fail nothing is stored.
func update() {
    var sum float64
    var names []string
    for _, s := range sources {
        var rate, err = fetch(s)
        if err != nil {
            logging.Warn("rate source %s: %v", s.name, err)
            continue
        }
        sum += rate
        names = append(names, s.name)
    }
    if len(names) == 0 {
        logging.Warn("no BTC rate sources available")
        return
    }
    var avg = sum / float64(len(names))
    if err := Add(avg); err != nil {
        logging.Err("store rate: %v", err)
        return
    }
    logging.Info("updated BTC rate: $%.2f (avg of %s)", avg, strings.Join(names, ", "))
}

// Start backfills the historical daily rates once, fetches an initial current
// rate, then refreshes every 5 minutes.
func Start() {
    go func() {
        backfill()
        update()
        var t = time.NewTicker(5 * time.Minute)
        defer t.Stop()
        for range t.C {
            update()
        }
    }()
}

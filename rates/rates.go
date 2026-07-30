package rates

import "encoding/binary"
import "encoding/json"
import "fmt"
import "io"
import "math"
import "net/http"
import "os"
import "strconv"
import "strings"
import "time"

import "go.etcd.io/bbolt"
import "bitnsbot/logging"

var db *bbolt.DB
var bucket = []byte("rates")
var marketBucket = []byte("market")

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

// a stored sample: a USD/BTC price at a point in time.  Time is not stored in the
// JSON value — the key already encodes the Unix timestamp.  Price is in cents to
// avoid floating-point drift in the database.
type rateRecord struct {
    Time  time.Time `json:"-"`
    Cents int64     `json:"cents"`
}

// Init stores the shared bbolt handle and ensures the rates bucket exists.
func Init(handle *bbolt.DB) error {
    db = handle
    return db.Update(func(tx *bbolt.Tx) error {
        for _, name := range [][]byte{bucket, marketBucket} {
            if _, err := tx.CreateBucketIfNotExists(name); err != nil { return err }
        }
        return nil
    })
}

func itob(v uint64) []byte {
    var buf = make([]byte, 8)
    binary.BigEndian.PutUint64(buf, v)
    return buf
}

func store(r rateRecord) error {
    logging.Db("store rate $%.2f", float64(r.Cents)/100)
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
    return store(rateRecord{Time: time.Now(), Cents: int64(math.Round(usd * 100))})
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
            if err := json.Unmarshal(v, &r); err != nil {
                logging.Err("error json unmarshal %v: %v", v, err)
                return err
            }
            usd, found = float64(r.Cents)/100, true
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
            if err := json.Unmarshal(v, &r); err != nil {
                logging.Err("error json unmarshal %v: %v", v, err)
                return
            }
            var diff = int64(binary.BigEndian.Uint64(k)) - target
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
    return float64(best.Cents)/100, true
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
        if json.Unmarshal(v, &r) == nil && time.Since(time.Unix(int64(binary.BigEndian.Uint64(k)), 0)) > historyHorizon {
            deep = true
        }
        return nil
    })
    logging.Db("has history %v", deep)
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

// historyFile is an optional local copy of the history endpoint's response.
// When set, backfill() loads from this file instead of the network on first run.
var historyFile string

// SetHistoryFile is called by main to point the backfill at a local file.
func SetHistoryFile(path string) { historyFile = path }

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
        records = append(records, rateRecord{Time: time.Unix(p.X, 0), Cents: int64(math.Round(p.Y * 100))})
    }
    return records, nil
}

func fetchHistory() ([]rateRecord, error) {
    for attempt := 1; ; attempt++ {
        logging.Net("rates → GET %s (attempt %d/3)", historyURL, attempt)
        var resp, err = httpClient.Get(historyURL)
        if err == nil {
            var body, readErr = io.ReadAll(resp.Body)
            resp.Body.Close()
            if readErr == nil && resp.StatusCode == http.StatusOK {
                var records, parseErr = parseHistory(body)
                if parseErr == nil {
                    logging.Net("rates ← history %d daily samples", len(records))
                    return records, nil
                }
                logging.Warn("rate history backfill: parse (attempt %d/3): %v", attempt, parseErr)
            } else if readErr != nil {
                logging.Warn("rate history backfill: read (attempt %d/3): %v", attempt, readErr)
            } else {
                logging.Warn("rate history backfill: status %d (attempt %d/3)", resp.StatusCode, attempt)
            }
        } else {
            logging.Warn("rate history backfill: fetch (attempt %d/3): %v", attempt, err)
        }
        if attempt >= 3 {
            return nil, fmt.Errorf("rate history backfill: all 3 attempts failed")
        }
        time.Sleep(10 * time.Second)
    }
}

// loadHistoryFromFile reads the JSON file at historyFile and returns the parsed
// records. The file must have the same shape as the blockchain.info history API.
func loadHistoryFromFile() ([]rateRecord, error) {
    var data, err = os.ReadFile(historyFile)
    if err != nil { return nil, err }
    return parseHistory(data)
}

// backfill loads the full daily BTC/USD history once, so /info on an old
// transaction can show a USD value from around its block time instead of
// nothing. Skipped when the store already holds backfilled history.  When
// -history-file is set it reads from the local file first; otherwise it
// fetches over the network.
func backfill() {
    if hasHistory() { return }
    var records []rateRecord
    var err error
    if historyFile != "" {
        records, err = loadHistoryFromFile()
        if err != nil {
            logging.Warn("rate history backfill: file %s: %v — falling back to network", historyFile, err)
        }
    }
    if len(records) == 0 {
        records, err = fetchHistory()
        if err != nil {
            logging.Warn("rate history backfill: %v", err)
            return
        }
    }
    if len(records) == 0 { return }
    if err := storeMany(records); err != nil {
        logging.Err("store rate history: %v", err)
        return
    }
    var src = "network"
    if historyFile != "" { src = "file " + historyFile }
    logging.Info("backfilled %d historical BTC rates from %s", len(records), src)
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
    updateMarket()
}

// updateMarket refreshes the stored capitalisation and volume. It runs on the
// price updater's tick rather than per /market command so the command reads the
// database like every other one, and so a rate-limited or down API costs a stale
// figure rather than a failed reply.
func updateMarket() {
    var m, ok = Snapshot()
    if !ok { return }
    if err := storeMarket(m); err != nil {
        logging.Err("store market: %v", err)
        return
    }
    logging.Info("updated market: cap $%.0f, 24h volume $%.0f", m.MarketCap, m.Volume24h)
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

// marketURL is CoinGecko's one-call snapshot of price, market capitalisation and
// 24h volume — free and no-auth, like the price sources. A package var so tests
// point it at a local server.
var marketURL = "https://api.coingecko.com/api/v3/simple/price?ids=bitcoin&vs_currencies=usd&include_market_cap=true&include_24hr_vol=true"

// Market is the snapshot behind /market. Price also comes from the stored rate
// history (Last), but capitalisation and volume have no other source, so they
// are fetched live and reported as unavailable when the fetch fails rather than
// failing the whole command.
type Market struct {
    Price     float64
    MarketCap float64
    Volume24h float64
}

func parseMarket(body []byte) (Market, error) {
    var v struct {
        Bitcoin struct {
            USD       float64 `json:"usd"`
            MarketCap float64 `json:"usd_market_cap"`
            Volume    float64 `json:"usd_24h_vol"`
        } `json:"bitcoin"`
    }
    if err := json.Unmarshal(body, &v); err != nil { return Market{}, err }
    if v.Bitcoin.USD <= 0 { return Market{}, fmt.Errorf("no price in response") }
    return Market{Price: v.Bitcoin.USD, MarketCap: v.Bitcoin.MarketCap, Volume24h: v.Bitcoin.Volume}, nil
}

// Snapshot fetches the current market figures. Unlike the price updater it is
// called per command rather than on a timer, so it makes no attempt to average
// across sources — only CoinGecko publishes capitalisation and volume for free.
func Snapshot() (Market, bool) {
    logging.Net("rates → GET %s", marketURL)
    var resp, err = httpClient.Get(marketURL)
    if err != nil {
        logging.Warn("market snapshot: %v", err)
        return Market{}, false
    }
    defer resp.Body.Close()
    var body, readErr = io.ReadAll(resp.Body)
    if readErr != nil {
        logging.Warn("market snapshot: %v", readErr)
        return Market{}, false
    }
    logging.Net("rates ← market %s", body)
    if resp.StatusCode != http.StatusOK {
        logging.Warn("market snapshot: status %d", resp.StatusCode)
        return Market{}, false
    }
    var m, parseErr = parseMarket(body)
    if parseErr != nil {
        logging.Warn("market snapshot: %v", parseErr)
        return Market{}, false
    }
    return m, true
}

// marketRecord is one stored market snapshot, keyed by Unix timestamp like the
// rate records so the newest is the last key in the bucket.
type marketRecord struct {
    Timestamp int64   `json:"timestamp"`
    Price     float64 `json:"price"`
    MarketCap float64 `json:"market_cap"`
    Volume24h float64 `json:"volume_24h"`
}

func storeMarket(m Market) error {
    if db == nil { return nil }
    var rec = marketRecord{Timestamp: time.Now().Unix(), Price: m.Price, MarketCap: m.MarketCap, Volume24h: m.Volume24h}
    logging.Db("store market cap %.0f volume %.0f", m.MarketCap, m.Volume24h)
    var data, err = json.Marshal(rec)
    if err != nil {
        logging.Err("store market json marshal: %v", err)
        return err
    }
    return db.Update(func(tx *bbolt.Tx) error {
        return tx.Bucket(marketBucket).Put(itob(uint64(rec.Timestamp)), data)
    })
}

// LastMarket returns the most recently stored snapshot. /market reads this
// rather than fetching, so the command never depends on a third-party API being
// reachable at the moment someone types it.
func LastMarket() (Market, bool) {
    if db == nil { return Market{}, false }
    logging.Db("last market")
    var m Market
    var found bool
    db.View(func(tx *bbolt.Tx) error {
        var k, v = tx.Bucket(marketBucket).Cursor().Last()
        if k == nil { return nil }
        var rec marketRecord
        if err := json.Unmarshal(v, &rec); err != nil {
            logging.Err("last market json unmarshal %v: %v", v, err)
            return nil
        }
        m, found = Market{Price: rec.Price, MarketCap: rec.MarketCap, Volume24h: rec.Volume24h}, true
        return nil
    })
    return m, found
}

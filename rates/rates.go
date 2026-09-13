package rates

import "encoding/json"
import "fmt"
import "io"
import "math"
import "net/http"
import "os"
import "strconv"
import "strings"
import "time"

import "database/sql"
import "bitnsbot/logging"

var db *sql.DB

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

// a stored sample: a USD/BTC price at a point in time. The timestamp is the
// table's primary key, and the price is cents — an integer, to keep a price off
// floating point the way every amount in the bot is kept off it.
type rate struct {
    Time  time.Time
    Cents int64
}

// Init stores the shared handle. The two tables are created by openDB, from the
// schema tools/tosqlite defines.
func Init(handle *sql.DB) error {
    db = handle
    return nil
}

func store(r rate) error {
    logging.Db("store rate $%.2f", float64(r.Cents)/100)
    if db == nil { return nil }
    var _, err = db.Exec("insert into rates (ts, cents) values (?, ?) "+
        "on conflict(ts) do update set cents = excluded.cents", r.Time.Unix(), r.Cents)
    return err
}

// storeRates writes many records in one transaction — used for the historical
// backfill, where per-record transactions would mean thousands of fsyncs.
// storeRates writes the whole daily history in one transaction — thousands of
// rows, where a statement apiece would be a commit apiece.
func storeRates(rates []rate) error {
    logging.Db("store %d rates", len(rates))
    if db == nil { return nil }
    var tx, err = db.Begin()
    if err != nil { return err }
    var stmt, perr = tx.Prepare("insert into rates (ts, cents) values (?, ?) " +
        "on conflict(ts) do update set cents = excluded.cents")
    if perr != nil {
        tx.Rollback()
        return perr
    }
    for _, r := range rates {
        if _, err := stmt.Exec(r.Time.Unix(), r.Cents); err != nil {
            stmt.Close()
            tx.Rollback()
            return err
        }
    }
    if err := stmt.Close(); err != nil {
        tx.Rollback()
        return err
    }
    return tx.Commit()
}

// Add stores a current BTC/USD rate (assumed USD), timestamped now.
func Add(usd float64) error {
    return store(rate{Time: time.Now(), Cents: int64(math.Round(usd * 100))})
}

// Last returns the most recently stored USD rate, reading only from the database
// (never from an online API); false if none is stored.
func Last() (float64, bool) {
    if db == nil { return 0, false }
    logging.Db("last rate")
    var cents int64
    // the newest sample is the largest primary key, which is the index's own end
    if db.QueryRow("select cents from rates order by ts desc limit 1").Scan(&cents) != nil {
        return 0, false
    }
    return float64(cents) / 100, true
}

// At returns the stored USD rate closest in time to t, or false if none is within
// tolerance (e.g. the transaction predates our rate history).
func At(t time.Time) (float64, bool) {
    if db == nil { return 0, false }
    logging.Db("rate at %d", t.Unix())
    var target = t.Unix()
    // The nearest sample is one of two rows: the last at or before the wanted
    // time, and the first after it. Both are one index seek, where scanning for
    // the smallest difference would read the whole series.
    var best int64
    var bestDiff int64 = 1 << 62
    var found bool
    for _, q := range []string{
        "select ts, cents from rates where ts <= ? order by ts desc limit 1",
        "select ts, cents from rates where ts > ? order by ts limit 1",
    } {
        var ts, cents int64
        if db.QueryRow(q, target).Scan(&ts, &cents) != nil { continue }
        var diff = ts - target
        if diff < 0 { diff = -diff }
        if diff < bestDiff { bestDiff, best, found = diff, cents, true }
    }
    if !found || bestDiff > int64(tolerance.Seconds()) { return 0, false }
    return float64(best) / 100, true
}

// hasHistory reports whether the store already holds deep (backfilled) history —
// the oldest sample being years old, which live updates alone can't produce for
// a newly deployed bot.
func hasHistory() bool {
    if db == nil { return false }
    logging.Db("has history")
    var oldest int64
    if db.QueryRow("select ts from rates order by ts limit 1").Scan(&oldest) != nil { return false }
    var deep = time.Since(time.Unix(oldest, 0)) > historyHorizon
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

func parseHistory(body []byte) ([]rate, error) {
    var v struct {
        Values []struct {
            X int64   `json:"x"`
            Y float64 `json:"y"`
        } `json:"values"`
    }
    if err := json.Unmarshal(body, &v); err != nil { return nil, err }
    var records []rate
    for _, p := range v.Values {
        if p.Y <= 0 { continue }
        records = append(records, rate{Time: time.Unix(p.X, 0), Cents: int64(math.Round(p.Y * 100))})
    }
    return records, nil
}

func fetchHistory() ([]rate, error) {
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
                logging.Warn("rate history: parse (attempt %d/3): %v", attempt, parseErr)
            } else if readErr != nil {
                logging.Warn("rate history: read (attempt %d/3): %v", attempt, readErr)
            } else {
                logging.Warn("rate history: status %d (attempt %d/3)", resp.StatusCode, attempt)
            }
        } else {
            logging.Warn("rate history: fetch (attempt %d/3): %v", attempt, err)
        }
        if attempt >= 3 {
            return nil, fmt.Errorf("rate history: all 3 attempts failed")
        }
        time.Sleep(10 * time.Second)
    }
}

// loadHistoryFromFile reads the JSON file at historyFile and returns the parsed
// records. The file must have the same shape as the blockchain.info history API.
func loadHistoryFromFile() ([]rate, error) {
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
    var records []rate
    var err error
    if historyFile != "" {
        records, err = loadHistoryFromFile()
        if err != nil {
            logging.Warn("rate history: file %s: %v — falling back to network", historyFile, err)
        }
    }
    if len(records) == 0 {
        records, err = fetchHistory()
        if err != nil {
            logging.Warn("rate history: %v", err)
            return
        }
    }
    if len(records) == 0 { return }
    if err := storeRates(records); err != nil {
        logging.Err("rates history: %v", err)
        return
    }
    var src = "network"
    if historyFile != "" { src = "file " + historyFile }
    logging.Info("rates: stored %d historical BTC rates from %s", len(records), src)
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
    logging.Info("rates: updated $%.2f (avg of %s)", avg, strings.Join(names, ", "))
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
        logging.Err("rates: market: %v", err)
        return
    }
    logging.Info("rates: updated market cap $%.0f, 24h volume $%.0f", m.MarketCap, m.Volume24h)
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
    logging.Net("rates: GET %s", marketURL)
    var resp, err = httpClient.Get(marketURL)
    if err != nil {
        logging.Warn("rates: market: %v", err)
        return Market{}, false
    }
    defer resp.Body.Close()
    var body, readErr = io.ReadAll(resp.Body)
    if readErr != nil {
        logging.Warn("rates: market: %v", readErr)
        return Market{}, false
    }
    logging.Net("rates: market %s", body)
    if resp.StatusCode != http.StatusOK {
        logging.Warn("rates: market: status %d", resp.StatusCode)
        return Market{}, false
    }
    var m, parseErr = parseMarket(body)
    if parseErr != nil {
        logging.Warn("rates: market: %v", parseErr)
        return Market{}, false
    }
    return m, true
}

// market is one stored market snapshot, keyed by Unix timestamp like the
// rate records so the newest is the last key in the bucket.
// cents is how money is stored, the unit the rates table already keeps a price
// in: a market capitalisation in trillions has no business being a float64 that
// drifts. tools/tosqlite converts the same way, which is what keeps a migrated
// database and one the bot wrote indistinguishable.
func cents(usd float64) int64 { return int64(math.Round(usd * 100)) }

func storeMarket(m Market) error {
    if db == nil { return nil }
    logging.Db("store market cap %.0f volume %.0f", m.MarketCap, m.Volume24h)
    var _, err = db.Exec(`insert into market (ts, price, cap, volume24h) values (?, ?, ?, ?)
        on conflict(ts) do update set price = excluded.price, cap = excluded.cap, volume24h = excluded.volume24h`,
        time.Now().Unix(), cents(m.Price), cents(m.MarketCap), cents(m.Volume24h))
    if err != nil { logging.Err("store market: %v", err) }
    return err
}

// LastMarket returns the most recently stored snapshot. /market reads this
// rather than fetching, so the command never depends on a third-party API being
// reachable at the moment someone types it.
func LastMarket() (Market, bool) {
    if db == nil { return Market{}, false }
    logging.Db("rates: market")
    var price, cap_, volume int64
    if db.QueryRow("select price, cap, volume24h from market order by ts desc limit 1").Scan(
        &price, &cap_, &volume) != nil {
        return Market{}, false
    }
    return Market{Price: float64(price) / 100, MarketCap: float64(cap_) / 100, Volume24h: float64(volume) / 100}, true
}

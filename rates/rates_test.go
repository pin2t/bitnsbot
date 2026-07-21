package rates

import "fmt"
import "net/http"
import "net/http/httptest"
import "path/filepath"
import "testing"
import "time"

import "go.etcd.io/bbolt"

func openTestDB(t *testing.T) {
    var d, err = bbolt.Open(filepath.Join(t.TempDir(), "rates.db"), 0600, nil)
    if err != nil { t.Fatalf("open: %v", err) }
    if err := Init(d); err != nil { t.Fatalf("init: %v", err) }
    t.Cleanup(func() { d.Close(); db = nil })
}

func TestRateParsers(t *testing.T) {
    if r, err := parseCoinGecko([]byte(`{"bitcoin":{"usd":58234.5}}`)); err != nil || r != 58234.5 {
        t.Fatalf("coingecko: %v %v", r, err)
    }
    if r, err := parseCoinbase([]byte(`{"data":{"amount":"58234.12","currency":"USD"}}`)); err != nil || r != 58234.12 {
        t.Fatalf("coinbase: %v %v", r, err)
    }
    if r, err := parseBlockchainInfo([]byte(`{"USD":{"last":58234.1,"symbol":"$"}}`)); err != nil || r != 58234.1 {
        t.Fatalf("blockchain.info: %v %v", r, err)
    }
}

func TestParseRateHistory(t *testing.T) {
    var body = []byte(`{"status":"ok","values":[{"x":1230940800,"y":0.0},{"x":1420070400,"y":320.19},{"x":1783987200,"y":62242.32}]}`)
    var records, err = parseHistory(body)
    if err != nil {
        t.Fatalf("parse: %v", err)
    }
    if len(records) != 2 { // the y=0 early sample is skipped
        t.Fatalf("expected 2 non-zero records, got %d", len(records))
    }
    if records[0].Time.Unix() != 1420070400 || records[0].USD != 320.19 {
        t.Fatalf("first record = %+v", records[0])
    }
}

func TestBackfillRates(t *testing.T) {
    openTestDB(t)
    var old = time.Now().Add(-5 * 365 * 24 * time.Hour).Unix()
    var hits int
    var srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
        hits++
        fmt.Fprintf(w, `{"values":[{"x":%d,"y":250.5},{"x":0,"y":0}]}`, old)
    }))
    defer srv.Close()
    var saved = historyURL
    defer func() { historyURL = saved }()
    historyURL = srv.URL
    backfill()
    // the backfilled old sample is retrievable for a tx around that time
    if r, ok := At(time.Unix(old, 0)); !ok || r != 250.5 {
        t.Fatalf("expected backfilled rate, got %v %v", r, ok)
    }
    // a second call is a no-op: deep history now exists, so no re-download
    backfill()
    if hits != 1 {
        t.Fatalf("expected exactly 1 history fetch, got %d", hits)
    }
}

func TestRateStorage(t *testing.T) {
    openTestDB(t)
    var base = time.Now().Truncate(time.Second)
    store(rateRecord{Time: base.Add(-10 * time.Minute), USD: 100})
    store(rateRecord{Time: base.Add(-5 * time.Minute), USD: 200})
    store(rateRecord{Time: base, USD: 300})
    // Last returns the newest
    if r, ok := Last(); !ok || r != 300 {
        t.Fatalf("Last = %v %v", r, ok)
    }
    // exact and nearest-time lookups
    if r, ok := At(base.Add(-5 * time.Minute)); !ok || r != 200 {
        t.Fatalf("At(-5m) = %v %v", r, ok)
    }
    if r, ok := At(base.Add(-6 * time.Minute)); !ok || r != 200 { // 1m from 200 vs 4m from 100
        t.Fatalf("At(-6m) = %v %v", r, ok)
    }
    if r, ok := At(base.Add(30 * time.Minute)); !ok || r != 300 { // after all, within tolerance
        t.Fatalf("At(+30m) = %v %v", r, ok)
    }
    // far outside the recorded window → unavailable
    if _, ok := At(base.Add(-72 * time.Hour)); ok {
        t.Fatalf("expected no rate days before earliest sample")
    }
}

func TestUpdateRatesAverages(t *testing.T) {
    openTestDB(t)
    var body = func(s string) *httptest.Server {
        return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
            w.Write([]byte(s))
        }))
    }
    var s1 = body(`{"bitcoin":{"usd":60000}}`)
    var s2 = body(`{"data":{"amount":"62000.00"}}`)
    var s3 = body(`{"USD":{"last":58000}}`)
    var bad = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
        w.WriteHeader(http.StatusInternalServerError)
    }))
    defer s1.Close()
    defer s2.Close()
    defer s3.Close()
    defer bad.Close()
    var saved = sources
    defer func() { sources = saved }()
    sources = []source{
        {"coingecko", s1.URL, parseCoinGecko},
        {"coinbase", s2.URL, parseCoinbase},
        {"blockchain.info", s3.URL, parseBlockchainInfo},
    }
    update()
    if r, ok := Last(); !ok || r != (60000.0+62000.0+58000.0)/3 {
        t.Fatalf("expected averaged rate, got %v %v", r, ok)
    }
    // a failing source is excluded from the average
    sources = []source{
        {"coingecko", s1.URL, parseCoinGecko},             // 60000
        {"blockchain.info", bad.URL, parseBlockchainInfo}, // fails (500)
    }
    update()
    if r, _ := Last(); r != 60000 {
        t.Fatalf("expected only coingecko averaged, got %v", r)
    }
}

// The market snapshot is the one figure set with no local source: capitalisation
// and volume can only be fetched. Parsed from CoinGecko's real response shape.
func TestParseMarket(t *testing.T) {
    var body = []byte(`{"bitcoin":{"usd":66202,"usd_market_cap":1327983664334.4749,"usd_24h_vol":31914279096.507484}}`)
    var m, err = parseMarket(body)
    if err != nil {
        t.Fatalf("parseMarket: %v", err)
    }
    if m.Price != 66202 {
        t.Errorf("price = %v", m.Price)
    }
    if m.MarketCap != 1327983664334.4749 {
        t.Errorf("market cap = %v", m.MarketCap)
    }
    if m.Volume24h != 31914279096.507484 {
        t.Errorf("volume = %v", m.Volume24h)
    }
    // a response without a price is not a usable snapshot
    if _, err := parseMarket([]byte(`{"bitcoin":{}}`)); err == nil {
        t.Error("expected an error for a response carrying no price")
    }
}

// A failed fetch must report not-ok rather than a zeroed snapshot, so /market
// can say "unavailable" instead of quoting a $0 market cap.
func TestSnapshotDegrades(t *testing.T) {
    var saved = marketURL
    t.Cleanup(func() { marketURL = saved })
    var srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
        http.Error(w, "rate limited", http.StatusTooManyRequests)
    }))
    defer srv.Close()
    marketURL = srv.URL
    if _, ok := Snapshot(); ok {
        t.Fatal("expected not-ok when the market API refuses")
    }
    var good = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
        w.Write([]byte(`{"bitcoin":{"usd":66202,"usd_market_cap":1.3e12,"usd_24h_vol":3.1e10}}`))
    }))
    defer good.Close()
    marketURL = good.URL
    var m, ok = Snapshot()
    if !ok || m.Price != 66202 {
        t.Fatalf("snapshot = %+v ok=%v", m, ok)
    }
}

// The snapshot is stored on the updater's tick and read back by /market, so the
// round trip through bbolt is what matters — not the fetch.
func TestMarketStorage(t *testing.T) {
    openTestDB(t)
    if _, ok := LastMarket(); ok {
        t.Fatal("expected no snapshot before one is stored")
    }
    if err := storeMarket(Market{Price: 66202, MarketCap: 1.3e12, Volume24h: 3.1e10}); err != nil {
        t.Fatalf("storeMarket: %v", err)
    }
    var m, ok = LastMarket()
    if !ok {
        t.Fatal("stored snapshot did not come back")
    }
    if m.Price != 66202 || m.MarketCap != 1.3e12 || m.Volume24h != 3.1e10 {
        t.Fatalf("round trip = %+v", m)
    }
    // a later snapshot supersedes the earlier one
    time.Sleep(1100 * time.Millisecond) // records are keyed by unix second
    if err := storeMarket(Market{Price: 70000, MarketCap: 1.4e12, Volume24h: 4e10}); err != nil {
        t.Fatalf("storeMarket: %v", err)
    }
    m, _ = LastMarket()
    if m.Price != 70000 {
        t.Fatalf("LastMarket returned %v, want the newest snapshot", m.Price)
    }
}

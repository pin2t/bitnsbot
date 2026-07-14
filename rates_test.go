package main

import "fmt"
import "net/http"
import "net/http/httptest"
import "path/filepath"
import "strings"
import "testing"
import "time"

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

func TestUSDFormat(t *testing.T) {
    if got := usd(1.5, 60000); got != "$90,000" { // ≥ $100 → whole dollars, grouped
        t.Fatalf("1.5 BTC @ 60000 = %q", got)
    }
    if got := usd(10, 12345.67); got != "$123,457" { // 123456.70 rounds up
        t.Fatalf("grouping = %q", got)
    }
    if got := usd(1, 100); got != "$100" { // exactly $100 → no cents
        t.Fatalf("boundary = %q", got)
    }
    if got := usd(0.001, 58234.12); got != "$58.23" { // < $100 → keep cents
        t.Fatalf("0.001 BTC @ 58234.12 = %q", got)
    }
    if got := usd(0.5, 199); got != "$99.50" { // just under $100 → keep cents
        t.Fatalf("sub-100 = %q", got)
    }
}

func TestParseRateHistory(t *testing.T) {
    var body = []byte(`{"status":"ok","values":[{"x":1230940800,"y":0.0},{"x":1420070400,"y":320.19},{"x":1783987200,"y":62242.32}]}`)
    var records, err = parseRateHistory(body)
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
    if err := openDB(filepath.Join(t.TempDir(), "watches.db")); err != nil {
        t.Fatalf("openDB: %v", err)
    }
    defer closeDB()
    var old = time.Now().Add(-5 * 365 * 24 * time.Hour).Unix()
    var hits int
    var srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
        hits++
        fmt.Fprintf(w, `{"values":[{"x":%d,"y":250.5},{"x":0,"y":0}]}`, old)
    }))
    defer srv.Close()
    var saved = rateHistoryURL
    defer func() { rateHistoryURL = saved }()
    rateHistoryURL = srv.URL
    backfillRates()
    // the backfilled old sample is retrievable for a tx around that time
    if r, ok := rateAt(time.Unix(old, 0)); !ok || r.USD != 250.5 {
        t.Fatalf("expected backfilled rate, got %v %v", r, ok)
    }
    // a second call is a no-op: deep history now exists, so no re-download
    backfillRates()
    if hits != 1 {
        t.Fatalf("expected exactly 1 history fetch, got %d", hits)
    }
}

func TestRateStorage(t *testing.T) {
    if err := openDB(filepath.Join(t.TempDir(), "watches.db")); err != nil {
        t.Fatalf("openDB: %v", err)
    }
    defer closeDB()
    var base = time.Now().Truncate(time.Second)
    storeRate(rateRecord{Time: base.Add(-10 * time.Minute), Source: "a", USD: 100})
    storeRate(rateRecord{Time: base.Add(-5 * time.Minute), Source: "b", USD: 200})
    storeRate(rateRecord{Time: base, Source: "c", USD: 300})
    // lastRate returns the newest
    if r, ok := lastRate(); !ok || r.USD != 300 {
        t.Fatalf("lastRate = %v %v", r, ok)
    }
    // exact and nearest-time lookups
    if r, ok := rateAt(base.Add(-5 * time.Minute)); !ok || r.USD != 200 {
        t.Fatalf("rateAt(-5m) = %v %v", r, ok)
    }
    if r, ok := rateAt(base.Add(-6 * time.Minute)); !ok || r.USD != 200 { // 1m from 200 vs 4m from 100
        t.Fatalf("rateAt(-6m) = %v %v", r, ok)
    }
    if r, ok := rateAt(base.Add(30 * time.Minute)); !ok || r.USD != 300 { // after all, within tolerance
        t.Fatalf("rateAt(+30m) = %v %v", r, ok)
    }
    // far outside the recorded window → unavailable
    if _, ok := rateAt(base.Add(-72 * time.Hour)); ok {
        t.Fatalf("expected no rate days before earliest sample")
    }
}

func TestUpdateRatesAverages(t *testing.T) {
    if err := openDB(filepath.Join(t.TempDir(), "watches.db")); err != nil {
        t.Fatalf("openDB: %v", err)
    }
    defer closeDB()
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
    var saved = rateSources
    defer func() { rateSources = saved }()
    rateSources = []rateSource{
        {"coingecko", s1.URL, parseCoinGecko},
        {"coinbase", s2.URL, parseCoinbase},
        {"blockchain.info", s3.URL, parseBlockchainInfo},
    }
    updateRates()
    var r, ok = lastRate()
    if !ok {
        t.Fatal("expected a stored rate")
    }
    if want := (60000.0 + 62000.0 + 58000.0) / 3; r.USD != want {
        t.Fatalf("expected avg %v, got %v", want, r.USD)
    }
    if r.Source != "coingecko,coinbase,blockchain.info" {
        t.Fatalf("unexpected source: %q", r.Source)
    }
    // a failing source is excluded from the average
    rateSources = []rateSource{
        {"coingecko", s1.URL, parseCoinGecko},   // 60000
        {"blockchain.info", bad.URL, parseBlockchainInfo}, // fails (500)
    }
    updateRates()
    if r, _ = lastRate(); r.USD != 60000 || r.Source != "coingecko" {
        t.Fatalf("expected only coingecko averaged, got %v / %q", r.USD, r.Source)
    }
}

func TestAmountLineUSD(t *testing.T) {
    if err := openDB(filepath.Join(t.TempDir(), "watches.db")); err != nil {
        t.Fatalf("openDB: %v", err)
    }
    defer closeDB()
    // no rate stored yet → satoshi only, no USD
    if s := amountLine(1.5, time.Time{}, true); strings.Contains(s, "$") {
        t.Fatalf("expected no USD without a rate: %q", s)
    }
    storeRate(rateRecord{Time: time.Now(), Source: "x", USD: 60000})
    var s = amountLine(1.5, time.Time{}, true)
    if !strings.Contains(s, "150 000 000 satoshi") || !strings.Contains(s, "$90,000") {
        t.Fatalf("amountLine = %q", s)
    }
}

func TestAmountLineFallback(t *testing.T) {
    if err := openDB(filepath.Join(t.TempDir(), "watches.db")); err != nil {
        t.Fatalf("openDB: %v", err)
    }
    defer closeDB()
    storeRate(rateRecord{Time: time.Now(), Source: "x", USD: 60000})
    // a confirmed tx whose block time predates our rate history (older than
    // rateTolerance) still shows USD, falling back to the latest known rate.
    var s = amountLine(1.5, time.Now().Add(-5*24*time.Hour), false)
    if !strings.Contains(s, "$90,000") {
        t.Fatalf("expected USD via fallback for an old confirmed tx: %q", s)
    }
}

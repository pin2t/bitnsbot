package main

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
    if got := usd(1.5, 60000); got != "$90,000.00" {
        t.Fatalf("1.5 BTC @ 60000 = %q", got)
    }
    if got := usd(0.001, 58234.12); got != "$58.23" {
        t.Fatalf("0.001 BTC @ 58234.12 = %q", got)
    }
    if got := usd(10, 12345.67); got != "$123,456.70" { // 10 * 12345.67 = 123456.70
        t.Fatalf("grouping = %q", got)
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
    if _, ok := rateAt(base.Add(-3 * time.Hour)); ok {
        t.Fatalf("expected no rate 3h before earliest sample")
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
    if !strings.Contains(s, "150 000 000 satoshi") || !strings.Contains(s, "$90,000.00") {
        t.Fatalf("amountLine = %q", s)
    }
}

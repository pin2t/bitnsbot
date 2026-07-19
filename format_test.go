package main

import "path/filepath"
import "strings"
import "testing"
import "time"

import "bitnsbot/rates"

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

func TestAmountLineUSD(t *testing.T) {
    if err := openDB(filepath.Join(t.TempDir(), "watches.db")); err != nil {
        t.Fatalf("openDB: %v", err)
    }
    defer closeDB()
    if err := rates.Init(db); err != nil {
        t.Fatalf("rates.Init: %v", err)
    }
    // no rate stored yet → sats only, no USD
    if s := amountLine(1.5, time.Time{}, true); strings.Contains(s, "$") {
        t.Fatalf("expected no USD without a rate: %q", s)
    }
    rates.Add(60000)
    var s = amountLine(1.5, time.Time{}, true)
    if !strings.Contains(s, "150 000 000 sats") || !strings.Contains(s, "$90,000") {
        t.Fatalf("amountLine = %q", s)
    }
}

func TestAmountLineFallback(t *testing.T) {
    if err := openDB(filepath.Join(t.TempDir(), "watches.db")); err != nil {
        t.Fatalf("openDB: %v", err)
    }
    defer closeDB()
    if err := rates.Init(db); err != nil {
        t.Fatalf("rates.Init: %v", err)
    }
    rates.Add(60000)
    // a confirmed tx whose block time predates our rate history (older than the
    // tolerance) still shows USD, falling back to the latest known rate.
    var s = amountLine(1.5, time.Now().Add(-5*24*time.Hour), false)
    if !strings.Contains(s, "$90,000") {
        t.Fatalf("expected USD via fallback for an old confirmed tx: %q", s)
    }
}

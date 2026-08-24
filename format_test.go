package main

import "path/filepath"
import "strings"
import "testing"
import "time"

import "bitnsbot/rates"

func TestUSDFormat(t *testing.T) {
    if got := usd(150000000, 60000); got != "$90,000" { // ≥ $100 → whole dollars, grouped
        t.Fatalf("1.5 BTC @ 60000 = %q", got)
    }
    if got := usd(1000000000, 12345.67); got != "$123,457" { // 123456.70 rounds up
        t.Fatalf("grouping = %q", got)
    }
    if got := usd(100000000, 100); got != "$100" { // exactly $100 → no cents
        t.Fatalf("boundary = %q", got)
    }
    if got := usd(100000, 58234.12); got != "$58.23" { // < $100 → keep cents
        t.Fatalf("0.001 BTC @ 58234.12 = %q", got)
    }
    if got := usd(50000000, 199); got != "$99.50" { // just under $100 → keep cents
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
    if s := amountLine(150000000, time.Time{}, true, 123); strings.Contains(s, "$") {
        t.Fatalf("expected no USD without a rate: %q", s)
    }
    rates.Add(60000)
    var s = amountLine(150000000, time.Time{}, true, 123)
    if !strings.Contains(s, "1.5 BTC") || !strings.Contains(s, "$90,000") {
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
    var s = amountLine(150000000, time.Now().Add(-5*24*time.Hour), false, 123)
    if !strings.Contains(s, "$90,000") {
        t.Fatalf("expected USD via fallback for an old confirmed tx: %q", s)
    }
}

// A watched address that spends reports a negative net move, the only place these
// formatters see a negative — and both used to mangle it ("- 100 010 000",
// "$-29450.00") because they split the sign off the digits.
func TestNegativeAmounts(t *testing.T) {
    if got := group(-100010000); got != "-100 010 000" {
        t.Fatalf("group(-100010000) = %q", got)
    }
    if got := group(-999); got != "-999" {
        t.Fatalf("group(-999) = %q", got)
    }
    if got := group(-100010000); got != "-100 010 000" {
        t.Fatalf("group(-100010000) = %q", got)
    }
    if got := usd(-100010000, 29447.06); got != "-$29,450" {
        t.Fatalf("usd(-100010000, 29447.06) = %q", got)
    }
    if got := usd(-100000, 50000); got != "-$50.00" {
        t.Fatalf("usd(-100000, 50000) = %q", got)
    }
}

func TestDateTime(t *testing.T) {
    var tm = time.Date(2023, 6, 15, 10, 30, 0, 0, time.UTC)

    // English (nil trans) — returns English month name unchanged.
    if got := trans(nil).DateTime(tm); got != "15 June 2023 10:30" {
        t.Fatalf("nil trans: expected '15 June 2023 10:30', got %q", got)
    }

    // Russian — month translated to genitive form.
    if got := langTrans["ru"].DateTime(tm); got != "15 июня 2023 10:30" {
        t.Fatalf("ru: expected '15 июня 2023 10:30', got %q", got)
    }

    // Spanish.
    if got := langTrans["es"].DateTime(tm); got != "15 junio 2023 10:30" {
        t.Fatalf("es: expected '15 junio 2023 10:30', got %q", got)
    }
}

// trimZeros must not touch an integer. With decimals=0 the old inline trim
// turned "870" into "87", silently dropping an order of magnitude — the reason
// humSize takes a decimals argument rather than reusing metric for whole units.
func TestMetricWholeUnits(t *testing.T) {
    var cases = []struct {
        in       float64
        decimals int
        want     string
    }{
        {870e9, 0, "870 G"},
        {869e9, 0, "869 G"},
        {868934901021, 0, "869 G"},
        {3299245, 1, "3.3 M"},
        {79000000000000, 2, "79 T"},
    }
    for _, c := range cases {
        if got := metric(c.in, c.decimals); got != c.want {
            t.Errorf("metric(%v, %d) = %q, want %q", c.in, c.decimals, got, c.want)
        }
    }
}

func TestHumSizeDecimals(t *testing.T) {
    if got := humSize(868934901021, 2, 0); got != "868.93 GB" {
        t.Errorf("humSize(.., 2) = %q, want 868.93 GB", got)
    }
    if got := humSize(868934901021, 0, 0); got != "869 GB" {
        t.Errorf("humSize(.., 0) = %q, want 869 GB", got)
    }
    if got := humSize(870000000000, 0, 0); got != "870 GB" {
        t.Errorf("humSize(870 GB, 0) = %q, want 870 GB — a trailing zero must survive", got)
    }
}

// An address's First tx / Last tx read as dates: the hour of the day says
// nothing about them, so day drops the clock time when is keeps.
func TestDayDropsTheTime(t *testing.T) {
    var old = time.Date(2025, time.April, 16, 17, 18, 42, 0, time.UTC).Unix()
    if got, want := day(old, 0), "16 april 2025"; got != want {
        t.Errorf("day = %q, want %q", got, want)
    }
    if got, want := when(old, 0), "16 april 2025 17:18"; got != want {
        t.Errorf("when = %q, want %q — only the address dates lose the time", got, want)
    }
}

// Both keep the relative form for anything recent, which never had a time in it.
func TestDayKeepsTheRelativeForm(t *testing.T) {
    var recent = time.Now().Add(-48 * time.Hour).Unix()
    if got := day(recent, 0); got != when(recent, 0) {
        t.Errorf("day = %q but when = %q; a recent date should read the same either way", got, when(recent, 0))
    }
    if got := day(recent, 0); !strings.Contains(got, "ago") {
        t.Errorf("day = %q, want a relative form", got)
    }
}

// The month is still translated, and dropping the time must not drop that.
func TestDateTranslatesTheMonth(t *testing.T) {
    var tm = time.Date(2025, time.April, 16, 17, 18, 0, 0, time.UTC)
    var ru = langTrans["ru"]
    if ru == nil {
        t.Skip("no ru table")
    }
    var full, short = ru.DateTime(tm), ru.Date(tm)
    if !strings.HasPrefix(full, strings.TrimSuffix(short, "")) {
        t.Errorf("Date %q is not DateTime %q without the time", short, full)
    }
    if strings.Contains(short, "April") {
        t.Errorf("Date %q left the month untranslated", short)
    }
    if strings.Contains(short, "17:18") {
        t.Errorf("Date %q still carries the time", short)
    }
}

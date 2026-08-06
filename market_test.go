package main

import "testing"

// Market figures use finance's suffixes, not SI's: a capitalisation quoted in
// "gigadollars" would read as a units error, which is why this is not `metric`.
func TestMoney(t *testing.T) {
    var cases = map[float64]string{
        1327983664334: "$1.33 T",
        31914279096:   "$31.91 B",
        1500000:       "$1.5 M",
        2500:          "$2500",
        999:           "$999",
        0:             "$0",
        -31914279096:  "-$31.91 B",
    }
    for in, want := range cases {
        if got := money(in, 1); got != want {
            t.Errorf("money(%v) = %q, want %q", in, got, want)
        }
    }
}

// A price quote keeps its cents — unlike `usd`, which drops them from $100 up
// because on a transaction amount they are noise.
func TestPrice(t *testing.T) {
    var cases = map[float64]string{
        66223:      "$66,223.00",
        66223.456:  "$66,223.46",
        0.5:        "$0.50",
        1234567.89: "$1,234,567.89",
        -1608.42:   "-$1,608.42",
    }
    for in, want := range cases {
        if got := price(in); got != want {
            t.Errorf("price(%v) = %q, want %q", in, got, want)
        }
    }
    // cents rounding up to a whole dollar must carry, not print ".100"
    if got := price(9.999); got != "$10.00" {
        t.Errorf("price(9.999) = %q, want $10.00", got)
    }
}

// Changes are always signed so direction is visible at a glance.
func TestChange(t *testing.T) {
    if got := change(66223, 64682.15); got != "+$1,540.85 (+2.38%)" {
        t.Errorf("rise = %q", got)
    }
    // the same absolute move gives a different percentage in each direction,
    // because it is measured against the *earlier* price, not the current one:
    // 1540.85/64682.15 = 2.38% rising, 1540.85/66223 = 2.33% falling
    if got := change(64682.15, 66223); got != "-$1,540.85 (-2.33%)" {
        t.Errorf("fall = %q", got)
    }
    if got := change(100, 100); got != "+$0.00 (+0%)" {
        t.Errorf("flat = %q", got)
    }
    // no earlier price to compare against means no percentage, not a divide by zero
    if got := change(100, 0); got != "+$100.00" {
        t.Errorf("no baseline = %q", got)
    }
}

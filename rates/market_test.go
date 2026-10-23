package rates

import "testing"

// A price quote keeps its cents — unlike main's `usd`, which drops them from $100 up
// because on a transaction amount they are noise.
//
// cents rounding up to a whole dollar must carry, not print ".100"
func TestPrice(t *testing.T) {
    var cases = map[float64]string{
        66223:      "$66,223.00",
        66223.456:  "$66,223.46",
        0.5:        "$0.50",
        1234567.89: "$1,234,567.89",
        -1608.42:   "-$1,608.42",
    }
    for in, want := range cases {
        if got := Price(in); got != want {
            t.Errorf("Price(%v) = %q, want %q", in, got, want)
        }
    }
    if got := Price(9.999); got != "$10.00" {
        t.Errorf("Price(9.999) = %q, want $10.00", got)
    }
}

// Changes are always signed so direction is visible at a glance.
//
// the same absolute move gives a different percentage in each direction,
// because it is measured against the *earlier* price, not the current one:
// 1540.85/64682.15 = 2.38% rising, 1540.85/66223 = 2.33% falling
//
// no earlier price to compare against means no percentage, not a divide by zero
func TestChange(t *testing.T) {
    if got := Change(66223, 64682.15); got != "+$1,540.85 (+2.38%)" {
        t.Errorf("rise = %q", got)
    }
    if got := Change(64682.15, 66223); got != "-$1,540.85 (-2.33%)" {
        t.Errorf("fall = %q", got)
    }
    if got := Change(100, 100); got != "+$0.00 (+0%)" {
        t.Errorf("flat = %q", got)
    }
    if got := Change(100, 0); got != "+$100.00" {
        t.Errorf("no baseline = %q", got)
    }
}

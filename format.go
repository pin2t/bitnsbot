package main

import "fmt"
import "math"
import "strconv"
import "strings"
import "time"

import "bitnsbot/rates"

func ago(n int, unit string) string {
    if n == 1 { return "1 " + unit + " ago" }
    return fmt.Sprintf("%d %ss ago", n, unit)
}

func when(unix int64) string {
    var t = time.Unix(unix, 0)
    if t.After(time.Now().AddDate(0, -3, 0)) {
        var since = time.Since(t)
        switch {
        case since < time.Minute:
            return "just now"
        case since < time.Hour:
            return ago(int(since.Minutes()), "minute")
        case since < 24*time.Hour:
            return ago(int(since.Hours()), "hour")
        case since < 31*24*time.Hour:
            return ago(int(since.Hours()/24), "day")
        default:
            return ago(int(since.Hours()/24/30), "month")
        }
    }
    return strings.ToLower(t.UTC().Format("2 January 2006 15:04"))
}

func short(s string) string {
    if len(s) <= 15 { return s }
    return s[:6] + "..." + s[len(s)-6:]
}

// group renders an integer with space-separated thousands. The sign is split off
// first: grouping "-100010000" as-is would put a space right after the minus
// ("- 100 010 000"), since the loop counts back from the end of the whole string.
func group(n int64) string {
    var sign = ""
    if n < 0 {
        sign, n = "-", -n
    }
    var s = strconv.FormatInt(n, 10)
    for i := len(s) - 3; i > 0; i -= 3 {
        s = s[:i] + " " + s[i:]
    }
    return sign + s
}

func satoshi(btc float64) string {
    return group(int64(math.Round(btc * 1e8)))
}

// metric renders a large number with a metric suffix, scaling by 1000 —
// e.g. metric(3299245, 1) → "3.3 M", metric(6719, 1) → "6.7 k", and (used for
// block difficulty) metric(79000000000000, 2) → "79 T".
func metric(f float64, decimals int) string {
    var unit = ""
    for _, u := range []string{" k", " M", " G", " T", " P", " E"} {
        if f < 1000 { break }
        f /= 1000
        unit = u
    }
    return strings.TrimRight(strings.TrimRight(strconv.FormatFloat(f, 'f', decimals, 64), "0"), ".") + unit
}

// compactBtc renders a BTC amount compactly with a USD approximation at the
// latest rate: as BTC once it reaches 0.05 BTC ("0.5 BTC"), otherwise in sats
// ("100 000 sats"), so large and small amounts each read naturally.
func compactBtc(btc float64) string {
    if btc >= 0.05 {
        return btcAmount(btc)
    }
    return amountLine(btc, time.Time{}, true)
}

// timeCompact renders a past time relatively and compactly ("2 m ago", "5 d ago",
// "3 h ago", "10 min ago") within the last year, or the exact lowercase date for
// older ("very old") times. "m" is months here; minutes are "min".
func timeCompact(unix int64) string {
    var t = time.Unix(unix, 0)
    var since = time.Since(t)
    switch {
    case since < time.Minute:
        return "just now"
    case since < time.Hour:
        return fmt.Sprintf("%d min ago", int(since.Minutes()))
    case since < 24*time.Hour:
        return fmt.Sprintf("%d h ago", int(since.Hours()))
    case since < 30*24*time.Hour:
        return fmt.Sprintf("%d d ago", int(since.Hours()/24))
    case since < 365*24*time.Hour:
        return fmt.Sprintf("%d m ago", int(since.Hours()/24/30))
    default:
        return strings.ToLower(t.UTC().Format("2 January 2006"))
    }
}

// periodText renders a duration as its two most-significant non-zero units among
// years / months / days / hours / minutes — "3 y 2 d", "2 m 1 d", "5 h 10 min".
// Years and months use 365- and 30-day approximations, extracted in order.
func periodText(d time.Duration) string {
    var total = int(d.Minutes())
    var years = total / (365 * 24 * 60)
    total -= years * 365 * 24 * 60
    var months = total / (30 * 24 * 60)
    total -= months * 30 * 24 * 60
    var days = total / (24 * 60)
    total -= days * 24 * 60
    var hours = total / 60
    var mins = total - hours*60
    var units = []struct {
        n int
        s string
    }{{years, "y"}, {months, "m"}, {days, "d"}, {hours, "h"}, {mins, "min"}}
    var parts []string
    for _, u := range units {
        if u.n > 0 {
            parts = append(parts, fmt.Sprintf("%d %s", u.n, u.s))
        }
    }
    if len(parts) == 0 {
        return "0 min"
    }
    if len(parts) > 2 {
        parts = parts[:2]
    }
    return strings.Join(parts, " ")
}

// usd renders btc at the given USD/BTC rate. From $100 up the cents are dropped
// as noise ("$90,000"); below $100 they're kept, where they matter ("$5.99"). The
// sign is pulled out before either decision so a negative amount (the net move of
// an address that just spent) reads "-$29,450", not "$-29450.00".
func usd(btc, rate float64) string {
    var value = btc * rate
    var sign = ""
    if value < 0 {
        sign, value = "-", -value
    }
    if value < 100 {
        return fmt.Sprintf("%s$%.2f", sign, value)
    }
    var s = strconv.FormatInt(int64(math.Round(value)), 10)
    for i := len(s) - 3; i > 0; i -= 3 {
        s = s[:i] + "," + s[i:]
    }
    return sign + "$" + s
}

// amountLine formats a BTC amount as "<sat> sats (≈ $usd)", appending the USD
// approximation only when a rate is available. current selects the rate: the
// last stored rate for mempool/notification amounts (no confirmed time), or the
// rate nearest the transaction's time otherwise — falling back to the last
// stored rate for confirmed times that predate our rate history, so an old
// transaction still shows its value at today's rate rather than nothing.
func amountLine(btc float64, at time.Time, current bool) string {
    var s = satoshi(btc) + " sats"
    var rate float64
    var ok bool
    if current {
        rate, ok = rates.Last()
    } else {
        rate, ok = rates.At(at)
        if !ok {
            rate, ok = rates.Last()
        }
    }
    if ok {
        s += " (≈ " + usd(btc, rate) + ")"
    }
    return s
}

// btcAmount renders a BTC value as "<btc> BTC (≈ $usd)" at the latest rate —
// used for the large /mempool totals, where sats would be unwieldy. Values ≥ 1
// BTC show two decimals ("9449.72 BTC"); smaller ones keep satoshi precision
// (trailing zeros trimmed) so a fraction of a BTC doesn't round away to nothing.
func btcAmount(btc float64) string {
    var num string
    if btc >= 1 || btc <= -1 {
        num = strconv.FormatFloat(btc, 'f', 2, 64)
    } else {
        num = strings.TrimRight(strings.TrimRight(strconv.FormatFloat(btc, 'f', 8, 64), "0"), ".")
    }
    var s = num + " BTC"
    if rate, ok := rates.Last(); ok {
        s += " (≈ " + usd(btc, rate) + ")"
    }
    return s
}

// durationText renders elapsed time compactly: "45 sec", "12 min", "2 h 5 min".
func durationText(d time.Duration) string {
    switch {
    case d < time.Minute:
        return fmt.Sprintf("%d sec", int(d.Seconds()))
    case d < time.Hour:
        return fmt.Sprintf("%d min", int(d.Minutes()))
    default:
        var h = int(d / time.Hour)
        var m = int(d/time.Minute) % 60
        if m == 0 { return fmt.Sprintf("%d h", h) }
        return fmt.Sprintf("%d h %d min", h, m)
    }
}

// trimNum formats a float with at most `decimals` places, trailing zeros (and a
// trailing dot) trimmed — "123", "0.1", "4.5".
func trimNum(v float64, decimals int) string {
    return strings.TrimRight(strings.TrimRight(strconv.FormatFloat(v, 'f', decimals, 64), "0"), ".")
}

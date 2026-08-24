package main

import "fmt"
import "math"
import "strconv"
import "strings"
import "time"
import "bitnsbot/rates"
import "unicode/utf8"

func ago(n int, unit string, chat int64) string {
    var s = i18n(chat).String("ago")
    if n == 1 { return "1 " + unit + " " + s }
    return fmt.Sprintf("%d %s %s", n, unit, s)
}

func when(unix int64, chat int64) string {
    if s := relative(unix, chat); s != "" { return s }
    return strings.ToLower(i18n(chat).DateTime(time.Unix(unix, 0).UTC()))
}

// day is when without the clock time. An address's first and last activity is
// read as a date — the hour of the day says nothing about it.
func day(unix int64, chat int64) string {
    if s := relative(unix, chat); s != "" { return s }
    return strings.ToLower(i18n(chat).Date(time.Unix(unix, 0).UTC()))
}

// relative renders a time within the last three months as "2 days ago", and ""
// for anything older, which belongs in absolute form.
func relative(unix int64, chat int64) string {
    var t = time.Unix(unix, 0)
    if !t.After(time.Now().AddDate(0, -3, 0)) { return "" }
    var since = time.Since(t)
    switch {
    case since < time.Minute:     return i18n(chat).String("just now")
    case since < time.Hour:       return ago(int(since.Minutes()), i18n(chat).String("min"), chat)
    case since < 24*time.Hour:    return ago(int(since.Hours()), i18n(chat).String("h"), chat)
    case since < 31*24*time.Hour: return ago(int(since.Hours()/24), i18n(chat).String("d"), chat)
    default:                      return ago(int(since.Hours()/24/30), i18n(chat).String("m"), chat)
    }
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

// toSat converts a BTC amount as Bitcoin Core and the price APIs report it into
// whole satoshi, the unit every amount is carried and calculated in once inside
// the bot. This rounding is the one boundary between the two representations:
// JSON hands us BTC as a float, 0.1 BTC has no exact float64 value, and summing
// such floats accumulates error that integer satoshi cannot. Parse structs keep
// float64 because that is what the wire format is; nothing downstream does.
func toSat(btc float64) int64 { return int64(math.Round(btc * 1e8)) }

// toBTC converts satoshi back to BTC for *display only* — never for
// arithmetic. Exact for every real amount: the 21M-coin supply is 2.1e15 sats,
// well inside float64's 2^53 integer range.
func toBTC(sat int64) float64 { return float64(sat) / 1e8 }

// metric renders a large number with a metric suffix, scaling by 1000 —
// e.g. metric(3299245, 1) → "3.3 M", metric(6719, 1) → "6.7 k", and (used for
// block difficulty) metric(79000000000000, 2) → "79 T".
func metric(f float64, decimals int) string {
    var unit = ""
    for _, u := range []string{" K", " M", " G", " T", " P", " E"} {
        if f < 1000 { break }
        f /= 1000
        unit = u
    }
    return trimZeros(strconv.FormatFloat(f, 'f', decimals, 64)) + unit
}

// trimZeros drops a trailing ".500" down to ".5" and ".000" to nothing. It must
// leave an integer alone: trimming "870" as a plain string would strip the zero
// and yield "87".
func trimZeros(s string) string {
    if !strings.Contains(s, ".") { return s }
    return strings.TrimRight(strings.TrimRight(s, "0"), ".")
}

// bigCount renders a large count the way people say one aloud — "1.4 B",
// "12.5 M". Deliberately not metric, which is SI and says "G" where a count of
// things reads as billions; and not money, which prefixes a dollar sign.
func bigCount(n int64) string {
    var f = float64(n)
    var units = []struct {
        scale  float64
        suffix string
    }{{1e12, " T"}, {1e9, " B"}, {1e6, " M"}}
    for _, u := range units {
        if f >= u.scale { return trimNum(f/u.scale, 1) + u.suffix }
    }
    return group(n)
}

// humSize renders a byte count with a 1000-scaled unit. decimals is per call
// site: the bot's messages want two, the Mini App's card wants none.
func humSize(s int64, decimals int, chat int64) string {
    var f = float64(s)
    var unit = i18n(chat).String(" B")
    for _, u := range []string{i18n(chat).String(" KB"), i18n(chat).String(" MB"), i18n(chat).String(" GB"), i18n(chat).String(" TB"), i18n(chat).String(" PB"), i18n(chat).String(" EB")} {
        if f < 1000 { break }
        f /= 1000
        unit = u
    }
    return trimZeros(strconv.FormatFloat(f, 'f', decimals, 64)) + unit
}

// compactBTC renders a BTC amount compactly with a USD approximation at the
// latest rate: as BTC once it reaches 0.05 BTC ("0.5 BTC"), otherwise in sats
// ("100 000 sats"), so large and small amounts each read naturally.
func compactBTC(sat int64, chat int64) string {
    if sat >= notifyBTCThreshold {
        return btcAmount(sat)
    }
    return amountLine(sat, time.Time{}, true, chat)
}

// periodText renders a duration as its two most-significant non-zero units among
// years / months / days / hours / minutes — "3 y 2 d", "2 m 1 d", "5 h 10 min".
// Years and months use 365- and 30-day approximations, extracted in order.
func periodText(d time.Duration, chat int64) string {
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
    }{{years, i18n(chat).String("y")}, {months, i18n(chat).String("m")}, {days, i18n(chat).String("d")}, {hours, i18n(chat).String("h")}, {mins, i18n(chat).String("min")}}
    var parts []string
    for _, u := range units {
        if u.n > 0 {
            parts = append(parts, fmt.Sprintf("%d %s", u.n, u.s))
        }
    }
    if len(parts) == 0 {
        return i18n(chat).String("just now")
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
func usd(sat int64, rate float64) string {
    var value = toBTC(sat) * rate
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
func amountLine(sat int64, at time.Time, current bool, chat int64) string {
    var s = amountText(sat, chat)
    var rate float64
    var ok bool
    if current {
        rate, ok = rates.Last()
    } else {
        rate, ok = rates.At(at)
        if !ok { rate, ok = rates.Last() }
    }
    if ok { s += " (≈ " + usd(sat, rate) + ")" }
    return s
}

// btcAmount renders a BTC value as "<btc> BTC (≈ $usd)" at the latest rate —
// used for the large /mempool totals, where sats would be unwieldy. Values ≥ 1
// BTC show two decimals ("9449.72 BTC"); smaller ones keep sats precision
// (trailing zeros trimmed) so a fraction of a BTC doesn't round away to nothing.
func btcAmount(sat int64) string {
    var num string
    if sat >= 1e8 || sat <= -1e8 {
        num = strconv.FormatFloat(toBTC(sat), 'f', 2, 64)
    } else {
        num = strings.TrimRight(strings.TrimRight(strconv.FormatFloat(toBTC(sat), 'f', 8, 64), "0"), ".")
    }
    var s = num + " BTC"
    if rate, ok := rates.Last(); ok {
        s += " (≈ " + usd(sat, rate) + ")"
    }
    return s
}

// durationText renders elapsed time compactly: "45 sec", "12 min", "2 h 5 min".
func durationText(d time.Duration, chat int64) string {
    switch {
    case d < time.Minute: return i18n(chat).Sprintf("%d sec", int(d.Seconds()))
    case d < time.Hour:   return i18n(chat).Sprintf("%d min", int(d.Minutes()))
    default:
        var h = int(d / time.Hour)
        var m = int(d/time.Minute) % 60
        if m == 0 { return i18n(chat).Sprintf("%d h", h) }
        return i18n(chat).Sprintf("%d h %d min", h, m)
    }
}

// trimNum formats a float with at most `decimals` places, trailing zeros (and a
// trailing dot) trimmed — "123", "0.1", "4.5".
func trimNum(v float64, decimals int) string {
    return trimZeros(strconv.FormatFloat(v, 'f', decimals, 64))
}

// money renders a large dollar figure the way markets quote them —
// "$1.33 T", "$31.9 B". Deliberately not `metric`, which is SI and so says "G"
// where finance says "B"; market capitalisation reported in gigadollars would
// read as a units error.
func money(usd float64, chat int64) string {
    var sign = ""
    if usd < 0 { sign, usd = "-", -usd }
    var units = []struct {
        scale  float64
        suffix string
    }{{1e12, i18n(chat).String(" T")}, {1e9, i18n(chat).String(" B")}, {1e6, i18n(chat).String(" M")}}
    for _, u := range units {
        if usd >= u.scale {
            return sign + "$" + trimNum(usd/u.scale, 2) + u.suffix
        }
    }
    return sign + "$" + trimNum(usd, 2)
}

// price renders an exact dollar amount with cents and comma-grouped thousands:
// "$66,202.00". `usd` drops cents from $100 up, which is right for transaction
// amounts where they are noise, but wrong on a price quote where they are the
// point.
func price(usd float64) string {
    var sign = ""
    if usd < 0 { sign, usd = "-", -usd }
    var whole = int64(usd)
    var cents = int64(math.Round((usd - float64(whole)) * 100))
    if cents == 100 { whole, cents = whole+1, 0 }
    return fmt.Sprintf("%s$%s.%02d", sign, comma(whole), cents)
}

func comma(n int64) string {
    var s = strconv.FormatInt(n, 10)
    for i := len(s) - 3; i > 0; i -= 3 {
        s = s[:i] + "," + s[i:]
    }
    return s
}

// change renders a move against an earlier price as both an absolute amount and
// a percentage, always signed so a glance shows direction: "+$1,608.42 (+2.49%)".
func change(now, then float64) string {
    var delta = now - then
    var sign = "+"
    if delta < 0 { sign = "" } // price() carries the minus itself
    var pct = ""
    if then > 0 {
        pct = fmt.Sprintf(" (%s%s%%)", sign, trimNum(delta/then*100, 2))
    }
    return sign + price(delta) + pct
}

// notifyBTCThreshold is where a notification headline switches from sats to
// BTC. From five million sats up the sats figure stops being readable at a
// glance — "5 000 000 sats" versus "0.05 BTC", which is the same amount.
const notifyBTCThreshold = 5_000_000

// amountText renders an amount for a notification headline: sats up to
// notifyBTCThreshold, BTC above it, trailing zeros trimmed either way.
func amountText(sat int64, chat int64) string {
    if sat >= notifyBTCThreshold || sat <= -notifyBTCThreshold {
        return trimNum(toBTC(sat), 8) + " BTC"
    }
    return group(sat) + " " + i18n(chat).String("sats")
}

func joinAlign(pairs [][2]string) string {
    var pad int
    for _, p := range pairs { pad = max(pad, utf8.RuneCountInString(p[0]) + 1) }
    var lines []string
    for _, p := range pairs {
        lines = append(lines, fmt.Sprintf("%-*s %s", pad, p[0]+":", p[1]))
    }
    return strings.Join(lines, "\n")
}
package main

import "path/filepath"
import "strconv"
import "testing"
import "time"
import "bitnsbot/app"

// chartDB opens a database holding the given blocks, each a height, a time, the
// pool that mined it and the difficulty it was mined at.
func chartDB(t *testing.T, blocks ...blockInfo) {
    if err := openDB(filepath.Join(t.TempDir(), "chart.db")); err != nil {
        t.Fatalf("open db: %v", err)
    }
    t.Cleanup(func() { closeDB() })
    for i := range blocks {
        if err := flushBlocks([]*blockInfo{&blocks[i]}); err != nil { t.Fatalf("store block: %v", err) }
    }
}

// at is a block of the test's own: the fields the chart reads, and nothing else.
func at(height int64, when time.Time, miner string, difficulty float64) blockInfo {
    return blockInfo{Height: height, Hash: strconv.FormatInt(height, 16), Time: when.Unix(), Miner: miner,
        Difficulty: difficulty}
}

func utc(year int, month time.Month, day, hour, min, sec int) time.Time {
    return time.Date(year, month, day, hour, min, sec, 0, time.UTC)
}

// A Wednesday afternoon, so a week has started and a month has not ended.
var chartNow = utc(2026, time.September, 16, 15, 30, 0)

func values(c app.Chart) []string {
    var out []string
    for _, b := range c.Bars { out = append(out, b.Value) }
    return out
}

// A month is the last thirty UTC days, today the last of them, and a bar counts
// what the pool mined that day — nothing of another pool's, nothing from before
// the first day, and a block stamped a little ahead of now in today's.
//
// the day before the first
//
// the first moment of the first
//
// an hour ahead of now
//
// the scale holds the peak of 3 at a round 4, and the bars are shares of it
func TestMinerChartCountsByDay(t *testing.T) {
    chartDB(t,
        at(1, utc(2026, time.August, 17, 23, 59, 59), "Foundry USA", 1e14),
        at(2, utc(2026, time.August, 18, 0, 0, 0), "Foundry USA", 1e14),
        at(3, utc(2026, time.September, 15, 23, 59, 59), "Foundry USA", 1e14),
        at(4, utc(2026, time.September, 16, 1, 0, 0), "Foundry USA", 1e14),
        at(5, utc(2026, time.September, 16, 5, 0, 0), "AntPool", 1e14),
        at(6, utc(2026, time.September, 16, 10, 0, 0), "Foundry USA", 1e14),
        at(7, utc(2026, time.September, 16, 16, 30, 0), "Foundry USA", 1e14),
    )
    var c = minerChart("", "Foundry USA", "blocks", "month", chartNow)
    if !c.OK { t.Fatal("a month with blocks in it is not OK") }
    if len(c.Bars) != 30 { t.Fatalf("a month is %d bars, want 30", len(c.Bars)) }
    var got = values(c)
    if got[0] != "1 block" || got[1] != "0 blocks" || got[28] != "1 block" || got[29] != "3 blocks" {
        t.Errorf("bars = %v", got)
    }
    if c.Value != "5 blocks" || c.Label != "last 30 days" {
        t.Errorf("headline = %q %q, want 5 blocks, last 30 days", c.Value, c.Label)
    }
    if c.Bars[0].Label != "18 aug 2026" || c.Bars[29].Label != "16 sep 2026" {
        t.Errorf("the month runs %q to %q, want 18 aug 2026 to 16 sep 2026", c.Bars[0].Label, c.Bars[29].Label)
    }
    if c.Top != "4" || c.Mid != "2" {
        t.Errorf("scale = %s / %s, want 4 / 2", c.Top, c.Mid)
    }
    if c.Bars[29].Height != 75 || c.Bars[28].Height != 25 || c.Bars[1].Height != 0 {
        t.Errorf("heights = %v, %v, %v; want 75, 25, 0", c.Bars[29].Height, c.Bars[28].Height, c.Bars[1].Height)
    }
    var ticks []string
    for i, b := range c.Bars {
        if b.Tick != "" { ticks = append(ticks, strconv.Itoa(i)+"="+b.Tick) }
    }
    if len(ticks) != 3 || ticks[0] != "0=18 aug" || ticks[1] != "15=2 sep" || ticks[2] != "29=16 sep" {
        t.Errorf("ticks = %v, want the first, middle and last day", ticks)
    }
}

// Consumption is the pool's share of each bucket's blocks — unattributed ones
// counted, since somebody mined them — at the difficulty its own blocks carried.
// The figures are worked from the formula here rather than through
// miners.Consumption, so the two cannot drift together.
//
// one block's worth of a network at 1e14: 1e14 × 2^32 hashes a 600s block,
// at 1e-11 J a hash, in GW
//
// 2 of 4 blocks, at 1.5e14 on average
//
// 1 of 4 blocks at 1e14
//
// the whole month is one share: 3 of 8 blocks, at 4e14 / 3 on average
func TestMinerChartConsumptionIsTheShareOfEachBucket(t *testing.T) {
    chartDB(t,
        at(1, utc(2026, time.September, 15, 1, 0, 0), "Foundry USA", 1e14),
        at(2, utc(2026, time.September, 15, 2, 0, 0), "Foundry USA", 2e14),
        at(3, utc(2026, time.September, 15, 3, 0, 0), "Unknown", 2e14),
        at(4, utc(2026, time.September, 15, 4, 0, 0), "Unknown", 2e14),
        at(5, utc(2026, time.September, 16, 1, 0, 0), "Foundry USA", 1e14),
        at(6, utc(2026, time.September, 16, 2, 0, 0), "AntPool", 1e14),
        at(7, utc(2026, time.September, 16, 3, 0, 0), "AntPool", 1e14),
        at(8, utc(2026, time.September, 16, 4, 0, 0), "AntPool", 1e14),
    )
    var gw = 1e14 * 4294967296 / 600 * 1e-11 / 1e9
    var c = minerChart("", "Foundry USA", "consumption", "month", chartNow)
    var yesterday, today = c.Bars[28], c.Bars[29]
    if want := trimNum(0.5*1.5*gw, 2) + " GW"; yesterday.Value != want || want != "5.37 GW" {
        t.Errorf("yesterday = %q, want %q", yesterday.Value, want)
    }
    if want := trimNum(0.25*gw, 2) + " GW"; today.Value != want || want != "1.79 GW" {
        t.Errorf("today = %q, want %q", today.Value, want)
    }
    if want := trimNum(3.0/8*(4.0/3)*gw, 2) + " GW"; c.Value != want || want != "3.58 GW" {
        t.Errorf("the month = %q, want %q", c.Value, want)
    }
    if c.Bars[0].Value != "0 GW" {
        t.Errorf("a day with no blocks = %q, want 0 GW", c.Bars[0].Value)
    }
    if c.Top != "6" || c.Mid != "3" || yesterday.Height != 89.5 || today.Height != 29.8 {
        t.Errorf("scale %s / %s, heights %v and %v; want 6 / 3, 89.5 and 29.8", c.Top, c.Mid, yesterday.Height, today.Height)
    }
}

// A quarter is thirteen weeks starting on a Monday, the running one last; a year
// is twelve calendar months, the running one last.
//
// before the year's first month
//
// the Sunday before the quarter
//
// a week that straddles the new year names both years
func TestMinerChartWeeksAndMonths(t *testing.T) {
    chartDB(t,
        at(1, utc(2025, time.September, 30, 23, 59, 59), "F2Pool", 1e14),
        at(2, utc(2025, time.October, 31, 23, 59, 59), "F2Pool", 1e14),
        at(3, utc(2026, time.June, 21, 23, 59, 59), "F2Pool", 1e14),
        at(4, utc(2026, time.June, 22, 0, 0, 0), "F2Pool", 1e14),
        at(5, utc(2026, time.September, 13, 23, 0, 0), "F2Pool", 1e14),
        at(6, utc(2026, time.September, 14, 0, 0, 0), "F2Pool", 1e14),
    )
    var q = minerChart("", "F2Pool", "blocks", "quarter", chartNow)
    if len(q.Bars) != 13 { t.Fatalf("a quarter is %d bars, want 13", len(q.Bars)) }
    if q.Bars[0].Label != "22 jun – 28 jun 2026" || q.Bars[12].Label != "14 sep – 20 sep 2026" {
        t.Errorf("the quarter runs %q to %q", q.Bars[0].Label, q.Bars[12].Label)
    }
    if got := values(q); got[0] != "1 block" || got[11] != "1 block" || got[12] != "1 block" || q.Value != "3 blocks" {
        t.Errorf("weeks = %v, total %q", got, q.Value)
    }
    if q.Bars[6].Tick != "3 aug" || q.Label != "last 13 weeks" {
        t.Errorf("middle tick %q, label %q", q.Bars[6].Tick, q.Label)
    }
    var y = minerChart("", "F2Pool", "blocks", "year", chartNow)
    if len(y.Bars) != 12 { t.Fatalf("a year is %d bars, want 12", len(y.Bars)) }
    if y.Bars[0].Label != "oct 2025" || y.Bars[11].Label != "sep 2026" || y.Bars[6].Tick != "apr 2026" {
        t.Errorf("the year runs %q to %q, middle %q", y.Bars[0].Label, y.Bars[11].Label, y.Bars[6].Tick)
    }
    if got := values(y); got[0] != "1 block" || got[8] != "2 blocks" || got[11] != "2 blocks" || y.Value != "5 blocks" {
        t.Errorf("months = %v, total %q", got, y.Value)
    }
    if y.Label != "last 12 months" { t.Errorf("label %q", y.Label) }
    var jan = minerChart("", "F2Pool", "blocks", "quarter", utc(2026, time.January, 7, 12, 0, 0))
    if got := jan.Bars[11].Label; got != "29 dec 2025 – 4 jan 2026" {
        t.Errorf("the week across the new year = %q", got)
    }
}

// Russian declines a month after a day differently from one standing alone, so a
// day reads "20 мая" and a month "май".
func TestMinerChartIsTranslated(t *testing.T) {
    var may = utc(2026, time.May, 20, 12, 0, 0)
    chartDB(t, at(1, may, "AntPool", 1e14))
    var day = minerChart("ru", "AntPool", "blocks", "month", may)
    if day.Bars[29].Label != "20 мая 2026" || day.Bars[29].Tick != "20 мая" || day.Label != "за 30 дней" {
        t.Errorf("a day in May = %q, tick %q, label %q", day.Bars[29].Label, day.Bars[29].Tick, day.Label)
    }
    if day.Bars[29].Value != "1 блок" || day.Value != "1 блок" {
        t.Errorf("a block = %q", day.Bars[29].Value)
    }
    var month = minerChart("ru", "AntPool", "blocks", "year", may)
    if month.Bars[11].Label != "май 2026" || month.Label != "за 12 месяцев" {
        t.Errorf("May on its own = %q, label %q", month.Bars[11].Label, month.Label)
    }
    if got := minerChart("es", "AntPool", "blocks", "year", may).Bars[11].Label; got != "may 2026" {
        t.Errorf("Spanish May = %q", got)
    }
}

// A table with no block in the period is a collector that has not got there, not
// a pool that mined nothing — so the chart says it has nothing rather than
// drawing zeroes.
//
// while a period that has blocks, none of them the pool's, is zeroes as fact
func TestMinerChartWithNoBlocksInThePeriod(t *testing.T) {
    chartDB(t, at(1, utc(2020, time.January, 1, 0, 0, 0), "AntPool", 1e14))
    for _, period := range []string{"month", "quarter", "year"} {
        if c := minerChart("", "AntPool", "blocks", period, chartNow); c.OK {
            t.Errorf("%s with no blocks in it is OK: %+v", period, c)
        }
    }
    chartDB(t, at(1, chartNow, "AntPool", 1e14))
    var c = minerChart("", "Braiins Pool", "blocks", "month", chartNow)
    if !c.OK || c.Value != "0 blocks" || c.Top != "2" {
        t.Errorf("a pool with no blocks in a mined month = %+v", c)
    }
}

func TestNiceTop(t *testing.T) {
    for _, c := range []struct {
        v     float64
        whole bool
        top   string
        mid   string
    }{
        {0, true, "2", "1"}, {1, true, "2", "1"}, {3, true, "4", "2"}, {43, true, "60", "30"},
        {144, true, "200", "100"}, {1000, true, "1000", "500"}, {1001, true, "2000", "1000"},
        {12000, true, "20000", "10000"},
        {0, false, "1", "0.5"}, {5.37, false, "6", "3"}, {9, false, "10", "5"},
        {0.55, false, "0.6", "0.3"}, {0.17, false, "0.2", "0.1"}, {0.061, false, "0.08", "0.04"},
    } {
        var top = niceTop(c.v, c.whole)
        var gotTop, gotMid = strconv.FormatFloat(top, 'f', -1, 64), strconv.FormatFloat(top/2, 'f', -1, 64)
        if gotTop != c.top || gotMid != c.mid {
            t.Errorf("niceTop(%v, %v) = %s / %s, want %s / %s", c.v, c.whole, gotTop, gotMid, c.top, c.mid)
        }
    }
}

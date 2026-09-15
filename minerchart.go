package main

import "math"
import "sort"
import "strconv"
import "time"
import "bitnsbot/app"
import "bitnsbot/logging"
import "bitnsbot/miners"

// MinerChart backs the chart under the Mini App's miner page.
func (appSource) MinerChart(lang, name, data, period string) app.Chart {
    return minerChart(lang, name, data, period, time.Now())
}

// minerChart is what one pool mined in each day of the last 30, each week of the
// last 13 or each month of the last 12, read out of the blocks table. The buckets
// are calendar ones in UTC, so the last is the one still running; a week starts
// on a Monday.
//
// Consumption needs every block in a bucket, not only the pool's: it is the pool's
// share of the bucket's blocks at the difficulty its own blocks were mined at —
// the same formula /miners applies to the whole history, through
// miners.Consumption. The share is of all the blocks, unattributed ones included,
// since those were mined by somebody.
//
// OK is false when the table holds no block in the period at all, which is a
// collector that has not got there yet rather than a pool that mined nothing.
//
// where a month follows a day, and where it stands alone — Russian declines
// the two differently ("14 мая" against "май 2026")
//
// blocks_ts is what makes this a range of the index rather than a scan of
// every block on the chain
//
// a block stamped ahead of now lands in the last bucket, which is where
// the search leaves anything past the last start
//
// the first, the last and the one in the middle: enough to read the
// period off, and few enough not to collide at a phone's width
func minerChart(lang, name, data, period string, now time.Time) app.Chart {
    var out = app.Chart{Name: name, Data: data, Period: period}
    if db == nil { return out }
    var t = i18nl(lang)
    var short = []string{t.String("jan"), t.String("feb"), t.String("mar"), t.String("apr"), t.String("may"),
        t.String("jun"), t.String("jul"), t.String("aug"), t.String("sep"), t.String("oct"), t.String("nov"), t.String("dec")}
    var alone = []string{t.String("jan-alone"), t.String("feb-alone"), t.String("mar-alone"), t.String("apr-alone"),
        t.String("may-alone"), t.String("jun-alone"), t.String("jul-alone"), t.String("aug-alone"), t.String("sep-alone"),
        t.String("oct-alone"), t.String("nov-alone"), t.String("dec-alone")}
    now = now.UTC()
    var today = time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.UTC)
    var starts []time.Time
    switch period {
    case "quarter":
        var monday = today.AddDate(0, 0, -((int(today.Weekday()) + 6) % 7))
        for i := 12; i >= 0; i-- { starts = append(starts, monday.AddDate(0, 0, -7*i)) }
        out.Label = t.String("last 13 weeks")
    case "year":
        var first = time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, time.UTC)
        for i := 11; i >= 0; i-- { starts = append(starts, first.AddDate(0, -i, 0)) }
        out.Label = t.String("last 12 months")
    default:
        for i := 29; i >= 0; i-- { starts = append(starts, today.AddDate(0, 0, -i)) }
        out.Label = t.String("last 30 days")
    }
    var mined, all = make([]int64, len(starts)), make([]int64, len(starts))
    var difficulty = make([]float64, len(starts))
    var rows, err = db.Query("select ts, miner, difficulty from blocks where ts >= ?", starts[0].Unix())
    if err != nil {
        logging.Warn("mini app: miner chart: %v", err)
        return out
    }
    defer rows.Close()
    for rows.Next() {
        var ts int64
        var miner string
        var d float64
        if rows.Scan(&ts, &miner, &d) != nil { continue }
        var i = sort.Search(len(starts), func(i int) bool { return starts[i].Unix() > ts }) - 1
        all[i]++
        if miner == name {
            mined[i]++
            difficulty[i] += d
        }
    }
    if err := rows.Err(); err != nil {
        logging.Warn("mini app: miner chart: %v", err)
        return out
    }
    var consumption = func(m, n int64, d float64) float64 {
        if m == 0 { return 0 }
        return miners.Consumption(float64(m)/float64(n), d/float64(m))
    }
    var format = func(v float64) string {
        if data == "consumption" { return trimNum(v, 2) + " GW" }
        if v == 1 { return t.String("1 block") }
        return t.Sprintf("%d blocks", int64(v))
    }
    var values = make([]float64, len(starts))
    var total, blocks, peak = int64(0), int64(0), 0.0
    var totalDifficulty float64
    for i := range starts {
        values[i] = float64(mined[i])
        if data == "consumption" { values[i] = consumption(mined[i], all[i], difficulty[i]) }
        if values[i] > peak { peak = values[i] }
        total += mined[i]
        blocks += all[i]
        totalDifficulty += difficulty[i]
    }
    if blocks == 0 { return out }
    out.OK = true
    out.Value = format(float64(total))
    if data == "consumption" { out.Value = format(consumption(total, blocks, totalDifficulty)) }
    var top = niceTop(peak, data != "consumption")
    out.Top, out.Mid = strconv.FormatFloat(top, 'f', -1, 64), strconv.FormatFloat(top/2, 'f', -1, 64)
    for i, s := range starts {
        var day = strconv.Itoa(s.Day()) + " " + short[s.Month()-1]
        var bar = app.Bar{Height: math.Round(values[i]/top*1000) / 10, Value: format(values[i]),
            Label: day + " " + strconv.Itoa(s.Year())}
        var tick = day
        switch period {
        case "quarter":
            var end = s.AddDate(0, 0, 6)
            var from = day
            if s.Year() != end.Year() { from += " " + strconv.Itoa(s.Year()) }
            bar.Label = from + " – " + strconv.Itoa(end.Day()) + " " + short[end.Month()-1] + " " + strconv.Itoa(end.Year())
        case "year":
            bar.Label = alone[s.Month()-1] + " " + strconv.Itoa(s.Year())
            tick = bar.Label
        }
        if i == 0 || i == len(starts)/2 || i == len(starts)-1 { bar.Tick = tick }
        out.Bars = append(out.Bars, bar)
    }
    return out
}

// niceTop is the top of a chart's scale: the smallest of 1, 2, 4, 6 or 8 times a
// power of ten that holds v, so the scale and its half both read as round
// numbers. A count's scale is at least 2, or its half would be half a block.
//
// A negative power is divided by rather than multiplied, because 10^-1 has no
// exact float and 6 × it prints as 0.6000000000000001; 6 / 10 is the float
// nearest 0.6, and so is what strconv prints back as "0.6".
func niceTop(v float64, whole bool) float64 {
    if whole && v < 2 { return 2 }
    if v <= 0 { return 1 }
    for k := int(math.Floor(math.Log10(v))); ; k++ {
        for _, m := range []float64{1, 2, 4, 6, 8} {
            var top = m * math.Pow10(k)
            if k < 0 { top = m / math.Pow10(-k) }
            if top >= v { return top }
        }
    }
}

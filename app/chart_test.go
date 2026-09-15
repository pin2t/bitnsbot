package app

import "html"
import "strconv"
import "strings"
import "testing"

// liveChart mirrors what main's minerChart builds: a bar per day, week or month,
// the figures formatted, the heights already scaled, and ticks under the first,
// middle and last bars only. The second bar is zero, so a test can see that an
// empty bucket is still drawn.
func liveChart(name, data, period string) Chart {
    var n = 30
    if period == "quarter" { n = 13 }
    if period == "year" { n = 12 }
    var c = Chart{OK: true, Name: name, Data: data, Period: period, Value: "412 blocks", Label: "last 30 days",
        Top: "60", Mid: "30"}
    if data == "consumption" { c.Value, c.Top, c.Mid = "2.83 GW", "4", "2" }
    for i := 0; i < n; i++ {
        var bar = Bar{Height: 71.7, Value: "43 blocks", Label: strconv.Itoa(i+1) + " sep 2026"}
        if i == 1 { bar.Height, bar.Value = 0, "0 blocks" }
        if i == 0 || i == n/2 || i == n-1 { bar.Tick = strconv.Itoa(i+1) + " sep" }
        c.Bars = append(c.Bars, bar)
    }
    return c
}

// The page a pool name opens draws its chart under the fields, on blocks over the
// last month, so there is something to read before a button is touched.
//
// the data buttons are on the left and the periods on the right, so the
// first group is the one naming what is plotted
func TestMinerPageDrawsTheChart(t *testing.T) {
    var h = handler(t, "TESTTOKEN", fakeSource{})
    var body = html.UnescapeString(get(h, "/miner?name=AntPool", freshInitData("TESTTOKEN")).Body.String())
    var at = strings.Index(body, `id="minerchart"`)
    if at < 0 { t.Fatalf("the miner page has no chart: %s", body) }
    if at < strings.Index(body, `class="fields"`) {
        t.Error("the chart belongs below the fields")
    }
    for _, want := range []string{
        `class="on" hx-get="minerchart?name=AntPool&data=blocks&period=month"`,
        `class="" hx-get="minerchart?name=AntPool&data=consumption&period=month"`,
        `class="" hx-get="minerchart?name=AntPool&data=blocks&period=year"`,
        `<span class="cv">412 blocks</span> <span class="cl">last 30 days</span>`,
    } {
        if !strings.Contains(body, want) {
            t.Errorf("the chart is missing %q", want)
        }
    }
    if n := strings.Count(body, `<div class="bar" `); n != 30 {
        t.Errorf("a month is %d bars, want 30", n)
    }
    var chart = body[at:]
    if strings.Index(chart, ">Consumption<") > strings.Index(chart, ">Month<") {
        t.Error("the data selection must come before the period selection")
    }
}

// The chart the page ships with is the chart its buttons swap in, byte for byte —
// the same guard the cards have, since both render one {{define}}.
func TestMinerChartIsWhatThePageShows(t *testing.T) {
    var h = handler(t, "TESTTOKEN", fakeSource{})
    var data = freshInitData("TESTTOKEN")
    var page = get(h, "/miner?name=AntPool", data).Body.String()
    var fragment = strings.TrimSpace(get(h, "/minerchart?name=AntPool&data=blocks&period=month", data).Body.String())
    if !strings.Contains(page, fragment) {
        t.Errorf("the miner page does not embed the exact chart fragment:\n%s", fragment)
    }
}

// Each button changes one selection and keeps the other, and the lit ones are
// what the bars under them are.
func TestMinerChartSelections(t *testing.T) {
    var h = handler(t, "TESTTOKEN", fakeSource{})
    var body = html.UnescapeString(get(h, "/minerchart?name=AntPool&data=consumption&period=year",
        freshInitData("TESTTOKEN")).Body.String())
    for _, want := range []string{
        `class="on" hx-get="minerchart?name=AntPool&data=consumption&period=year"`,
        `class="" hx-get="minerchart?name=AntPool&data=blocks&period=year"`,
        `class="" hx-get="minerchart?name=AntPool&data=consumption&period=month"`,
        `class="" hx-get="minerchart?name=AntPool&data=consumption&period=quarter"`,
        `<span class="cv">2.83 GW</span>`,
    } {
        if !strings.Contains(body, want) {
            t.Errorf("the chart is missing %q:\n%s", want, body)
        }
    }
    if n := strings.Count(body, `class="on"`); n != 2 {
        t.Errorf("%d buttons are lit, want exactly one per group", n)
    }
    if n := strings.Count(body, `<div class="bar" `); n != 12 {
        t.Errorf("a year is %d bars, want 12", n)
    }
    if !strings.Contains(body, `hx-target="#minerchart" hx-swap="outerHTML"`) {
        t.Error("a button must replace the chart, not the page around it")
    }
}

// Both selections arrive in a URL a user can edit and go straight back out into
// the buttons, so an unknown one is what the page opens on, never carried through.
func TestMinerChartRefusesWhatItDoesNotKnow(t *testing.T) {
    var h = handler(t, "TESTTOKEN", fakeSource{})
    var data = freshInitData("TESTTOKEN")
    var body = html.UnescapeString(get(h, `/minerchart?name=AntPool&data="><script>&period=decade`, data).Body.String())
    if strings.Contains(body, "<script>") || strings.Contains(body, "decade") {
        t.Errorf("an unknown selection was carried into the links: %s", body)
    }
    if !strings.Contains(body, `class="on" hx-get="minerchart?name=AntPool&data=blocks&period=month"`) {
        t.Error("an unknown selection should fall back to blocks over a month")
    }
    if got := get(h, "/minerchart?name=%20", data).Code; got != 400 {
        t.Errorf("a chart with no pool answered %d, want 400", got)
    }
    if got := get(h, "/minerchart?name=AntPool", "").Code; got != 401 {
        t.Errorf("the chart without initData answered %d, want 401", got)
    }
}

// Pool names have spaces in them, and every button carries the name.
func TestMinerChartKeepsANameWithSpaces(t *testing.T) {
    var h = handler(t, "TESTTOKEN", fakeSource{})
    var body = get(h, "/minerchart?name=Foundry+USA", freshInitData("TESTTOKEN")).Body.String()
    if n := strings.Count(body, `minerchart?name=Foundry&#43;USA&`); n != 5 {
        t.Errorf("%d of the 5 buttons carry the escaped name: %s", n, body)
    }
}

// The heights are percentages of the scale, a zero bar is drawn with no floor
// under it, and only the bars with a tick carry one.
func TestMinerChartBars(t *testing.T) {
    var h = handler(t, "TESTTOKEN", fakeSource{})
    var body = get(h, "/minerchart?name=AntPool&period=quarter", freshInitData("TESTTOKEN")).Body.String()
    for _, want := range []string{
        `<i class="nz" style="height: 71.7%"></i>`,
        `<i class="" style="height: 0%"></i>`,
        `data-value="0 blocks" data-label="2 sep 2026"`,
        `<div class="yax"><span>60</span><span>30</span><span>0</span></div>`,
    } {
        if !strings.Contains(body, want) {
            t.Errorf("the bars are missing %q:\n%s", want, body)
        }
    }
    if n := strings.Count(body, `<span><span>`); n != 3 {
        t.Errorf("%d ticks under the axis, want 3", n)
    }
}

// A table with no block in the period says so, and keeps its buttons so another
// period is still one tap away.
func TestMinerChartWithNothingInThePeriod(t *testing.T) {
    var h = handler(t, "TESTTOKEN", fakeSource{})
    var body = get(h, "/minerchart?name=NoBlocksYet&period=year", freshInitData("TESTTOKEN")).Body.String()
    if !strings.Contains(body, "no blocks cached for this period") {
        t.Errorf("an empty period should say so: %s", body)
    }
    if strings.Contains(body, `class="plot"`) {
        t.Error("an empty period drew a plot")
    }
    if !strings.Contains(body, ">Quarter<") {
        t.Error("an empty period lost its buttons")
    }
}

// Only a miner page has a chart, and only one that found the pool: a block page
// has none, and nor does "nothing found".
func TestOnlyAMinerPageHasAChart(t *testing.T) {
    var h = handler(t, "TESTTOKEN", fakeSource{d: liveBlockInfo()})
    var data = freshInitData("TESTTOKEN")
    for _, path := range []string{"/block?height=963268", "/miner?name=NoSuchPool"} {
        if body := get(h, path, data).Body.String(); strings.Contains(body, "minerchart") {
            t.Errorf("%s drew a chart", path)
        }
    }
}

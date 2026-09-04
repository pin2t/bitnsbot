package app

import "net/http"
import "net/http/httptest"
import "strconv"
import "strings"
import "testing"
import "time"

// getLang is get() with an Accept-Language header, which is all a plain browser
// offers.
func getLang(h http.Handler, path, initData, accept string) *httptest.ResponseRecorder {
    var r = httptest.NewRequest("GET", path, nil)
    if initData != "" { r.Header.Set("X-Telegram-Init-Data", initData) }
    if accept != "" { r.Header.Set("Accept-Language", accept) }
    var w = httptest.NewRecorder()
    h.ServeHTTP(w, r)
    return w
}

// initDataLang is a signed payload for a Telegram user whose client is set to
// lang, which is the setting the page follows when the app is opened from the
// bot.
func initDataLang(token, lang string) string {
    return signInitData(token, map[string]string{
        "auth_date": strconv.FormatInt(time.Now().Unix(), 10),
        "user":      `{"id":42,"first_name":"Pin","language_code":"` + lang + `"}`,
        "query_id":  "AAF",
    })
}

// Every language has a page, and each parses — a broken translation would take
// the binary down at startup rather than at the first request, so this is worth
// pinning even though parsePages panics.
func TestEveryLanguageHasAPage(t *testing.T) {
    for _, lang := range langs {
        if templates[lang] == nil {
            t.Errorf("no page parsed for %q", lang)
        }
    }
    if langs[0] != "en" {
        t.Errorf("English must come first: it is the reference and the fallback, got %q", langs[0])
    }
}

// A plain browser has only Accept-Language.
func TestAcceptLanguagePicksThePage(t *testing.T) {
    var h = handler(t, "TESTTOKEN", fakeSource{f: liveFees()})
    for _, c := range []struct{ accept, want, marker string }{
        {"ru-RU,ru;q=0.9,en;q=0.8", "ru", "Комиссии сети"},
        {"es-ES,es;q=0.9", "es", "Comisiones de red"},
        {"en-GB,en;q=0.9", "en", "Network fees"},
        {"", "en", "Network fees"},
        // nothing we have a page for falls back rather than failing
        {"ja,ko;q=0.8", "en", "Network fees"},
        // a language we do have, behind one we do not
        {"ja,ru;q=0.8", "ru", "Комиссии сети"},
    } {
        var w = getLang(h, "/", "", c.accept)
        if got := w.Header().Get("Content-Language"); got != c.want {
            t.Errorf("Accept-Language %q gave Content-Language %q, want %q", c.accept, got, c.want)
        }
        if !strings.Contains(w.Body.String(), c.marker) {
            t.Errorf("Accept-Language %q did not render %q", c.accept, c.marker)
        }
    }
}

// Quality decides the order, not the order the header is written in — reading it
// left to right would answer "en;q=0.1,ru" in English.
func TestAcceptLanguageHonoursQuality(t *testing.T) {
    var h = handler(t, "TESTTOKEN", fakeSource{f: liveFees()})
    if got := getLang(h, "/", "", "en;q=0.1,ru;q=0.9").Header().Get("Content-Language"); got != "ru" {
        t.Errorf("q-values ignored: got %q, want ru", got)
    }
    // a malformed q keeps the default of 1.0 rather than sinking the language
    if got := getLang(h, "/", "", "ru;q=oops,en;q=0.5").Header().Get("Content-Language"); got != "ru" {
        t.Errorf("a broken q demoted its language: got %q, want ru", got)
    }
}

// Opened from Telegram, the page follows the user's own client setting — which
// Telegram signs — over whatever the webview asks for.
func TestInitDataLanguageWins(t *testing.T) {
    var h = handler(t, "TESTTOKEN", fakeSource{f: liveFees()})
    var w = getLang(h, "/fees", initDataLang("TESTTOKEN", "ru"), "en-US,en;q=0.9")
    if got := w.Header().Get("Content-Language"); got != "ru" {
        t.Errorf("Content-Language = %q, want ru — the signed user setting wins", got)
    }
    if !strings.Contains(w.Body.String(), "Комиссии сети") {
        t.Errorf("fees card is not in Russian: %s", w.Body.String())
    }
    // a regional variant is still the language it is a variant of
    if got := getLang(h, "/fees", initDataLang("TESTTOKEN", "es-419"), "").Header().Get("Content-Language"); got != "es" {
        t.Errorf("es-419 gave %q, want es", got)
    }
    // A user language we have no page for falls through to what the webview
    // asks for rather than jumping to English: a reader whose Telegram is set
    // to Japanese on a Spanish phone is better served in Spanish.
    var jp = getLang(h, "/fees", initDataLang("TESTTOKEN", "ja"), "ru")
    if got := jp.Header().Get("Content-Language"); got != "ru" {
        t.Errorf("an untranslated user language gave %q, want the header's ru", got)
    }
    // with nothing left to fall back to, English
    if got := getLang(h, "/fees", initDataLang("TESTTOKEN", "ja"), "ko").Header().Get("Content-Language"); got != "en" {
        t.Errorf("nothing translated on either side gave %q, want en", got)
    }
}

// The cache is keyed by URL, which is the same for every reader — so without the
// language in the key one reader's page is served to the next in the wrong one.
func TestCacheIsPerLanguage(t *testing.T) {
    var h = handler(t, "TESTTOKEN", fakeSource{f: liveFees(), n: liveNetwork(), m: liveMarket(), b: liveBlocks()})
    for _, path := range []string{"/", "/fees", "/network", "/market", "/blocks"} {
        var data = ""
        if path != "/" { data = freshInitData("TESTTOKEN") }
        // fill the cache in English first, then ask in Russian
        getLang(h, path, data, "en")
        var ru = getLang(h, path, data, "ru").Body.String()
        if strings.Contains(ru, "Network fees") || strings.Contains(ru, "Blockchain</h2>") {
            t.Errorf("%s served the cached English page to a Russian reader", path)
        }
        // and back again, to catch a key that collapses the other way
        var en = getLang(h, path, data, "en").Body.String()
        if strings.Contains(en, "Комиссии сети") || strings.Contains(en, "Блокчейн") {
            t.Errorf("%s served the cached Russian page to an English reader", path)
        }
    }
}

// Notify has to drop the card in every language, or the readers of the ones it
// missed keep the stale copy until the TTL runs out.
func TestNotifyClearsEveryLanguage(t *testing.T) {
    var src = newCounting()
    var h = handler(t, "TESTTOKEN", src)
    var data = freshInitData("TESTTOKEN")
    for _, lang := range langs { getLang(h, "/fees", data, lang) }
    Notify("fees")
    for _, lang := range langs { getLang(h, "/fees", data, lang) }
    if n := src.count("fees"); n != 2*len(langs) {
        t.Errorf("fees rendered %d times across %d languages, want %d — Notify missed some",
            n, len(langs), 2*len(langs))
    }
}

// The translated pages are whole second copies of the page, so everything the
// English one is pinned for has to hold in them too.
func TestTranslatedPagesRenderCompletely(t *testing.T) {
    var h = handler(t, "TESTTOKEN", fakeSource{f: liveFees(), n: liveNetwork(), m: liveMarket(), b: liveBlocks()})
    for _, lang := range langs {
        var body = getLang(h, "/", "", lang).Body.String()
        if !strings.HasSuffix(strings.TrimSpace(body), "</html>") {
            t.Errorf("[%s] page does not end in </html> — a render that fails midway is silently truncated", lang)
        }
        // the cards and the tab bar are all present, whatever the words are
        for _, want := range []string{`id="fees"`, `id="network"`, `id="market"`, `id="blocklist"`,
            `id="watchpanel"`, `data-panel="home"`, `data-panel="watches"`, "963268"} {
            if !strings.Contains(body, want) {
                t.Errorf("[%s] page is missing %s", lang, want)
            }
        }
    }
}

// langSpy records the language each Source call was made in. The details rows,
// a block row's miner and a market period are main's words rather than the
// page's, so the reader's language has to reach Source — which is invisible in
// the response when the fake has nothing translated to render.
type langSpy struct {
    fakeSource
    seen *[]string
}

func (s langSpy) Market(lang string) Market {
    *s.seen = append(*s.seen, "market:"+lang)
    return s.fakeSource.Market(lang)
}

func (s langSpy) Blocks(lang string, rng Range) Blocks {
    *s.seen = append(*s.seen, "blocks:"+lang)
    return s.fakeSource.Blocks(lang, rng)
}

func (s langSpy) BlockInfo(lang string, height int64) Info {
    *s.seen = append(*s.seen, "block:"+lang)
    return s.fakeSource.BlockInfo(lang, height)
}

func (s langSpy) TxInfo(lang, txid string) Info {
    *s.seen = append(*s.seen, "tx:"+lang)
    return s.fakeSource.TxInfo(lang, txid)
}

func (s langSpy) AddrInfo(lang, addr string) Info {
    *s.seen = append(*s.seen, "address:"+lang)
    return s.fakeSource.AddrInfo(lang, addr)
}

func (s langSpy) MinerInfo(lang, name string) Info {
    *s.seen = append(*s.seen, "miner:"+lang)
    return s.fakeSource.MinerInfo(lang, name)
}

func TestSourceIsAskedInTheReadersLanguage(t *testing.T) {
    var seen []string
    var h = handler(t, "TESTTOKEN", langSpy{fakeSource{f: liveFees(), n: liveNetwork(),
        m: liveMarket(), b: liveBlocks(), d: liveBlockInfo(), t: liveTx(), a: liveAddr()}, &seen})
    var ru = initDataLang("TESTTOKEN", "ru")
    for _, path := range []string{"/", "/market", "/blocks", "/block?height=963268",
        "/tx?id=" + liveTxid, "/address?a=" + liveAddress, "/miner?name=AntPool"} {
        var data = ru
        if path == "/" { data = "" }
        getLang(h, path, data, "ru")
    }
    for _, want := range []string{"market:ru", "blocks:ru", "block:ru", "tx:ru", "address:ru", "miner:ru"} {
        var found = false
        for _, got := range seen {
            if got == want { found = true }
        }
        if !found {
            t.Errorf("Source was never asked %q; it saw %v", want, seen)
        }
    }
    // and an English reader is asked in English, not in whoever came first
    seen = nil
    getLang(h, "/market", freshInitData("TESTTOKEN"), "en")
    if len(seen) != 1 || seen[0] != "market:en" {
        t.Errorf("English reader asked Source %v, want [market:en]", seen)
    }
}

package main

import "os"
import "path/filepath"
import "strings"
import "testing"

// reference is a page carrying every shape the stripper has to get right: text
// between tags, actions in text, a translatable attribute and a structural one,
// a raw <script> body holding both a "<" and some markup in a string, and a
// <style> block.
const reference = `<!doctype html>
<html>
<head>
<style>.a { color: red } /* < not a tag */</style>
<script>
  for (var i = 0; i < 3; i++) { hint.textContent = "loading…"; }
  wp.innerHTML = '<div class="empty">Only inside Telegram</div>';
</script>
</head>
<body>
<h2>Network fees</h2>
<input id="q" placeholder="Transaction, block or address"
       hx-get="search" hx-trigger="keyup[key=='Enter']">
<p class="note">projected from {{.TxCount}} transactions</p>
{{define "fees"}}
  <button title="{{if .On}}Stop watching{{else}}Watch this{{end}}">&lt; Back</button>
{{end}}
</body>
</html>
`

// A translation that only rewrote words is the same page.
const translated = `<!doctype html>
<html>
<head>
<style>.a { color: blue } /* другое */</style>
<script>
  for (var i = 0; i < 5; i++) { hint.textContent = "загрузка…"; }
  wp.innerHTML = '<div class="empty">Только внутри Telegram</div>';
</script>
</head>
<body>
<h2>Комиссии сети</h2>
<input id="q" placeholder="Транзакция, блок или адрес"
       hx-get="search" hx-trigger="keyup[key=='Enter']">
<p class="note">по {{.TxCount}} транзакциям</p>
{{define "fees"}}
  <button title="{{if .On}}Не следить{{else}}Следить{{end}}">&lt; Назад</button>
{{end}}
</body>
</html>
`

func same(t *testing.T, a, b string) bool {
    t.Helper()
    return diff(strip(a), strip(b)) == ""
}

// Rewriting the words — including the ones inside a script, a style and a
// translatable attribute — leaves the page the same page.
func TestTranslationPasses(t *testing.T) {
    if d := diff(strip(reference), strip(translated)); d != "" {
        t.Errorf("a translation that only changed words was rejected:\n%s", d)
    }
}

// What the check exists to catch: markup that drifted.
func TestStructuralDriftFails(t *testing.T) {
    for _, c := range []struct{ name, broken string }{
        {"a renamed class", strings.Replace(translated, `class="note"`, `class="not"`, 1)},
        {"a dropped element", strings.Replace(translated, "<h2>Комиссии сети</h2>", "", 1)},
        {"a lost template action", strings.Replace(translated, "{{.TxCount}}", "", 1)},
        {"a changed action", strings.Replace(translated, "{{if .On}}", "{{if .Off}}", 1)},
        {"a broken attribute", strings.Replace(translated, `hx-get="search"`, `hx-get="find"`, 1)},
        {"a dropped attribute", strings.Replace(translated, ` hx-get="search"`, "", 1)},
        {"a whole script gone", strings.Replace(translated, "<script>", "", 1)},
        {"an unclosed tag", strings.Replace(translated, "</body>", "", 1)},
    } {
        if same(t, reference, c.broken) {
            t.Errorf("%s was not caught", c.name)
        }
    }
}

// Cosmetic differences a translator legitimately introduces must not fail: a
// longer word wraps its attributes differently.
func TestFormattingIsIgnored(t *testing.T) {
    var rewrapped = strings.Replace(translated,
        `<input id="q" placeholder="Транзакция, блок или адрес"
       hx-get="search" hx-trigger="keyup[key=='Enter']">`,
        `<input id="q"
          placeholder="Транзакция, блок или адрес"    hx-get="search"
          hx-trigger="keyup[key=='Enter']">`, 1)
    if !same(t, reference, rewrapped) {
        t.Error("re-wrapping a tag's attributes should not count as drift")
    }
    // and an HTML comment is neither structure nor translatable text
    var commented = strings.Replace(translated, "<body>", "<!-- перевод -->\n<body>", 1)
    if !same(t, reference, commented) {
        t.Error("an HTML comment should not count as drift")
    }
}

// The stripper keeps exactly the parts a translation must reproduce.
func TestSkeletonKeepsStructureOnly(t *testing.T) {
    var got = strings.Join(strip(reference), "\n")
    for _, want := range []string{
        `<h2>`, `{{define "fees"}}`, `{{.TxCount}}`,
        `<input id="q" placeholder="" hx-get="search" hx-trigger="keyup[key=='Enter']">`,
        `<button title="{{if .On}}{{else}}{{end}}">`,
    } {
        if !strings.Contains(got, want) {
            t.Errorf("skeleton is missing %q:\n%s", want, got)
        }
    }
    for _, gone := range []string{
        "Network fees", "Transaction, block", "Stop watching", "projected from",
        "color: red", "textContent", "Only inside Telegram", "Back",
    } {
        if strings.Contains(got, gone) {
            t.Errorf("skeleton still carries the translatable %q:\n%s", gone, got)
        }
    }
}

// A "<" inside a script is arithmetic, not a tag. Reading it as one made the
// scanner swallow whatever followed, which differed between two translations
// and failed a page that was in fact identical.
func TestScriptBodyIsNotMarkup(t *testing.T) {
    var got = strings.Join(strip(reference), "\n")
    if strings.Count(got, "<script>") != 1 || strings.Count(got, "</script>") != 1 {
        t.Errorf("the script's own tags should appear exactly once each:\n%s", got)
    }
    if strings.Contains(got, "<div class=\"empty\">") {
        t.Error("markup inside a script string is not part of the page's structure")
    }
    // The cost of that, stated outright: markup a script writes is invisible to
    // the check, so a translation may rename a class there and get away with it.
    if !same(t, reference, strings.Replace(translated, `class="empty"`, `class="empt"`, 1)) {
        t.Error("markup inside a script is documented as unchecked; this now checks it, so say so")
    }
}

// Which file is a translation of which.
func TestSplitNamesTheReference(t *testing.T) {
    for _, c := range []struct{ path, base, lang string }{
        {"app/app.html", "app/app", ""},
        {"app/app.ru.html", "app/app", "ru"},
        {"app/app.pt-BR.html", "app/app", "pt-BR"},
        // not a language tag, so it is a reference of its own rather than a
        // translation into "min"
        {"app/app.min.html", "app/app.min", ""},
        {"app/app.HTML5.html", "app/app.HTML5", ""},
        // three letters is a real ISO 639-3 code, but reading a build artifact
        // as a translation is the worse mistake
        {"app/app.fil.html", "app/app.fil", ""},
    } {
        var base, lang = split(c.path)
        if base != c.base || lang != c.lang {
            t.Errorf("split(%q) = (%q, %q), want (%q, %q)", c.path, base, lang, c.base, c.lang)
        }
    }
}

// End to end over real files, which is how the tool is actually run.
func TestRunOverADirectory(t *testing.T) {
    var dir = t.TempDir()
    var write = func(name, body string) {
        if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
            t.Fatal(err)
        }
    }
    write("page.html", reference)
    write("page.ru.html", translated)
    var want, err = skeleton(filepath.Join(dir, "page.html"))
    if err != nil { t.Fatal(err) }
    got, err := skeleton(filepath.Join(dir, "page.ru.html"))
    if err != nil { t.Fatal(err) }
    if d := diff(want, got); d != "" {
        t.Errorf("the pair on disk did not match:\n%s", d)
    }
    if len(want) < 10 {
        t.Errorf("the skeleton is suspiciously short (%d elements)", len(want))
    }
}

// The repo's own pages are what this exists for.
func TestAppPagesMatch(t *testing.T) {
    var want, err = skeleton("../../app/app.html")
    if err != nil { t.Fatal(err) }
    var found int
    for _, lang := range []string{"ru", "es"} {
        var got, err = skeleton("../../app/app." + lang + ".html")
        if err != nil { t.Fatal(err) }
        if d := diff(want, got); d != "" {
            t.Errorf("app.%s.html has drifted from app.html:\n%s", lang, d)
        }
        found++
    }
    if found != 2 {
        t.Errorf("checked %d translations, want 2", found)
    }
}

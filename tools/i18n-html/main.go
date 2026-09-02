// i18n-html checks that a page's translations still have the same shape as the
// page itself. Run it over a directory: for every reference xxx.html it finds
// the translations xxx.<lang>.html beside it, reduces each to the parts a
// translator must not touch — the tags, their attributes and the template
// actions — and compares those as text. A translation that lost an element,
// renamed a class or dropped a {{template}} call fails the check; one that only
// rewrote the words passes.
//
//	i18n-html ../app/
//
// It is the HTML half of what i18n-vet does for the Go string tables: nothing
// else notices when a copy drifts, because a translated page is a whole second
// file rather than a table of strings with one entry missing.
//
// What it deliberately does not see: the contents of <script> and <style>, which
// are dropped whole. Both carry user-visible strings that a translation has to
// rewrite, and neither can be compared without either forbidding that or
// parsing JavaScript. A missing <script> block is still caught — its tags are.
package main

import "fmt"
import "os"
import "path/filepath"
import "strings"

// text a reader sees lives in these attributes, so their values are stripped
// like the text between tags. Every other attribute — class, hx-get, the SVG
// path data — is structure and must match exactly.
var translatable = map[string]bool{
    "placeholder":  true,
    "title":        true,
    "alt":          true,
    "aria-label":   true,
    "aria-placeholder": true,
}

// raw is the set of elements whose content is not markup. Their bodies are
// dropped without being scanned, which is also what keeps a "<" in a script
// (i < 4, or an HTML string being assigned to innerHTML) from being read as a
// tag.
var raw = map[string]bool{"script": true, "style": true}

func main() {
    if len(os.Args) != 2 {
        fmt.Fprintln(os.Stderr, "usage: i18n-html <directory>")
        os.Exit(2)
    }
    var files, err = filepath.Glob(filepath.Join(os.Args[1], "*.html"))
    if err != nil {
        fmt.Fprintf(os.Stderr, "i18n-html: %v\n", err)
        os.Exit(2)
    }
    // group the translations under the reference they belong to
    var refs = map[string]bool{}
    var trans = map[string][]string{}
    for _, f := range files {
        var base, lang = split(f)
        if lang == "" {
            refs[base] = true
        } else {
            trans[base] = append(trans[base], f)
        }
    }
    var bad = 0
    for base, list := range trans {
        if !refs[base] {
            fmt.Fprintf(os.Stderr, "%s.<lang>.html has no reference %s.html\n", base, base)
            bad++
            continue
        }
        var want, err = skeleton(base + ".html")
        if err != nil {
            fmt.Fprintf(os.Stderr, "i18n-html: %v\n", err)
            os.Exit(2)
        }
        for _, f := range list {
            var got, err = skeleton(f)
            if err != nil {
                fmt.Fprintf(os.Stderr, "i18n-html: %v\n", err)
                os.Exit(2)
            }
            if d := diff(want, got); d != "" {
                fmt.Fprintf(os.Stderr, "%s does not match %s.html:\n%s\n", f, base, d)
                bad++
                continue
            }
            fmt.Printf("%s: %s matches (%d elements)\n", filepath.Base(base)+".html", lang(f), len(want))
        }
    }
    if bad > 0 { os.Exit(1) }
}

// split names the reference a file belongs to and the language it is in, which
// is "" for the reference itself. A dot in the stem is a language tag only when
// it looks like one, so a file named app.min.html is its own reference rather
// than a translation into "min".
func split(path string) (string, string) {
    var stem = strings.TrimSuffix(path, ".html")
    var dot = strings.LastIndex(stem, ".")
    if dot < 0 { return stem, "" }
    var tag = stem[dot+1:]
    if !isLang(tag) { return stem, "" }
    return stem[:dot], tag
}

// isLang is deliberately strict: two lowercase letters, optionally a region.
// That is the shape of every code Telegram puts in language_code, and it is what
// keeps a file like app.min.html being read as a translation into "min" — which
// is a real ISO 639-3 code, so allowing three letters would make an ordinary
// build artifact fail the check for a reason nobody would guess.
func isLang(s string) bool {
    var main, region, dashed = strings.Cut(s, "-")
    if len(main) != 2 || !lower(main) { return false }
    if dashed && (len(region) != 2 || !upper(region)) { return false }
    return true
}

func lower(s string) bool {
    for _, c := range s {
        if c < 'a' || c > 'z' { return false }
    }
    return true
}

func upper(s string) bool {
    for _, c := range s {
        if c < 'A' || c > 'Z' { return false }
    }
    return true
}

// lang reports the language a translation file is in, for the summary line.
func lang(path string) string {
    var _, l = split(path)
    return l
}

// diff reports the first line the two skeletons disagree on, which is where a
// translation drifted. One line is enough: everything after it is usually the
// same mistake echoing.
func diff(want, got []string) string {
    for i := 0; i < len(want) || i < len(got); i++ {
        var a, b = "(end of file)", "(end of file)"
        if i < len(want) { a = want[i] }
        if i < len(got) { b = got[i] }
        if a != b {
            return fmt.Sprintf("  element %d\n    reference:   %s\n    translation: %s", i+1, a, b)
        }
    }
    return ""
}

func skeleton(path string) ([]string, error) {
    var b, err = os.ReadFile(path)
    if err != nil { return nil, err }
    return strip(string(b)), nil
}

// strip reduces a page to the parts a translation has to reproduce exactly: one
// line per tag and per template action, in the order they appear. The text
// between tags is dropped except for the actions inside it, since that text is
// precisely what a translation rewrites.
func strip(src string) []string {
    var out []string
    for i := 0; i < len(src); {
        if a, n := action(src, i); n > 0 {
            out = append(out, a)
            i += n
            continue
        }
        if src[i] != '<' {
            i++
            continue
        }
        if strings.HasPrefix(src[i:], "<!--") {
            // A comment is neither structure nor user-visible text, so a
            // translation is free to keep, drop or translate it.
            var end = strings.Index(src[i:], "-->")
            if end < 0 { break }
            i += end + 3
            continue
        }
        var end = tagEnd(src, i)
        var name, norm = normalize(src[i:end])
        out = append(out, norm)
        i = end
        if raw[name] {
            // The body is not markup: skip it whole rather than scan it.
            var close = "</" + name
            var at = index(src[i:], close)
            if at < 0 { break }
            i += at
        }
    }
    return out
}

// index is strings.Index over an ASCII-lowercased haystack, so </SCRIPT> closes
// a <script> the way a browser reads it.
func index(hay, needle string) int {
    return strings.Index(strings.ToLower(hay), strings.ToLower(needle))
}

// tagEnd is the offset just past the ">" that closes the tag starting at i. A
// quoted attribute value and a template action are both atomic, so a ">" inside
// either does not end the tag.
func tagEnd(src string, i int) int {
    for j := i + 1; j < len(src); {
        if _, n := action(src, j); n > 0 {
            j += n
            continue
        }
        switch src[j] {
        case '"', '\'':
            var q = src[j]
            for j++; j < len(src) && src[j] != q; {
                if _, n := action(src, j); n > 0 {
                    j += n
                    continue
                }
                j++
            }
            j++
        case '>':
            return j + 1
        default:
            j++
        }
    }
    return len(src)
}

// action reads a {{...}} template action at i, returning it and its length. It
// is what survives inside text and inside a translated attribute value: the
// words around it are the translation's business, the action is not.
func action(src string, i int) (string, int) {
    if !strings.HasPrefix(src[i:], "{{") { return "", 0 }
    var end = strings.Index(src[i:], "}}")
    if end < 0 { return "", 0 }
    return src[i : i+end+2], end + 2
}

// normalize rewrites one tag into its comparable form: the element name, then
// each attribute with its whitespace collapsed, with the values a reader sees
// reduced to the actions they contain. Collapsing the whitespace is what lets a
// translation wrap its attributes differently — a longer word may not fit where
// the original did.
func normalize(tag string) (string, string) {
    var inner = strings.TrimSuffix(strings.TrimPrefix(tag, "<"), ">")
    inner = strings.TrimSpace(strings.TrimSuffix(strings.TrimSpace(inner), "/"))
    if inner == "" { return "", "<>" }
    // A doctype or a closing tag has no attributes — and no name to report,
    // since only an *opening* script or style tag begins a raw body. Naming the
    // closing one too made it skip ahead to the next </script>, swallowing
    // whatever tags lay between.
    if inner[0] == '!' || inner[0] == '/' {
        return "", "<" + strings.Join(strings.Fields(inner), " ") + ">"
    }
    var pos = 0
    for pos < len(inner) && !space(inner[pos]) { pos++ }
    var name = strings.ToLower(inner[:pos])
    var parts = []string{name}
    for pos < len(inner) {
        for pos < len(inner) && space(inner[pos]) { pos++ }
        var start = pos
        for pos < len(inner) && !space(inner[pos]) && inner[pos] != '=' { pos++ }
        if pos == start { break }
        var attr = strings.ToLower(inner[start:pos])
        if pos >= len(inner) || inner[pos] != '=' {
            parts = append(parts, attr)
            continue
        }
        pos++
        var value string
        value, pos = readValue(inner, pos)
        if translatable[attr] { value = actions(value) }
        parts = append(parts, attr+`="`+value+`"`)
    }
    return name, "<" + strings.Join(parts, " ") + ">"
}

func space(c byte) bool { return c == ' ' || c == '\t' || c == '\n' || c == '\r' }

// readValue reads an attribute value, quoted or bare, and returns it with the
// quotes removed along with the offset just past it.
func readValue(inner string, pos int) (string, int) {
    if pos < len(inner) && (inner[pos] == '"' || inner[pos] == '\'') {
        var q = inner[pos]
        var start = pos + 1
        for pos = start; pos < len(inner) && inner[pos] != q; {
            if _, n := action(inner, pos); n > 0 {
                pos += n
                continue
            }
            pos++
        }
        return inner[start:pos], pos + 1
    }
    var start = pos
    for pos < len(inner) && !space(inner[pos]) { pos++ }
    return inner[start:pos], pos
}

// actions keeps only the template actions in a run of text, which is what a
// translated attribute value must still carry.
func actions(s string) string {
    var out strings.Builder
    for i := 0; i < len(s); {
        if a, n := action(s, i); n > 0 {
            out.WriteString(a)
            i += n
            continue
        }
        i++
    }
    return out.String()
}

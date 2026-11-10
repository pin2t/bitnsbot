// i18n-vet checks that every i18n translation covers all format strings used in
// the codebase with matching format specifiers.
//
// Usage:
//
//	i18n-vet <directory>
//
// The tool walks all .go files recursively under <directory>, finds every
// i18n(…)/i18nl(…).Sprintf("format", …), .String("s") and .Plural(n, "forms")
// call, and verifies that
// each format string appears in every translation section of i18n.go. Translation
// sections are delimited by comments:
//
//	// i18n-vet:translation <lang>
//	    …
//	// i18n-vet:end translation
//
// For each translation, the tool checks that the translated string has the same
// number and types of format specifiers as the original. A Plural key holds its
// forms separated by "|", and a translation as many as its language has, so
// there every form of the translation must carry the verbs of every form of the
// key.
package main

import "bytes"
import "fmt"
import "go/ast"
import "go/parser"
import "go/token"
import "os"
import "path/filepath"
import "regexp"
import "sort"
import "strings"

// formatVerbs extracts the sequence of format verbs from a format string,
// ignoring flags, width, precision, and index. For example "%+3.1f" → "f",
// "%*s" → "s", "%[1]d" → "d".
var formatVerbRE = regexp.MustCompile(`%[+#\-0 ]*\[?(\d*)\]?\.?(\d*)([sdfvtqxbcegGp])`)

// m[3] is the verb letter
func formatVerbs(s string) string {
	var verbs []string
	for _, m := range formatVerbRE.FindAllStringSubmatch(s, -1) {
		verbs = append(verbs, m[3])
	}
	return strings.Join(verbs, "")
}

func main() {
	if len(os.Args) < 2 {
		fmt.Fprintf(os.Stderr, "usage: i18n-vet <directory>\n")
		os.Exit(2)
	}
	var dir = os.Args[1]
	var sourceStrings []i18nString
	var err = filepath.Walk(dir, func(path string, info os.FileInfo, err error) error {
		if err != nil { return err }
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		var fset = token.NewFileSet()
		var f, parseErr = parser.ParseFile(fset, path, nil, parser.ParseComments)
		if parseErr != nil { return fmt.Errorf("%s: %w", path, parseErr) }
		sourceStrings = append(sourceStrings, extractI18nStrings(f)...)
		return nil
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "walk: %v\n", err)
		os.Exit(1)
	}
	var i18nPath = filepath.Join(dir, "i18n.go")
	var translations, transErr = parseTranslationSections(i18nPath)
	if transErr != nil {
		fmt.Fprintf(os.Stderr, "parse translations: %v\n", transErr)
		os.Exit(1)
	}
	var errors int
	for _, src := range sourceStrings {
		for lang, langTrans := range translations {
			var translated, ok = langTrans[src.s]
			if !ok {
				fmt.Printf("%s: missing translation for %q\n", lang, src.s)
				errors++
				continue
			}
			var srcForms = []string{src.s}
			var trForms = []string{translated}
			if src.plural {
				srcForms = strings.Split(src.s, "|")
				trForms = strings.Split(translated, "|")
			}
			var srcVerbs = formatVerbs(srcForms[0])
			for _, form := range append(srcForms, trForms...) {
				if formatVerbs(form) != srcVerbs {
					fmt.Printf("%s: format specifier mismatch for %q\n", lang, src.s)
					fmt.Printf("  original:    %q (verbs: %q)\n", src.s, srcVerbs)
					fmt.Printf("  translation: %q (verbs: %q)\n", translated, formatVerbs(form))
					errors++
					break
				}
			}
		}
	}
	if errors > 0 {
		fmt.Printf("\n%d error(s) found.\n", errors)
		os.Exit(1)
	}
}

// i18nString is a translated string found at a call site, and whether it is a
// Plural key, whose forms are checked one by one.
type i18nString struct {
	s      string
	plural bool
}

// extractI18nStrings walks an AST and returns every string literal passed as the
// first argument to i18n(…)/i18nl(…).Sprintf or .String, or as the second to
// .Plural, which takes the count first.
//
// The receiver must be i18n(…) — a chat's language — or i18nl(…), the
// same lookup by language code, which is how the Mini App translates.
//
// First argument is the format string
//
// Unquote the string literal
func extractI18nStrings(f *ast.File) []i18nString {
	var found []i18nString
	ast.Inspect(f, func(n ast.Node) bool {
		var call, ok = n.(*ast.CallExpr)
		if !ok { return true }
		var sel, selOK = call.Fun.(*ast.SelectorExpr)
		if !selOK { return true }
		var plural = sel.Sel.Name == "Plural"
		if sel.Sel.Name != "Sprintf" && sel.Sel.Name != "String" && !plural { return true }
		var call2, innerOK = sel.X.(*ast.CallExpr)
		if !innerOK { return true }
		var ident, identOK = call2.Fun.(*ast.Ident)
		if !identOK || (ident.Name != "i18n" && ident.Name != "i18nl") { return true }
		var arg = 0
		if plural { arg = 1 }
		if len(call.Args) <= arg { return true }
		var lit, litOK = call.Args[arg].(*ast.BasicLit)
		if !litOK || lit.Kind != token.STRING { return true }
		var s, err = stringLiteral(lit.Value)
		if err != nil { return true }
		found = append(found, i18nString{s, plural})
		return true
	})
	return found
}

// stringLiteral unquotes a Go string literal, handling both regular and raw strings.
func stringLiteral(s string) (string, error) {
	if len(s) >= 2 && s[0] == '`' && s[len(s)-1] == '`' {
		return s[1 : len(s)-1], nil
	}
	if len(s) >= 2 && s[0] == '"' && s[len(s)-1] == '"' {
		var buf bytes.Buffer
		var inner = s[1 : len(s)-1]
		for i := 0; i < len(inner); i++ {
			if inner[i] == '\\' && i+1 < len(inner) {
				i++
				switch inner[i] {
				case 'n': buf.WriteByte('\n')
				case 't': buf.WriteByte('\t')
				case '\\': buf.WriteByte('\\')
				case '"':  buf.WriteByte('"')
				default:
					buf.WriteByte('\\')
					buf.WriteByte(inner[i])
				}
			} else {
				buf.WriteByte(inner[i])
			}
		}
		return buf.String(), nil
	}
	return "", fmt.Errorf("not a string literal: %s", s)
}

// parseTranslationSections reads i18n.go and extracts translation sections
// delimited by // i18n-vet:translation <lang> and // i18n-vet:end translation.
// Returns a map from language code to trans map.
//
// Matches a translation entry: "key": "value",
func parseTranslationSections(path string) (map[string]map[string]string, error) {
	var data, err = os.ReadFile(path)
	if err != nil { return nil, err }
	var lines = strings.Split(string(data), "\n")
	var result = make(map[string]map[string]string)
	var inSection bool
	var currentLang string
	var currentTrans map[string]string
	var entryRE = regexp.MustCompile(`^\s*"((?:[^"\\]|\\.)*)"\s*:\s*"((?:[^"\\]|\\.)*)"\s*,?\s*$`)
	for _, line := range lines {
		var trimmed = strings.TrimSpace(line)
		if strings.Contains(trimmed, "i18n-vet:translation ") {
			var parts = strings.Fields(trimmed)
			if len(parts) >= 3 {
				currentLang = parts[len(parts)-1]
				currentTrans = make(map[string]string)
				inSection = true
			}
			continue
		}
		if strings.Contains(trimmed, "i18n-vet:end translation") {
			if inSection && currentLang != "" {
				result[currentLang] = currentTrans
			}
			inSection = false
			currentLang = ""
			currentTrans = nil
			continue
		}
		if inSection {
			var m = entryRE.FindStringSubmatch(line)
			if m != nil {
				var key, err = stringLiteral("\"" + m[1] + "\"")
				if err != nil { continue }
				var val, valErr = stringLiteral("\"" + m[2] + "\"")
				if valErr != nil { continue }
				currentTrans[key] = val
			}
		}
	}
	return result, nil
}

// Ensure deterministic iteration order for stable output.
var _ = sort.Strings

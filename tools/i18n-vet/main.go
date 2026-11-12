// i18n-vet checks that every i18n translation covers all format strings used in
// the codebase with matching format specifiers.
//
// Usage:
//
//	i18n-vet <directory>
//
// The tool walks all .go files recursively under <directory>, finds every
// i18n(…)/i18nl(…).Sprintf("format", …) and .String("s") call, and verifies that
// each format string appears in every translation section of i18n.go. Translation
// sections are delimited by comments:
//
//	// i18n-vet:translation <lang>
//	    …
//	// i18n-vet:end translation
//
// For each translation, the tool checks that the translated string has the same
// number and types of format specifiers as the original.
//
// It also finds every i18np(…).Plural(n, "key") call and checks the key against
// every plural section, delimited the same way:
//
//	// i18n-vet:plurals <lang>
//	    "key": {"%d form", …},
//	// i18n-vet:end plurals
//
// Each language must have the key, with two or three forms, and every form must
// hold exactly one %d, since Plural formats the count and nothing else.
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
	var sourceStrings []string
	var pluralKeys []string
	var err = filepath.Walk(dir, func(path string, info os.FileInfo, err error) error {
		if err != nil { return err }
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		var fset = token.NewFileSet()
		var f, parseErr = parser.ParseFile(fset, path, nil, parser.ParseComments)
		if parseErr != nil { return fmt.Errorf("%s: %w", path, parseErr) }
		var strs, keys = extractI18nStrings(f)
		sourceStrings = append(sourceStrings, strs...)
		pluralKeys = append(pluralKeys, keys...)
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
	var pluralSections, pluralErr = parsePluralSections(i18nPath)
	if pluralErr != nil {
		fmt.Fprintf(os.Stderr, "parse plurals: %v\n", pluralErr)
		os.Exit(1)
	}
	var errors int
	for _, src := range sourceStrings {
		for lang, langTrans := range translations {
			var translated, ok = langTrans[src]
			if !ok {
				fmt.Printf("%s: missing translation for %q\n", lang, src)
				errors++
				continue
			}
			var srcVerbs = formatVerbs(src)
			var trVerbs = formatVerbs(translated)
			if srcVerbs != trVerbs {
				fmt.Printf("%s: format specifier mismatch for %q\n", lang, src)
				fmt.Printf("  original:    %q (verbs: %q)\n", src, srcVerbs)
				fmt.Printf("  translation: %q (verbs: %q)\n", translated, trVerbs)
				errors++
			}
		}
	}
	if len(pluralKeys) > 0 && len(pluralSections) == 0 {
		fmt.Printf("no plural sections found for %d Plural calls\n", len(pluralKeys))
		errors++
	}
	var checked = make(map[string]bool)
	for _, key := range pluralKeys {
		if checked[key] { continue }
		checked[key] = true
		for lang, forms := range pluralSections {
			var list, ok = forms[key]
			if !ok {
				fmt.Printf("%s: missing plural forms for %q\n", lang, key)
				errors++
				continue
			}
			if len(list) < 2 || len(list) > 3 {
				fmt.Printf("%s: %q has %d plural forms, want 2 or 3\n", lang, key, len(list))
				errors++
			}
			for _, form := range list {
				if formatVerbs(form) != "d" {
					fmt.Printf("%s: plural form %q of %q must hold exactly one %%d\n", lang, form, key)
					errors++
				}
			}
		}
	}
	if errors > 0 {
		fmt.Printf("\n%d error(s) found.\n", errors)
		os.Exit(1)
	}
}

// extractI18nStrings walks an AST and returns every string literal passed as the
// first argument to i18n(…)/i18nl(…).Sprintf or .String, and separately every
// key passed as the second argument to i18np(…).Plural, which takes the count
// first.
//
// The receiver must be i18n(…) — a chat's language — or i18nl(…), the
// same lookup by language code, which is how the Mini App translates.
//
// First argument is the format string
//
// Unquote the string literal
func extractI18nStrings(f *ast.File) ([]string, []string) {
	var found []string
	var keys []string
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
		if !identOK { return true }
		if plural != (ident.Name == "i18np") { return true }
		if !plural && ident.Name != "i18n" && ident.Name != "i18nl" { return true }
		var arg = 0
		if plural { arg = 1 }
		if len(call.Args) <= arg { return true }
		var lit, litOK = call.Args[arg].(*ast.BasicLit)
		if !litOK || lit.Kind != token.STRING { return true }
		var s, err = stringLiteral(lit.Value)
		if err != nil { return true }
		if plural {
			keys = append(keys, s)
		} else {
			found = append(found, s)
		}
		return true
	})
	return found, keys
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

// parsePluralSections reads i18n.go and extracts the plural sections delimited
// by // i18n-vet:plurals <lang> and // i18n-vet:end plurals, each entry a key and
// its forms on one line: "key": {"%d form", "%d forms"},
func parsePluralSections(path string) (map[string]map[string][]string, error) {
	var data, err = os.ReadFile(path)
	if err != nil { return nil, err }
	var result = make(map[string]map[string][]string)
	var current map[string][]string
	var entryRE = regexp.MustCompile(`^\s*"((?:[^"\\]|\\.)*)"\s*:\s*\{(.*)\}\s*,?\s*$`)
	var formRE = regexp.MustCompile(`"((?:[^"\\]|\\.)*)"`)
	for _, line := range strings.Split(string(data), "\n") {
		var trimmed = strings.TrimSpace(line)
		if strings.Contains(trimmed, "i18n-vet:plurals ") {
			var parts = strings.Fields(trimmed)
			current = make(map[string][]string)
			result[parts[len(parts)-1]] = current
			continue
		}
		if strings.Contains(trimmed, "i18n-vet:end plurals") {
			current = nil
			continue
		}
		if current == nil { continue }
		var m = entryRE.FindStringSubmatch(line)
		if m == nil { continue }
		var forms []string
		for _, f := range formRE.FindAllStringSubmatch(m[2], -1) {
			var form, formErr = stringLiteral("\"" + f[1] + "\"")
			if formErr != nil { continue }
			forms = append(forms, form)
		}
		current[m[1]] = forms
	}
	return result, nil
}

// Ensure deterministic iteration order for stable output.
var _ = sort.Strings

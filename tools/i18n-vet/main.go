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

func formatVerbs(s string) string {
	var verbs []string
	for _, m := range formatVerbRE.FindAllStringSubmatch(s, -1) {
		// m[3] is the verb letter
		verbs = append(verbs, m[3])
	}
	return strings.Join(verbs, "")
}

func main() {
	if len(os.Args) < 2 {
		fmt.Fprintf(os.Stderr, "usage: i18n-vet <directory>\n")
		os.Exit(2)
	}
	dir := os.Args[1]
	var sourceStrings []string
	err := filepath.Walk(dir, func(path string, info os.FileInfo, err error) error {
		if err != nil { return err }
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		fset := token.NewFileSet()
		f, err := parser.ParseFile(fset, path, nil, parser.ParseComments)
		if err != nil { return fmt.Errorf("%s: %w", path, err) }
		sourceStrings = append(sourceStrings, extractI18nStrings(f)...)
		return nil
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "walk: %v\n", err)
		os.Exit(1)
	}
	i18nPath := filepath.Join(dir, "i18n.go")
	translations, err := parseTranslationSections(i18nPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "parse translations: %v\n", err)
		os.Exit(1)
	}
	var errors int
	for _, src := range sourceStrings {
		for lang, langTrans := range translations {
			translated, ok := langTrans[src]
			if !ok {
				fmt.Printf("%s: missing translation for %q\n", lang, src)
				errors++
				continue
			}
			srcVerbs := formatVerbs(src)
			trVerbs := formatVerbs(translated)
			if srcVerbs != trVerbs {
				fmt.Printf("%s: format specifier mismatch for %q\n", lang, src)
				fmt.Printf("  original:    %q (verbs: %q)\n", src, srcVerbs)
				fmt.Printf("  translation: %q (verbs: %q)\n", translated, trVerbs)
				errors++
			}
		}
	}
	if errors > 0 {
		fmt.Printf("\n%d error(s) found.\n", errors)
		os.Exit(1)
	}
}

// extractI18nStrings walks an AST and returns every string literal passed as the
// first argument to i18n(…)/i18nl(…).Sprintf or .String.
func extractI18nStrings(f *ast.File) []string {
	var strings []string
	ast.Inspect(f, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok { return true }
		sel, ok := call.Fun.(*ast.SelectorExpr)
		if !ok { return true }
		if sel.Sel.Name != "Sprintf" && sel.Sel.Name != "String" { return true }
		// The receiver must be i18n(…) — a chat's language — or i18nl(…), the
		// same lookup by language code, which is how the Mini App translates.
		call2, ok := sel.X.(*ast.CallExpr)
		if !ok { return true }
		ident, ok := call2.Fun.(*ast.Ident)
		if !ok || (ident.Name != "i18n" && ident.Name != "i18nl") { return true }
		// First argument is the format string
		if len(call.Args) == 0 { return true }
		lit, ok := call.Args[0].(*ast.BasicLit)
		if !ok || lit.Kind != token.STRING { return true }
		// Unquote the string literal
		s, err := stringLiteral(lit.Value)
		if err != nil { return true }
		strings = append(strings, s)
		return true
	})
	return strings
}

// stringLiteral unquotes a Go string literal, handling both regular and raw strings.
func stringLiteral(s string) (string, error) {
	if len(s) >= 2 && s[0] == '`' && s[len(s)-1] == '`' {
		return s[1 : len(s)-1], nil
	}
	if len(s) >= 2 && s[0] == '"' && s[len(s)-1] == '"' {
		var buf bytes.Buffer
		inner := s[1 : len(s)-1]
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
func parseTranslationSections(path string) (map[string]map[string]string, error) {
	data, err := os.ReadFile(path)
	if err != nil { return nil, err }
	lines := strings.Split(string(data), "\n")
	result := make(map[string]map[string]string)
	var inSection bool
	var currentLang string
	var currentTrans map[string]string
	// Matches a translation entry: "key": "value",
	entryRE := regexp.MustCompile(`^\s*"((?:[^"\\]|\\.)*)"\s*:\s*"((?:[^"\\]|\\.)*)"\s*,?\s*$`)
	for _, line := range lines {
		trimmed := strings.TrimSpace(line)
		if strings.Contains(trimmed, "i18n-vet:translation ") {
			parts := strings.Fields(trimmed)
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
			m := entryRE.FindStringSubmatch(line)
			if m != nil {
				key, err := stringLiteral("\"" + m[1] + "\"")
				if err != nil { continue }
				val, err := stringLiteral("\"" + m[2] + "\"")
				if err != nil { continue }
				currentTrans[key] = val
			}
		}
	}
	return result, nil
}

// Ensure deterministic iteration order for stable output.
var _ = sort.Strings

package main

import "testing"

// English is the default table now, not nil: a source string still passes
// straight through, but a context key resolves to the word it stands for.
func TestEnglishIsTheDefault(t *testing.T) {
    for _, lang := range []string{"", "en", "zz"} {
        if got := i18nl(lang); got == nil {
            t.Fatalf("i18nl(%q) is nil; English is a table now", lang)
        }
        if got := i18nl(lang).String("average-tx"); got != "average" {
            t.Errorf("i18nl(%q) renders the context key as %q, want average", lang, got)
        }
        // a plain source string is its own translation and is not in the table
        if got := i18nl(lang).String("Reward + fees"); got != "Reward + fees" {
            t.Errorf("i18nl(%q) mangled a source string: %q", lang, got)
        }
    }
    if got := i18nl("ru").String("average-tx"); got != "средний" {
        t.Errorf("ru renders the context key as %q, want средний", got)
    }
}

// A context key is not a word — it is a key. Every language needs its own, or a
// reader is shown "average-tx" where a word belongs. i18n-vet catches the ones
// that appear at a call site; this catches a key added to English alone.
func TestContextKeysAreTranslated(t *testing.T) {
    for key := range english {
        for lang, table := range langTrans {
            if _, ok := table[key]; !ok {
                t.Errorf("%s has no translation for the context key %q", lang, key)
            }
        }
    }
}

// The whole point of the key: one English word, two Russian words, chosen by
// where it sits.
func TestAverageDependsOnWhatItAverages(t *testing.T) {
    var fee, size = i18nl("ru").String("average"), i18nl("ru").String("average-tx")
    if fee != "средняя" || size != "средний" {
        t.Errorf("average fee = %q, average size = %q; want средняя and средний", fee, size)
    }
    if i18nl("").String("average") != i18nl("").String("average-tx") {
        t.Error("in English both are the same word, which is what the key exists to hide")
    }
}

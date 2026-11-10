package main

import "testing"
import "time"

// English is the default table now, not nil: a source string still passes
// straight through, but a context key resolves to the word it stands for.
//
// a plain source string is its own translation and is not in the table
func TestEnglishIsTheDefault(t *testing.T) {
    for _, lang := range []string{"", "en", "zz"} {
        if got := i18nl(lang); got == nil {
            t.Fatalf("i18nl(%q) is nil; English is a table now", lang)
        }
        if got := i18nl(lang).String("average-tx"); got != "average" {
            t.Errorf("i18nl(%q) renders the context key as %q, want average", lang, got)
        }
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

// Russian has three forms and picks by the last two digits — 11-14 are the
// exception to 1 and 2-4 — where English and Spanish have two, and zero is plural.
func TestPlural(t *testing.T) {
    var cases = []struct {
        lang string
        n    int64
        want string
    }{
        {"ru", 0, "0 часов"}, {"ru", 1, "1 час"}, {"ru", 2, "2 часа"}, {"ru", 4, "4 часа"},
        {"ru", 5, "5 часов"}, {"ru", 11, "11 часов"}, {"ru", 12, "12 часов"}, {"ru", 14, "14 часов"},
        {"ru", 21, "21 час"}, {"ru", 22, "22 часа"}, {"ru", 25, "25 часов"}, {"ru", 101, "101 час"},
        {"ru", 111, "111 часов"}, {"ru", 112, "112 часов"}, {"ru", 122, "122 часа"}, {"ru", -1, "-1 час"},
        {"", 0, "0 hours"}, {"", 1, "1 hour"}, {"", 2, "2 hours"}, {"", 21, "21 hours"},
        {"es", 0, "0 horas"}, {"es", 1, "1 hora"}, {"es", 2, "2 horas"},
    }
    for _, c := range cases {
        if got := i18nl(c.lang).Plural(c.n, "%d hour|%d hours"); got != c.want {
            t.Errorf("%s: Plural(%d) = %q, want %q", c.lang, c.n, got, c.want)
        }
    }
}

// The examples the change was asked for, end to end through the formatters:
// a relative time and a period in each language, and a duration after "after",
// which Russian puts in the accusative.
func TestPluralUnits(t *testing.T) {
    var now = time.Now()
    var cases = []struct {
        lang, got, want string
    }{
        {"ru", relative(now.Add(-time.Hour-time.Second).Unix(), "ru"), "1 час назад"},
        {"ru", relative(now.Add(-5*time.Hour-time.Second).Unix(), "ru"), "5 часов назад"},
        {"ru", relative(now.Add(-18*time.Minute-time.Second).Unix(), "ru"), "18 минут назад"},
        {"ru", relative(now.Add(-21*time.Minute-time.Second).Unix(), "ru"), "21 минуту назад"},
        {"ru", relative(now.Add(-2*24*time.Hour-time.Second).Unix(), "ru"), "2 дня назад"},
        {"", relative(now.Add(-2*24*time.Hour-time.Second).Unix(), ""), "2 days ago"},
        {"", relative(now.Add(-time.Minute-time.Second).Unix(), ""), "1 minute ago"},
        {"es", relative(now.Add(-2*24*time.Hour-time.Second).Unix(), "es"), "hace 2 días"},
        {"es", relative(now.Add(-time.Hour-time.Second).Unix(), "es"), "hace 1 hora"},
        {"ru", periodText((15*365+8*30)*24*time.Hour, "ru"), "15 лет 8 месяцев"},
        {"ru", periodText((365+30)*24*time.Hour, "ru"), "1 год 1 месяц"},
        {"ru", periodText(2*time.Hour+time.Minute, "ru"), "2 часа 1 минута"},
        {"", periodText((15*365+8*30)*24*time.Hour, ""), "15 years 8 months"},
        {"es", periodText((15*365+30)*24*time.Hour, "es"), "15 años 1 mes"},
        {"ru", durationText(time.Minute, "ru"), "1 минуту"},
        {"ru", durationText(21*time.Second, "ru"), "21 секунду"},
        {"ru", durationText(2*time.Hour+3*time.Minute, "ru"), "2 часа 3 минуты"},
        {"es", durationText(90*time.Minute, "es"), "1 hora 30 minutos"},
    }
    for _, c := range cases {
        if c.got != c.want {
            t.Errorf("%s: got %q, want %q", c.lang, c.got, c.want)
        }
    }
}

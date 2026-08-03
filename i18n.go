package main

import "fmt"

// trans holds translations for a single language, mapping English source strings
// to translated strings. A nil or empty trans is valid and falls back to the
// original English string.
type trans map[string]string

// langTrans holds per-language translation maps. English is not stored — it is
// the default and Sprintf/String fall back to the original format string when no
// translation is found.
var langTrans = map[string]trans{
	"ru": {
		// ---- info.go ----
		"Couldn't find transaction %s.":                 "Транзакция %s не найдена.",
		"Transaction <code>%s</code>\n\n<pre>%s</pre>":  "Транзакция <code>%s</code>\n\n<pre>%s</pre>",
		"Couldn't find block %d.":                       "Блок %d не найден.",
		"%s doesn't look like a valid Bitcoin address.": "%s не похож на действительный Bitcoin-адрес.",
		"Address %s\n\n<pre>%s</pre>":                   "Адрес %s\n\n<pre>%s</pre>",

		// ---- main.go ----
		"Watching %s: %s (%s)":                     "Отслеживаю %s: %s (%s)",
		"Watching %s: %s":                          "Отслеживаю %s: %s",
		"You're not watching %s.":                   "Вы не отслеживаете %s.",
		"Stopped watching %s.":                      "Прекращено отслеживание %s.",
		"Estimated network fees\n\n<pre>%s</pre>\n%s":              "Оценка комиссий сети\n\n<pre>%s</pre>\n%s",
		"Mempool\n\n<pre>%s</pre>":                                 "Мемпул\n\n<pre>%s</pre>",
		"%d blocks":                                                "%d блоков",
		"%d. %s. %s mined, reward %s BTC, fees %s BTC, consumption %s GW": "%d. %s. %s добыто, награда %s BTC, комиссии %s BTC, потребление %s ГВт",
		"Top miners by blocks mined:\n\n%s":                        "Топ майнеров по добытым блокам:\n\n%s",
		"Bitcoin market\n\n<pre>%s</pre>":                          "Рынок Bitcoin\n\n<pre>%s</pre>",

		// ---- notify.go ----
		"%s is sending %s to\n":                                "%s отправляет %s на\n",
		"%s sats (%s sat/vB)":                                  "%s сат (%s сат/вБ)",
		"🔔 %s receiving %s. Transaction <code>%s</code>\nETA %s": "🔔 %s получает %s. Транзакция <code>%s</code>\nETA %s",
		"🔔 %s receiving %s. Transaction <code>%s</code>":        "🔔 %s получает %s. Транзакция <code>%s</code>",
		"Transaction <code>%s</code>":                            "Транзакция <code>%s</code>",
		"Confirmed in block #%d after %s. Transaction <code>%s</code>": "Подтверждено в блоке #%d через %s. Транзакция <code>%s</code>",
		"🔔 Transaction %s was confirmed in block #%d after %s":  "🔔 Транзакция %s подтверждена в блоке #%d через %s",
		"%s sent %s to %s. %s":    "%s отправил %s на %s. %s",
		"%s received %s. %s":      "%s получил %s. %s",
	},
	"es": {
		// ---- info.go ----
		"Couldn't find transaction %s.":                 "No se encontró la transacción %s.",
		"Transaction <code>%s</code>\n\n<pre>%s</pre>":  "Transacción <code>%s</code>\n\n<pre>%s</pre>",
		"Couldn't find block %d.":                       "No se encontró el bloque %d.",
		"%s doesn't look like a valid Bitcoin address.": "%s no parece una dirección Bitcoin válida.",
		"Address %s\n\n<pre>%s</pre>":                   "Dirección %s\n\n<pre>%s</pre>",

		// ---- main.go ----
		"Watching %s: %s (%s)":                     "Observando %s: %s (%s)",
		"Watching %s: %s":                          "Observando %s: %s",
		"You're not watching %s.":                   "No estás observando %s.",
		"Stopped watching %s.":                      "Se dejó de observar %s.",
		"Estimated network fees\n\n<pre>%s</pre>\n%s":              "Comisiones de red estimadas\n\n<pre>%s</pre>\n%s",
		"Mempool\n\n<pre>%s</pre>":                                 "Mempool\n\n<pre>%s</pre>",
		"%d blocks":                                                "%d bloques",
		"%d. %s. %s mined, reward %s BTC, fees %s BTC, consumption %s GW": "%d. %s. %s minado, recompensa %s BTC, comisiones %s BTC, consumo %s GW",
		"Top miners by blocks mined:\n\n%s":                        "Principales mineros por bloques minados:\n\n%s",
		"Bitcoin market\n\n<pre>%s</pre>":                          "Mercado Bitcoin\n\n<pre>%s</pre>",

		// ---- notify.go ----
		"%s is sending %s to\n":                                "%s está enviando %s a\n",
		"%s sats (%s sat/vB)":                                  "%s sats (%s sat/vB)",
		"🔔 %s receiving %s. Transaction <code>%s</code>\nETA %s": "🔔 %s recibiendo %s. Transacción <code>%s</code>\nETA %s",
		"🔔 %s receiving %s. Transaction <code>%s</code>":        "🔔 %s recibiendo %s. Transacción <code>%s</code>",
		"Transaction <code>%s</code>":                            "Transacción <code>%s</code>",
		"Confirmed in block #%d after %s. Transaction <code>%s</code>": "Confirmado en bloque #%d después de %s. Transacción <code>%s</code>",
		"🔔 Transaction %s was confirmed in block #%d after %s":  "🔔 Transacción %s fue confirmada en bloque #%d después de %s",
		"%s sent %s to %s. %s":    "%s envió %s a %s. %s",
		"%s received %s. %s":      "%s recibió %s. %s",
	},
}

// chatLangs is an LRU cache of chat ID → language code, bounded to prevent
// unbounded growth from one-off chats.
var chatLangs = newLRU[int64, string](10000)

// SetChatLanguage sets the language for a specific chat.
func SetChatLanguage(chatID int64, lang string) {
	chatLangs.Put(chatID, lang)
}

// i18n returns the trans for the given chat's language, or nil when the
// language is "en" (the default). Sprintf and String fall back to the original
// English string when trans is nil or the key is missing.
func i18n(chatID int64) trans {
	lang, _ := chatLangs.Get(chatID)
	if lang == "" || lang == "en" {
		return nil
	}
	return langTrans[lang]
}

// Sprintf works like fmt.Sprintf but translates the format string first. If no
// translation exists, it falls back to the original format string.
func (t trans) Sprintf(format string, a ...interface{}) string {
	if t != nil {
		if translated, ok := t[format]; ok {
			return fmt.Sprintf(translated, a...)
		}
	}
	return fmt.Sprintf(format, a...)
}

// String returns a translated string for the given key. If no translation
// exists, it returns the key unchanged.
func (t trans) String(s string) string {
	if t != nil {
		if translated, ok := t[s]; ok {
			return translated
		}
	}
	return s
}

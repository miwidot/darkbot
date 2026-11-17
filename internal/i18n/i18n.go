// Package i18n holds darkbot's UI strings. One JSON file per language in
// locales/ is the single source for both the Go side (error messages shown
// in the UI) and the frontend (fetched via the App.GetTranslations binding).
//
// Adding a language: copy locales/en.json to locales/<code>.json, translate
// the values, add the code to Supported. TestLocalesComplete fails until
// every key and every {placeholder} matches English.
//
// Logs stay English on purpose: they are for bug reports.
package i18n

import (
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"strings"
)

// Default is used for missing keys and when no supported language matches.
const Default = "en"

// Supported lists the shipped languages (file names in locales/).
var Supported = []string{"en", "de"}

//go:embed locales/*.json
var files embed.FS

var dicts = mustLoad()

// mustLoad parses the embedded locale files. They are compiled into the
// binary, so a broken file is a build defect: panicking surfaces it in the
// first test run instead of shipping a half-translated UI.
func mustLoad() map[string]map[string]string {
	out := map[string]map[string]string{}
	for _, lang := range Supported {
		data, err := files.ReadFile("locales/" + lang + ".json")
		if err != nil {
			panic(fmt.Sprintf("i18n: missing locale %s: %v", lang, err))
		}
		m := map[string]string{}
		if err := json.Unmarshal(data, &m); err != nil {
			panic(fmt.Sprintf("i18n: invalid locale %s: %v", lang, err))
		}
		out[lang] = m
	}
	return out
}

// IsSupported reports whether code is a shipped language.
func IsSupported(code string) bool {
	_, ok := dicts[code]
	return ok
}

// Resolve picks the UI language: the user's choice if set and supported,
// else the first supported system language ("de-AT" counts as "de"),
// else Default.
func Resolve(preferred string, system []string) string {
	if IsSupported(preferred) {
		return preferred
	}
	for _, s := range system {
		base, _, _ := strings.Cut(strings.ToLower(strings.TrimSpace(s)), "-")
		base, _, _ = strings.Cut(base, "_")
		if IsSupported(base) {
			return base
		}
	}
	return Default
}

// Dict returns all strings for lang, with English filling any gaps. The map
// is a copy and safe to hand to the frontend.
func Dict(lang string) map[string]string {
	out := make(map[string]string, len(dicts[Default]))
	maps.Copy(out, dicts[Default])
	maps.Copy(out, dicts[lang])
	return out
}

// T translates key and fills {name} placeholders from args, given as
// alternating name, value pairs: T("de", "err.price", "min", 1, "max", 9).
// A missing key falls back to English, then to the key itself, so a
// mistake is visible instead of an empty string.
func T(lang, key string, args ...any) string {
	s, ok := dicts[lang][key]
	if !ok {
		if s, ok = dicts[Default][key]; !ok {
			s = key
		}
	}
	for i := 0; i+1 < len(args); i += 2 {
		s = strings.ReplaceAll(s, "{"+fmt.Sprint(args[i])+"}", fmt.Sprint(args[i+1]))
	}
	return s
}

// Error is an error whose message the UI can show in the user's language.
// Error() returns English (for logs); Localize translates.
type Error struct {
	Key  string
	Args []any
	Err  error // optional cause, appended after a colon
}

func (e *Error) Error() string { return e.localize(Default) }
func (e *Error) Unwrap() error { return e.Err }

func (e *Error) localize(lang string) string {
	msg := T(lang, e.Key, e.Args...)
	if e.Err != nil {
		msg += ": " + Localize(e.Err, lang)
	}
	return msg
}

// E creates a translatable error.
func E(key string, args ...any) error { return &Error{Key: key, Args: args} }

// Wrap creates a translatable error with a cause.
func Wrap(err error, key string, args ...any) error {
	return &Error{Key: key, Args: args, Err: err}
}

type sentinel struct {
	err error
	key string
}

var sentinels []sentinel

// RegisterSentinel translates err (matched with errors.Is) to key. Call it
// from init; it is not safe for concurrent use with Localize.
func RegisterSentinel(err error, key string) {
	sentinels = append(sentinels, sentinel{err, key})
}

// Localize renders err for the UI in lang. Translatable errors and
// registered sentinels are translated; anything else (e.g. a raw network
// error) is shown as-is.
func Localize(err error, lang string) string {
	if err == nil {
		return ""
	}
	if e, ok := errors.AsType[*Error](err); ok {
		return e.localize(lang)
	}
	for _, s := range sentinels {
		if errors.Is(err, s.err) {
			return T(lang, s.key)
		}
	}
	return err.Error()
}

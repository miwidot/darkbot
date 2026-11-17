package i18n

import (
	"errors"
	"fmt"
	"regexp"
	"slices"
	"testing"
)

var placeholder = regexp.MustCompile(`\{[a-zA-Z]+\}`)

// TestLocalesComplete keeps every language in step with English: same keys,
// same placeholders, no empty values.
func TestLocalesComplete(t *testing.T) {
	en := dicts[Default]
	for _, lang := range Supported {
		d := dicts[lang]
		for k, v := range en {
			tv, ok := d[k]
			if !ok {
				t.Errorf("%s: missing key %q", lang, k)
				continue
			}
			if tv == "" {
				t.Errorf("%s: empty value for %q", lang, k)
			}
			want := placeholder.FindAllString(v, -1)
			got := placeholder.FindAllString(tv, -1)
			slices.Sort(want)
			slices.Sort(got)
			if !slices.Equal(want, got) {
				t.Errorf("%s: %q placeholders %v, English has %v", lang, k, got, want)
			}
		}
		for k := range d {
			if _, ok := en[k]; !ok {
				t.Errorf("%s: key %q does not exist in English", lang, k)
			}
		}
	}
}

func TestResolve(t *testing.T) {
	cases := []struct {
		pref   string
		system []string
		want   string
	}{
		{"de", []string{"en-US"}, "de"},
		{"", []string{"de-AT", "en"}, "de"},
		{"", []string{"fr-FR", "de_DE"}, "de"},
		{"", []string{"fr-FR"}, "en"},
		{"xx", nil, "en"},
		{"", []string{" EN-gb "}, "en"},
	}
	for _, c := range cases {
		if got := Resolve(c.pref, c.system); got != c.want {
			t.Errorf("Resolve(%q, %v) = %q, want %q", c.pref, c.system, got, c.want)
		}
	}
}

func TestTPlaceholdersAndFallback(t *testing.T) {
	if got := T("de", "err.price", "min", 1, "max", 9); got != "der Preis muss zwischen 1 und 9 Punkten liegen" {
		t.Errorf("T de = %q", got)
	}
	if got := T("xx", "err.chooseFile"); got != "choose a sound file" {
		t.Errorf("unknown language should fall back to English, got %q", got)
	}
	if got := T("de", "no.such.key"); got != "no.such.key" {
		t.Errorf("missing key should show the key, got %q", got)
	}
}

func TestLocalize(t *testing.T) {
	sentinel := errors.New("raw sentinel")
	RegisterSentinel(sentinel, "err.obsOffline")
	t.Cleanup(func() { sentinels = sentinels[:len(sentinels)-1] })

	err := Wrap(fmt.Errorf("helix: %w", sentinel), "err.createReward")
	if got := Localize(err, "de"); got != "Belohnung auf Twitch anlegen: nicht mit OBS verbunden" {
		t.Errorf("Localize de = %q", got)
	}
	if got := err.Error(); got != "create reward on Twitch: not connected to OBS" {
		t.Errorf("Error() should be English, got %q", got)
	}
	if !errors.Is(err, sentinel) {
		t.Error("Wrap must keep the cause for errors.Is")
	}
	if got := Localize(errors.New("dial tcp: refused"), "de"); got != "dial tcp: refused" {
		t.Errorf("unknown errors pass through, got %q", got)
	}
	if Localize(nil, "de") != "" {
		t.Error("nil error should localize to empty string")
	}
}

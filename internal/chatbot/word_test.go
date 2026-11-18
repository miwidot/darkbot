package chatbot

import (
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/miwidot/darkbot/internal/config"
	"github.com/miwidot/darkbot/internal/i18n"
	"github.com/miwidot/darkbot/internal/twitch"
)

var greet = config.ChatCommand{ID: "g", Name: "hi", Aliases: []string{"hallo"}, Trigger: config.TriggerWord,
	Response: "Hey {user}, wie geht es dir?", Permission: config.PermEveryone, OncePerStream: true, Enabled: true}

func TestWordTrigger(t *testing.T) {
	r := newRig(cmds(greet))
	for _, text := range []string{"Hi!", "HALLO, alle zusammen", "  hi @someone"} {
		m := viewer(text)
		m.ChatterID = text // another viewer each time (once per stream)
		if got := r.say(m); len(got) != 1 || got[0] != "Hey Anna, wie geht es dir?" {
			t.Fatalf("%q: got %v", text, got)
		}
	}
	for _, text := range []string{"say hi", "!hi", "hiya", "hi-five"} {
		m := viewer(text)
		m.ChatterID = "other " + text
		if got := r.say(m); len(got) != 0 {
			t.Fatalf("%q must not trigger, got %v", text, got)
		}
	}
	streamer := viewer("hi chat")
	streamer.IsBroadcaster = true
	if got := r.say(streamer); len(got) != 0 {
		t.Fatal("the streamer's own hi must not make their account answer")
	}
}

func TestOncePerStream(t *testing.T) {
	r := newRig(cmds(greet))
	if got := r.say(viewer("hi")); len(got) != 1 {
		t.Fatal("first hi is answered")
	}
	r.advance(time.Hour)
	if got := r.say(viewer("hi again")); len(got) != 0 {
		t.Fatal("the same viewer is answered once per stream")
	}
	other := viewer("hi")
	other.ChatterID = "v2"
	if got := r.say(other); len(got) != 1 {
		t.Fatal("another viewer is answered")
	}
	r.NewStream()
	if got := r.say(viewer("hi")); len(got) != 1 {
		t.Fatal("a new stream answers everyone again")
	}
}

// A "!" command and a word can share a name; song command names are only
// reserved for "!" commands.
func TestWordNamesAreSeparate(t *testing.T) {
	bang := config.ChatCommand{ID: "b", Name: "hi", Response: "bang", Permission: config.PermEveryone, Enabled: true}
	if _, err := NormalizeCommand(greet, []config.ChatCommand{bang}, []string{"sr"}); err != nil {
		t.Fatalf("word hi next to !hi: %v", err)
	}
	word := greet
	word.ID, word.Name, word.Aliases = "w", "sr", nil
	if _, err := NormalizeCommand(word, nil, []string{"sr"}); err != nil {
		t.Fatalf("the word sr is no song command: %v", err)
	}
	dup := greet
	dup.ID = "x"
	if _, err := NormalizeCommand(dup, []config.ChatCommand{greet}, nil); err == nil || !strings.Contains(i18n.Localize(err, "en"), "hi") || strings.Contains(i18n.Localize(err, "en"), "!hi") {
		t.Fatalf("a second word hi must be refused, shown without !: %v", err)
	}
	fileOnly := config.ChatCommand{Name: "gn", Trigger: config.TriggerWord, ResponseFile: "/x.txt", Permission: config.PermEveryone}
	if _, err := NormalizeCommand(fileOnly, nil, nil); err != nil {
		t.Fatalf("an answer file is an answer: %v", err)
	}
}

func writeLines(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestReadLines(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "a.txt")
	writeLines(t, p, "\xEF\xBB\xBFWie geht es dir, {user}?\r\n\r\n  Schön dich zu sehen!  \r\n\tNa du\t\n")
	lines, err := ReadLines(p)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"Wie geht es dir, {user}?", "Schön dich zu sehen!", "Na du"}
	if !slices.Equal(lines, want) {
		t.Fatalf("got %q", lines)
	}
	writeLines(t, p, "\n \n")
	if _, err := ReadLines(p); err == nil || !strings.Contains(err.Error(), i18n.T("en", "err.lineFileEmpty")) {
		t.Fatalf("empty file: %v", err)
	}
	writeLines(t, p, strings.Repeat("x", MaxLineFileBytes+1))
	if _, err := ReadLines(p); err == nil {
		t.Fatal("a file over the limit must be refused")
	}
	if _, err := ReadLines(filepath.Join(dir, "missing.txt")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("missing: %v", err)
	}
}

func TestAnswerFile(t *testing.T) {
	p := filepath.Join(t.TempDir(), "hi.txt")
	writeLines(t, p, "Eins {user}\nZwei {user}\n")
	g := greet
	g.ResponseFile, g.Response, g.OncePerStream = p, "", false
	r := newRig(cmds(g))
	r.lines.intN = func(int) int { return 0 } // always "random" index 0
	var got []string
	for range 4 {
		r.advance(time.Minute)
		got = append(got, r.say(viewer("hi"))...)
	}
	// Index 0 every time, but never the same line twice in a row.
	if !slices.Equal(got, []string{"Eins Anna", "Zwei Anna", "Eins Anna", "Zwei Anna"}) {
		t.Fatalf("got %q", got)
	}
	// Edited while darkbot runs: the new lines are used.
	writeLines(t, p, "Neu {user}, ganz neu\n")
	future := time.Now().Add(time.Minute)
	if err := os.Chtimes(p, future, future); err != nil {
		t.Fatal(err)
	}
	r.advance(time.Minute)
	if out := r.say(viewer("hi")); len(out) != 1 || out[0] != "Neu Anna, ganz neu" {
		t.Fatalf("after edit: %q", out)
	}
	// File gone: no answer (logged), and no cooldown used up.
	if err := os.Remove(p); err != nil {
		t.Fatal(err)
	}
	r.advance(time.Minute)
	if out := r.say(viewer("hi")); len(out) != 0 {
		t.Fatalf("missing file: %q", out)
	}
}

func TestFirstWord(t *testing.T) {
	for in, want := range map[string]string{"Hi!": "hi", "hallo,": "hallo", "¡Hola!": "hola", "  Moin  Leute": "moin", "👋": "", "Grüß": "grüß", "": ""} {
		if got, _ := firstWord(in); got != want {
			t.Errorf("firstWord(%q) = %q, want %q", in, got, want)
		}
	}
	m := twitch.ChatMessage{Text: "hi"}
	if _, _, ok := match(nil, m); ok {
		t.Fatal("no commands, no match")
	}
}

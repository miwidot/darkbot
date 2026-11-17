package main

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"testing"
	"time"

	"github.com/miwidot/darkbot/internal/applog"
	"github.com/miwidot/darkbot/internal/bot"
	"github.com/miwidot/darkbot/internal/config"
	"github.com/miwidot/darkbot/internal/i18n"
	"github.com/miwidot/darkbot/internal/stats"
	"github.com/miwidot/darkbot/internal/twitch"
)

func TestRenderTemplate(t *testing.T) {
	ev := twitch.ChannelEvent{Kind: twitch.EvResub, User: "Anna", Tier: "2", Months: 14, Amount: 3, Message: "{user} hacked"}
	got := renderTemplate("Danke {user} für {months} Monate (Tier {tier}, {amount})! {message}", ev, "de")
	if got != "Danke Anna für 14 Monate (Tier 2, 3)! {message}" {
		t.Fatalf("got %q (viewer text must never be inserted)", got)
	}
	anon := renderTemplate("{user} cheered", twitch.ChannelEvent{Kind: twitch.EvCheer, Anonymous: true}, "en")
	if anon != "Anonymous cheered" {
		t.Fatalf("got %q", anon)
	}
	if got := renderTemplate("Danke {top}!", twitch.ChannelEvent{Kind: twitch.EvHypeEnd, Top: "Ben"}, "de"); got != "Danke Ben!" {
		t.Fatalf("got %q", got)
	}
	if got := renderTemplate("Thanks {top}!", twitch.ChannelEvent{Kind: twitch.EvHypeEnd}, "en"); got != "Thanks everyone!" {
		t.Fatalf("got %q", got)
	}
}

// A sub bomb: one gift event, then one sub per recipient. Only the gift
// event runs an action; the sub action must not fire 50 times.
func TestSubBombRunsOneAction(t *testing.T) {
	a, out := newEventApp(t, map[string]config.EventAction{
		twitch.EvSub:  {Enabled: true, Sound: "/sub.mp3", Volume: 1},
		twitch.EvGift: {Enabled: true, Sound: "/bomb.mp3", Volume: 1},
	})
	a.handleEvent(twitch.ChannelEvent{Kind: twitch.EvGift, User: "Jonas", Amount: 50}, false)
	for range 50 {
		a.handleEvent(twitch.ChannelEvent{Kind: twitch.EvSub, User: "lucky", Gift: true}, false)
	}
	select {
	case s := <-out.played:
		if s.File != "/bomb.mp3" {
			t.Fatalf("played %s first", s.File)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("the gift event must play its sound")
	}
	select {
	case s := <-out.played:
		t.Fatalf("gift recipients must not play %s", s.File)
	case <-time.After(200 * time.Millisecond):
	}
	if s := a.st.Snapshot().Session; s.GiftedSubs != 50 || s.Subs != 0 {
		t.Fatalf("got %+v", s)
	}
}

type recordOutput struct{ played chan config.Sound }

func (r *recordOutput) Play(_ context.Context, s config.Sound, _ float64) error {
	r.played <- s
	return nil
}

func newEventApp(t *testing.T, events map[string]config.EventAction) (*App, *recordOutput) {
	t.Helper()
	cfg, err := config.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := cfg.Update(func(c *config.Config) { c.Events = events }); err != nil {
		t.Fatal(err)
	}
	log := applog.New()
	out := &recordOutput{played: make(chan config.Sound, 4)}
	a := &App{cfg: cfg, log: log, st: stats.Open(filepath.Join(t.TempDir(), "s.json"), log)}
	a.bot = bot.New(cfg, log, func() bot.Output { return out }, nil)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go a.bot.Run(ctx)
	return a, out
}

func TestEventActionThresholdAndCounting(t *testing.T) {
	a, out := newEventApp(t, map[string]config.EventAction{
		twitch.EvCheer: {Enabled: true, Sound: "/bits.mp3", Volume: 1, MinAmount: 100},
	})
	a.handleEvent(twitch.ChannelEvent{Kind: twitch.EvCheer, User: "Ben", Amount: 50}, false)
	a.handleEvent(twitch.ChannelEvent{Kind: twitch.EvCheer, User: "Ben", Amount: 500}, false)
	select {
	case s := <-out.played:
		if s.File != "/bits.mp3" || s.ID != eventSoundPrefix+twitch.EvCheer {
			t.Fatalf("played %+v", s)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("500 bits must play the sound")
	}
	select {
	case s := <-out.played:
		t.Fatalf("50 bits are below the minimum, but %+v played", s)
	case <-time.After(200 * time.Millisecond):
	}
	if got := a.st.Snapshot().Session.Bits; got != 550 {
		t.Fatalf("bits counted %d, want 550 (thresholds only limit the action)", got)
	}
}

func TestTestEventsAreNotCountedAndGiftRecipientsNotDoubled(t *testing.T) {
	a, _ := newEventApp(t, nil)
	a.handleEvent(a.sampleEvent(twitch.EvFollow), true)
	a.handleEvent(twitch.ChannelEvent{Kind: twitch.EvGift, User: "Gifter", Amount: 5}, false)
	for range 5 {
		a.handleEvent(twitch.ChannelEvent{Kind: twitch.EvSub, User: "lucky", Gift: true}, false)
	}
	a.handleEvent(twitch.ChannelEvent{Kind: twitch.EvSub, User: "own"}, false)
	s := a.st.Snapshot().Session
	if s.Follows != 0 || s.GiftedSubs != 5 || s.Subs != 1 {
		t.Fatalf("got %+v", s)
	}
}

func TestStreamOnlineStartsNewSession(t *testing.T) {
	a, _ := newEventApp(t, nil)
	a.handleEvent(twitch.ChannelEvent{Kind: twitch.EvFollow, User: "x"}, false)
	a.handleEvent(twitch.ChannelEvent{Kind: twitch.EvOnline}, false)
	snap := a.st.Snapshot()
	if snap.Session.Follows != 0 || snap.Total.Follows != 1 {
		t.Fatalf("session %+v total %+v", snap.Session, snap.Total)
	}
}

func TestServeEventSound(t *testing.T) {
	dir := t.TempDir()
	snd := filepath.Join(dir, "follow.mp3")
	if err := os.WriteFile(snd, []byte("ID3follow"), 0o600); err != nil {
		t.Fatal(err)
	}
	a, _ := newEventApp(t, map[string]config.EventAction{twitch.EvFollow: {Enabled: true, Sound: snd, Volume: 1}})
	for path, want := range map[string]int{"/sound/ev-follow": http.StatusOK, "/sound/ev-raid": http.StatusNotFound, "/sound/ev-../x": http.StatusNotFound} {
		rec := httptest.NewRecorder()
		a.serveSound(rec, httptest.NewRequest(http.MethodGet, path, nil))
		if rec.Code != want {
			t.Errorf("%s: status %d, want %d", path, rec.Code, want)
		}
		if want == http.StatusOK {
			if body, _ := io.ReadAll(rec.Body); string(body) != "ID3follow" {
				t.Errorf("body %q", body)
			}
		}
	}
}

// Follows and subs ask for fresh channel totals; one pending read is
// enough for a burst of events.
func TestChannelEventsRefreshTotals(t *testing.T) {
	a, _ := newEventApp(t, nil)
	a.ch.kick = make(chan struct{}, 1)
	a.handleEvent(twitch.ChannelEvent{Kind: twitch.EvRaid, User: "r", Amount: 3}, false)
	if len(a.ch.kick) != 0 {
		t.Fatal("a raid doesn't change followers or subs")
	}
	for range 5 {
		a.handleEvent(twitch.ChannelEvent{Kind: twitch.EvFollow, User: "f"}, false)
	}
	if len(a.ch.kick) != 1 {
		t.Fatal("follows must ask for one refresh")
	}
	<-a.ch.kick
	a.handleEvent(a.sampleEvent(twitch.EvSub), true)
	if len(a.ch.kick) != 0 {
		t.Fatal("a test event must not trigger a read")
	}
}

// Every event that can post in chat has a suggestion in every language
// (except an ended sub: announcing it would put the viewer on the spot),
// and suggestions only use placeholders darkbot fills.
func TestChatSuggestions(t *testing.T) {
	placeholder := regexp.MustCompile(`\{[a-z]+\}`)
	known := map[string]bool{"{user}": true, "{amount}": true, "{tier}": true, "{months}": true, "{top}": true}
	a, _ := newEventApp(t, nil)
	for _, lang := range i18n.Supported {
		dict := i18n.Dict(lang)
		for _, k := range a.GetEventKinds() {
			s, has := dict["evdefault."+k.Kind]
			want := k.HasChat && k.Kind != twitch.EvSubEnd
			if has != want {
				t.Errorf("%s %s: suggestion present=%v, want %v", lang, k.Kind, has, want)
				continue
			}
			for _, p := range placeholder.FindAllString(s, -1) {
				if !known[p] {
					t.Errorf("%s %s: unknown placeholder %s", lang, k.Kind, p)
				}
			}
			out := renderTemplate(s, a.sampleEvent(k.Kind), lang)
			if placeholder.MatchString(out) || len([]rune(out)) > config.MaxChatTemplate {
				t.Errorf("%s %s: rendered %q", lang, k.Kind, out)
			}
		}
	}
}

func TestSaveEventActionMakesOneLine(t *testing.T) {
	a, _ := newEventApp(t, nil)
	if err := a.SaveEventAction(twitch.EvFollow, config.EventAction{Enabled: true, Chat: "  Danke\r\n{user}!\n\nWillkommen  "}); err != nil {
		t.Fatal(err)
	}
	if got := a.cfg.Get().Events[twitch.EvFollow].Chat; got != "Danke {user}! Willkommen" {
		t.Fatalf("got %q", got)
	}
}

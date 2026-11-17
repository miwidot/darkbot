package songs

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/miwidot/darkbot/internal/config"
	"github.com/miwidot/darkbot/internal/spotify"
	"github.com/miwidot/darkbot/internal/twitch"
)

type fakePlayer struct {
	mu      sync.Mutex
	tracks  map[string]*spotify.Track
	search  *spotify.Track
	queued  []string
	skipped int
	queue   spotify.Queue
	addErr  error
}

func (f *fakePlayer) GetTrack(_ context.Context, id string) (*spotify.Track, error) {
	if t, ok := f.tracks[id]; ok {
		return t, nil
	}
	return nil, spotify.ErrNotFound
}
func (f *fakePlayer) SearchTrack(context.Context, string) (*spotify.Track, error) {
	if f.search == nil {
		return nil, spotify.ErrNotFound
	}
	return f.search, nil
}
func (f *fakePlayer) AddToQueue(_ context.Context, uri string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.addErr != nil {
		return f.addErr
	}
	f.queued = append(f.queued, uri)
	return nil
}
func (f *fakePlayer) Next(context.Context) error {
	f.mu.Lock()
	f.skipped++
	f.mu.Unlock()
	return nil
}
func (f *fakePlayer) GetQueue(context.Context) (*spotify.Queue, error) { q := f.queue; return &q, nil }

type nopLog struct{}

func (nopLog) Infof(string, ...any) {}
func (nopLog) Warnf(string, ...any) {}

type harness struct {
	h       *Handler
	p       *fakePlayer
	s       config.SongRequests
	replies []string
	acts    []Activity
	now     time.Time
}

func track(id, name string, minutes int) *spotify.Track {
	t := &spotify.Track{ID: id, URI: "spotify:track:" + id, Name: name, DurationMS: minutes * int(time.Minute/time.Millisecond)}
	t.Artists = append(t.Artists, struct {
		Name string `json:"name"`
	}{Name: "Artist"})
	return t
}

func newHarness() *harness {
	hs := &harness{
		p: &fakePlayer{tracks: map[string]*spotify.Track{
			"4uLU6hMCjMI75M1A2tKUQC": track("4uLU6hMCjMI75M1A2tKUQC", "Song", 3),
		}},
		s:   config.Default().Songs,
		now: time.Date(2026, 11, 17, 20, 0, 0, 0, time.UTC),
	}
	hs.s.Enabled = true
	hs.h = New(Deps{
		Settings:   func() config.SongRequests { return hs.s },
		Player:     hs.p,
		Connected:  func() bool { return true },
		Lang:       func() string { return "en" },
		Reply:      func(_ context.Context, text, _ string) { hs.replies = append(hs.replies, text) },
		OnActivity: func(a Activity) { hs.acts = append(hs.acts, a) },
		Log:        nopLog{},
		Now:        func() time.Time { return hs.now },
	})
	return hs
}

func viewer(text string) twitch.ChatMessage {
	return twitch.ChatMessage{ID: "m1", ChatterID: "42", ChatterLogin: "viewer", ChatterName: "Viewer", Text: text}
}

func (hs *harness) last() string {
	if len(hs.replies) == 0 {
		return ""
	}
	return hs.replies[len(hs.replies)-1]
}

func TestRequestByLinkQueuesAndReplies(t *testing.T) {
	hs := newHarness()
	hs.h.Handle(context.Background(), viewer("!sr https://open.spotify.com/intl-de/track/4uLU6hMCjMI75M1A2tKUQC?si=abc"))
	if len(hs.p.queued) != 1 || hs.p.queued[0] != "spotify:track:4uLU6hMCjMI75M1A2tKUQC" {
		t.Fatalf("queued %v", hs.p.queued)
	}
	if hs.last() != "Added to the queue: Artist – Song" {
		t.Fatalf("reply %q", hs.last())
	}
	if len(hs.acts) != 1 || hs.acts[0].Kind != ActAdded {
		t.Fatalf("activity %+v", hs.acts)
	}
}

func TestDisabledIgnoresEverything(t *testing.T) {
	hs := newHarness()
	hs.s.Enabled = false
	hs.h.Handle(context.Background(), viewer("!sr song"))
	if len(hs.replies) != 0 || len(hs.p.queued) != 0 {
		t.Fatal("disabled song requests must stay silent")
	}
}

func TestUserCooldownAndExemptions(t *testing.T) {
	hs := newHarness()
	hs.p.search = track("abc", "Searched", 3)
	hs.h.Handle(context.Background(), viewer("!sr first"))
	hs.h.Handle(context.Background(), viewer("!sr second"))
	if len(hs.p.queued) != 1 || !strings.Contains(hs.last(), "wait 60 s") {
		t.Fatalf("second request must hit the cooldown, queued %v reply %q", hs.p.queued, hs.last())
	}
	hs.now = hs.now.Add(61 * time.Second)
	hs.h.Handle(context.Background(), viewer("!sr third"))
	if len(hs.p.queued) != 2 {
		t.Fatal("cooldown should have expired")
	}
	mod := viewer("!sr mod")
	mod.IsModerator = true
	hs.h.Handle(context.Background(), mod)
	hs.h.Handle(context.Background(), mod)
	if len(hs.p.queued) != 4 {
		t.Fatalf("moderators have no cooldown, queued %d", len(hs.p.queued))
	}
}

func TestFailedRequestKeepsNoCooldown(t *testing.T) {
	hs := newHarness()
	hs.h.Handle(context.Background(), viewer("!sr nothing matches"))
	if hs.last() != "no song found" {
		t.Fatalf("reply %q", hs.last())
	}
	hs.p.search = track("abc", "Found", 3)
	hs.h.Handle(context.Background(), viewer("!sr better words"))
	if len(hs.p.queued) != 1 {
		t.Fatal("a failed request must not start the cooldown")
	}
}

func TestRules(t *testing.T) {
	hs := newHarness()
	long := track("long", "Mix", 30)
	hs.p.search = long
	hs.h.Handle(context.Background(), viewer("!sr mix"))
	if hs.last() != "Songs can be at most 10 minutes long." {
		t.Fatalf("reply %q", hs.last())
	}
	hs.s.BlockExplicit = true
	hs.s.MaxMinutes = 0
	long.Explicit = true
	hs.h.Handle(context.Background(), viewer("!sr mix"))
	if hs.last() != "Explicit songs aren’t allowed." {
		t.Fatalf("reply %q", hs.last())
	}
	hs.h.Handle(context.Background(), viewer("!sr https://youtu.be/dQw4w9WgXcQ"))
	if hs.last() != "Only Spotify links or song names work here." {
		t.Fatalf("reply %q", hs.last())
	}
	hs.h.Handle(context.Background(), viewer("!sr https://open.spotify.com/playlist/37i9dQZF1DXcBWIGoYBM5M"))
	if hs.last() != "only links to single songs work, not albums or playlists" {
		t.Fatalf("reply %q", hs.last())
	}
	if len(hs.p.queued) != 0 {
		t.Fatal("nothing should be queued")
	}
}

func TestPlayerErrorsAreTranslated(t *testing.T) {
	hs := newHarness()
	hs.p.search = track("abc", "Song", 3)
	hs.p.addErr = errors.Join(spotify.ErrNoDevice)
	hs.h.Handle(context.Background(), viewer("!sr song"))
	if hs.last() != "Spotify isn’t playing on any device, start playback in Spotify first" {
		t.Fatalf("reply %q", hs.last())
	}
	hs.p.addErr = errors.New("dial tcp: connection refused")
	hs.h.Handle(context.Background(), viewer("!sr song"))
	if hs.last() != "Something went wrong, please try again later." {
		t.Fatalf("unexpected errors must not leak details into chat, got %q", hs.last())
	}
}

func TestSkipPermissions(t *testing.T) {
	hs := newHarness()
	hs.p.search = track("mine", "Mine", 3)
	hs.h.Handle(context.Background(), viewer("!sr mine"))
	hs.p.queue.Current = hs.p.search

	other := twitch.ChatMessage{ID: "m2", ChatterID: "7", ChatterName: "Other", Text: "!skip"}
	hs.h.Handle(context.Background(), other)
	if hs.p.skipped != 0 || hs.last() != "You can’t skip this song." {
		t.Fatalf("other viewers must not skip, reply %q", hs.last())
	}
	hs.h.Handle(context.Background(), viewer("!skip"))
	if hs.p.skipped != 1 {
		t.Fatal("the requester may skip their own song")
	}
	hs.s.RequesterCanSkip = false
	hs.h.Handle(context.Background(), viewer("!skip"))
	if hs.p.skipped != 1 {
		t.Fatal("requester skip is off")
	}
	other.IsBroadcaster = true
	hs.h.Handle(context.Background(), other)
	if hs.p.skipped != 2 {
		t.Fatal("the broadcaster can always skip")
	}
}

func TestInfoCommands(t *testing.T) {
	hs := newHarness()
	hs.p.search = track("req", "Wish", 3)
	hs.h.Handle(context.Background(), viewer("!sr wish"))
	hs.p.queue = spotify.Queue{Current: track("now", "Now", 3), Next: []spotify.Track{*hs.p.search, *track("b", "B", 3)}}

	hs.h.Handle(context.Background(), viewer("!song"))
	if hs.last() != "Now playing: Artist – Now" {
		t.Fatalf("reply %q", hs.last())
	}
	hs.h.Handle(context.Background(), viewer("!next"))
	if hs.last() != "Up next: Artist – Wish (requested by Viewer)" {
		t.Fatalf("reply %q", hs.last())
	}
	hs.h.Handle(context.Background(), viewer("!queue"))
	if hs.last() != "Coming up: 1. Artist – Wish (requested by Viewer) | 2. Artist – B" {
		t.Fatalf("reply %q", hs.last())
	}
	n := len(hs.replies)
	hs.h.Handle(context.Background(), viewer("!queue"))
	if len(hs.replies) != n {
		t.Fatal("info commands answer at most once per infoCooldown")
	}
}

func TestIsCommand(t *testing.T) {
	for text, want := range map[string]bool{"!sr x": true, "!SR x": true, "  !skip": true, "!song": true, "!songs": false, "hello !sr": false, "": false} {
		if IsCommand(text) != want {
			t.Errorf("IsCommand(%q) != %v", text, want)
		}
	}
}

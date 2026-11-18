package songs

import (
	"context"
	"errors"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/miwidot/darkbot/internal/config"
	"github.com/miwidot/darkbot/internal/spotify"
	"github.com/miwidot/darkbot/internal/twitch"
	"github.com/miwidot/darkbot/internal/ytmusic"
)

type fakePlayer struct {
	service string
	name    string
	parse   func(string) (string, bool, error)
	conn    bool
	playing bool

	mu      sync.Mutex
	tracks  map[string]*Track // by link ID
	search  *Track
	queued  []string
	skipped int
	queue   Queue
	addErr  error
	now     *Now
	nowErr  error
}

func (f *fakePlayer) Now(context.Context) (*Now, error) {
	if f.now == nil {
		return nil, f.nowErr
	}
	n := *f.now
	return &n, f.nowErr
}

func (f *fakePlayer) Service() string                          { return f.service }
func (f *fakePlayer) Name() string                             { return f.name }
func (f *fakePlayer) Connected() bool                          { return f.conn }
func (f *fakePlayer) ParseLink(s string) (string, bool, error) { return f.parse(s) }
func (f *fakePlayer) Playing(context.Context) (bool, error)    { return f.playing, nil }

func (f *fakePlayer) Track(_ context.Context, id string) (*Track, error) {
	if t, ok := f.tracks[id]; ok {
		return t, nil
	}
	return nil, spotify.ErrNotFound
}
func (f *fakePlayer) Search(context.Context, string) (*Track, error) {
	if f.search == nil {
		return nil, spotify.ErrNotFound
	}
	return f.search, nil
}
func (f *fakePlayer) Add(_ context.Context, t Track) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.addErr != nil {
		return f.addErr
	}
	f.queued = append(f.queued, t.ID)
	f.queue.Next = append(f.queue.Next, t)
	return nil
}
func (f *fakePlayer) Skip(context.Context) error {
	f.mu.Lock()
	f.skipped++
	f.mu.Unlock()
	return nil
}
func (f *fakePlayer) Queue(context.Context) (*Queue, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	q := f.queue
	return &q, nil
}

type nopLog struct{}

func (nopLog) Infof(string, ...any) {}
func (nopLog) Warnf(string, ...any) {}

type harness struct {
	h       *Handler
	p       *fakePlayer // Spotify, connected
	y       *fakePlayer // YouTube Music, off unless a test turns it on
	s       config.SongRequests
	replies []string
	acts    []Activity
	now     time.Time
}

// track is a Spotify song as the adapter returns it (ID = URI).
func track(id, name string, minutes int) *Track {
	return &Track{ID: "spotify:track:" + id, Title: "Artist – " + name, Duration: time.Duration(minutes) * time.Minute}
}

// ytTrack is a YouTube Music song (ID = video ID).
func ytTrack(id, name string) *Track {
	return &Track{ID: id, Title: "YT Artist – " + name, Duration: 3 * time.Minute}
}

func newHarness() *harness {
	hs := &harness{
		p: &fakePlayer{service: config.ServiceSpotify, name: "Spotify", parse: spotify.ParseLink, conn: true, tracks: map[string]*Track{
			"4uLU6hMCjMI75M1A2tKUQC": track("4uLU6hMCjMI75M1A2tKUQC", "Song", 3),
		}},
		y: &fakePlayer{service: config.ServiceYTMusic, name: "YouTube Music", parse: ytmusic.ParseLink, tracks: map[string]*Track{
			"4D7u5KF7SP8": ytTrack("4D7u5KF7SP8", "Get Lucky"),
		}},
		s:   config.Default().Songs,
		now: time.Date(2026, 11, 17, 20, 0, 0, 0, time.UTC),
	}
	hs.s.Enabled = true
	hs.h = New(Deps{
		Settings:   func() config.SongRequests { return hs.s },
		Players:    []Player{hs.p, hs.y},
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

// say handles m a little later than the previous message, past the spam gate.
func (hs *harness) say(m twitch.ChatMessage) {
	hs.now = hs.now.Add(commandGap + time.Second)
	hs.h.Handle(context.Background(), m)
}

func (hs *harness) last() string {
	if len(hs.replies) == 0 {
		return ""
	}
	return hs.replies[len(hs.replies)-1]
}

func TestRequestByLinkQueuesAndReplies(t *testing.T) {
	hs := newHarness()
	hs.say(viewer("!sr https://open.spotify.com/intl-de/track/4uLU6hMCjMI75M1A2tKUQC?si=abc"))
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
	hs.say(viewer("!sr song"))
	if len(hs.replies) != 0 || len(hs.p.queued) != 0 {
		t.Fatal("disabled song requests must stay silent")
	}
}

func TestUserCooldownAndExemptions(t *testing.T) {
	hs := newHarness()
	hs.p.search = track("a", "A", 3)
	hs.say(viewer("!sr first"))
	hs.p.search = track("b", "B", 3)
	hs.say(viewer("!sr second"))
	if len(hs.p.queued) != 1 || !strings.HasPrefix(hs.last(), "Please wait") {
		t.Fatalf("second request must hit the cooldown, queued %v reply %q", hs.p.queued, hs.last())
	}
	hs.now = hs.now.Add(time.Minute)
	hs.say(viewer("!sr third"))
	if len(hs.p.queued) != 2 {
		t.Fatal("cooldown should have expired")
	}
	mod := viewer("!sr mod")
	mod.IsModerator = true
	hs.p.search = track("c", "C", 3)
	hs.h.Handle(context.Background(), mod)
	hs.p.search = track("d", "D", 3)
	hs.h.Handle(context.Background(), mod)
	if len(hs.p.queued) != 4 {
		t.Fatalf("moderators have no cooldown and no spam gate, queued %d", len(hs.p.queued))
	}
}

func TestFailedRequestGetsShortCooldown(t *testing.T) {
	hs := newHarness()
	hs.say(viewer("!sr nothing matches"))
	if hs.last() != "no song found" {
		t.Fatalf("reply %q", hs.last())
	}
	hs.p.search = track("abc", "Found", 3)
	hs.say(viewer("!sr better words")) // 4 s later: still inside failCooldown
	if len(hs.p.queued) != 0 || !strings.HasPrefix(hs.last(), "Please wait") {
		t.Fatalf("retry right away must wait, reply %q", hs.last())
	}
	hs.now = hs.now.Add(failCooldown)
	hs.say(viewer("!sr better words"))
	if len(hs.p.queued) != 1 {
		t.Fatal("after failCooldown a refused request may be retried, not only after the full cooldown")
	}
}

func TestSpamGate(t *testing.T) {
	hs := newHarness()
	hs.say(viewer("!sr"))
	n := len(hs.replies)
	hs.h.Handle(context.Background(), viewer("!sr")) // same instant
	if len(hs.replies) != n {
		t.Fatal("a viewer's second command within commandGap must be ignored")
	}
}

func TestReplyCap(t *testing.T) {
	hs := newHarness()
	for i := range maxReplies + 5 {
		m := viewer("!sr")
		m.ChatterID = string(rune('a' + i)) // many different viewers
		hs.h.Handle(context.Background(), m)
	}
	if len(hs.replies) != maxReplies {
		t.Fatalf("%d replies, want the cap %d", len(hs.replies), maxReplies)
	}
	hs.now = hs.now.Add(replyWindow)
	hs.say(viewer("!sr"))
	if len(hs.replies) != maxReplies+1 {
		t.Fatal("replies resume after the window")
	}
}

func TestRules(t *testing.T) {
	hs := newHarness()
	hs.s.UserCooldownSeconds = 0
	long := track("long", "Mix", 30)
	hs.p.search = long
	hs.say(viewer("!sr mix"))
	if hs.last() != "Songs can be at most 10 minutes long." {
		t.Fatalf("reply %q", hs.last())
	}
	hs.s.BlockExplicit = true
	hs.s.MaxMinutes = 0
	long.Explicit = true
	hs.say(viewer("!sr mix"))
	if hs.last() != "Explicit songs aren’t allowed." {
		t.Fatalf("reply %q", hs.last())
	}
	hs.say(viewer("!sr https://youtu.be/dQw4w9WgXcQ"))
	if hs.last() != "YouTube Music songs don’t work here right now." {
		t.Fatalf("YouTube is off: reply %q", hs.last())
	}
	hs.say(viewer("!sr https://soundcloud.com/artist/song"))
	if hs.last() != "This kind of link doesn’t work here. Use a song link or the song’s name." {
		t.Fatalf("reply %q", hs.last())
	}
	hs.say(viewer("!sr https://open.spotify.com/playlist/37i9dQZF1DXcBWIGoYBM5M"))
	if hs.last() != "only links to single songs work, not albums or playlists" {
		t.Fatalf("reply %q", hs.last())
	}
	if len(hs.p.queued) != 0 {
		t.Fatal("nothing should be queued")
	}
}

func TestDuplicateRefused(t *testing.T) {
	hs := newHarness()
	hs.s.UserCooldownSeconds = 0
	hs.p.search = track("dup", "Dup", 3)
	hs.say(viewer("!sr dup"))
	hs.say(viewer("!sr dup"))
	if len(hs.p.queued) != 1 || hs.last() != "This song is already playing or in the queue." {
		t.Fatalf("queued %v reply %q", hs.p.queued, hs.last())
	}
}

func TestPlayerErrorsAreTranslated(t *testing.T) {
	hs := newHarness()
	hs.s.UserCooldownSeconds = 0
	hs.p.search = track("abc", "Song", 3)
	hs.p.addErr = errors.Join(spotify.ErrNoDevice)
	hs.say(viewer("!sr song"))
	if hs.last() != "Spotify isn’t playing on any device, start playback in Spotify first" {
		t.Fatalf("reply %q", hs.last())
	}
	hs.p.addErr = errors.New("dial tcp: connection refused")
	hs.say(viewer("!sr song"))
	if hs.last() != "Something went wrong, please try again later." {
		t.Fatalf("unexpected errors must not leak details into chat, got %q", hs.last())
	}
}

func TestSkipPermissions(t *testing.T) {
	hs := newHarness()
	hs.p.search = track("mine", "Mine", 3)
	hs.say(viewer("!sr mine"))
	hs.p.queue = Queue{Current: hs.p.search}

	other := twitch.ChatMessage{ID: "m2", ChatterID: "7", ChatterName: "Viewer", Text: "!skip"} // same display name, other account
	hs.say(other)
	if hs.p.skipped != 0 || hs.last() != "You can’t skip this song." {
		t.Fatalf("only the requesting account may skip, reply %q", hs.last())
	}
	hs.say(viewer("!skip"))
	if hs.p.skipped != 1 {
		t.Fatal("the requester may skip their own song")
	}
	hs.s.RequesterCanSkip = false
	hs.say(viewer("!skip"))
	if hs.p.skipped != 1 {
		t.Fatal("requester skip is off")
	}
	other.IsBroadcaster = true
	hs.say(other)
	if hs.p.skipped != 2 {
		t.Fatal("the broadcaster can always skip")
	}
}

func TestCannotClaimStreamersSong(t *testing.T) {
	hs := newHarness()
	playing := track("streamer", "Streamer pick", 3)
	hs.p.queue = Queue{Current: playing}
	hs.p.tracks[playing.ID] = playing
	hs.say(viewer("!sr spotify:track:streamer"))
	hs.say(viewer("!skip"))
	if hs.p.skipped != 0 {
		t.Fatal("requesting the song that already plays must not grant the right to skip it")
	}
}

func TestInfoCommands(t *testing.T) {
	hs := newHarness()
	hs.p.search = track("req", "Wish", 3)
	hs.say(viewer("!sr wish"))
	hs.p.queue = Queue{Current: track("now", "Now", 3), Next: []Track{*hs.p.search, *track("b", "B", 3)}}

	hs.say(viewer("!song"))
	if hs.last() != "Now playing: Artist – Now" {
		t.Fatalf("reply %q", hs.last())
	}
	hs.say(viewer("!next"))
	if hs.last() != "Up next: Artist – Wish (requested by Viewer)" {
		t.Fatalf("reply %q", hs.last())
	}
	hs.say(viewer("!queue"))
	if hs.last() != "Coming up: 1. Artist – Wish (requested by Viewer) | 2. Artist – B" {
		t.Fatalf("reply %q", hs.last())
	}
	n := len(hs.replies)
	other := viewer("!queue")
	other.ChatterID = "99"
	hs.now = hs.now.Add(time.Second)
	hs.h.Handle(context.Background(), other)
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

// Many accounts at once (a bot wave) can't flood the queue: the
// whole channel gets maxChannelRequests per window; mods are not limited.
func TestChannelRequestLimit(t *testing.T) {
	hs := newHarness()
	hs.s.UserCooldownSeconds = 0
	for i := range maxChannelRequests + 2 {
		m := viewer("!sr https://open.spotify.com/track/4uLU6hMCjMI75M1A2tKUQC")
		m.ChatterID = "bot" + strconv.Itoa(i)
		hs.h.Handle(context.Background(), m)
	}
	if hs.last() != "Too many song requests right now, please try again in a minute." {
		t.Fatalf("reply %q", hs.last())
	}
	mod := viewer("!sr https://open.spotify.com/track/4uLU6hMCjMI75M1A2tKUQC")
	mod.ChatterID, mod.IsModerator = "mod", true
	hs.h.Handle(context.Background(), mod)
	if strings.HasPrefix(hs.last(), "Too many") {
		t.Fatal("mods are not limited")
	}
	hs.now = hs.now.Add(channelRequestWindow)
	hs.say(viewer("!sr https://open.spotify.com/track/4uLU6hMCjMI75M1A2tKUQC"))
	if strings.HasPrefix(hs.last(), "Too many") {
		t.Fatal("the limit must end after the window")
	}
}

// With titles hidden, no song title reaches chat (it is posted as the
// streamer, unfiltered); the feed still shows it to the streamer.
func TestHideTitles(t *testing.T) {
	hs := newHarness()
	hs.s.HideTitles = true
	hs.say(viewer("!sr https://open.spotify.com/track/4uLU6hMCjMI75M1A2tKUQC"))
	if hs.last() != "Added your song to the queue." {
		t.Fatalf("reply %q", hs.last())
	}
	if len(hs.acts) != 1 || hs.acts[0].Title != "Artist – Song" {
		t.Fatalf("the feed must keep the title: %+v", hs.acts)
	}
	for _, cmd := range []string{"!song", "!next", "!queue"} {
		hs.say(viewer(cmd))
		if strings.Contains(hs.last(), "Song") && hs.last() != "Song titles aren’t posted in chat here." {
			t.Fatalf("%s posted a title: %q", cmd, hs.last())
		}
	}
	for _, r := range hs.replies {
		if strings.Contains(r, "Artist") {
			t.Fatalf("a title reached chat: %q", r)
		}
	}
}

// Links go to the service they belong to, whatever the search service is.
func TestLinksPickTheirService(t *testing.T) {
	hs := newHarness()
	hs.s.UserCooldownSeconds = 0
	hs.y.conn = true
	hs.say(viewer("!sr https://music.youtube.com/watch?v=4D7u5KF7SP8&si=x"))
	if len(hs.y.queued) != 1 || hs.y.queued[0] != "4D7u5KF7SP8" || len(hs.p.queued) != 0 {
		t.Fatalf("yt %v spotify %v", hs.y.queued, hs.p.queued)
	}
	if hs.last() != "Added to the queue: YT Artist – Get Lucky" {
		t.Fatalf("reply %q", hs.last())
	}
	hs.say(viewer("!sr https://open.spotify.com/track/4uLU6hMCjMI75M1A2tKUQC"))
	if len(hs.p.queued) != 1 || len(hs.y.queued) != 1 {
		t.Fatalf("yt %v spotify %v", hs.y.queued, hs.p.queued)
	}
	hs.say(viewer("!sr https://music.youtube.com/playlist?list=PL1"))
	if hs.last() != "only links to single songs work, not playlists or channels" {
		t.Fatalf("reply %q", hs.last())
	}
}

// A search goes to the chosen service, or to the other one while the
// chosen one is off.
func TestSearchService(t *testing.T) {
	hs := newHarness()
	hs.s.UserCooldownSeconds = 0
	hs.p.search = track("s", "Spot", 3)
	hs.y.search = ytTrack("yyyyyyyyyyy", "Tube")
	hs.y.conn = true
	hs.s.SearchService = config.ServiceYTMusic
	hs.say(viewer("!sr tube"))
	if len(hs.y.queued) != 1 || len(hs.p.queued) != 0 {
		t.Fatalf("search must use YouTube Music: yt %v spotify %v", hs.y.queued, hs.p.queued)
	}
	hs.y.conn = false
	hs.say(viewer("!sr spot"))
	if len(hs.p.queued) != 1 {
		t.Fatalf("with YouTube Music off, search falls back to Spotify: %v", hs.p.queued)
	}
	hs.p.conn = false
	hs.say(viewer("!sr spot"))
	if hs.last() != "Song requests aren’t available right now." {
		t.Fatalf("reply %q", hs.last())
	}
}

// !song, !skip etc. talk about the service that plays right now.
func TestInfoFollowsThePlayingService(t *testing.T) {
	hs := newHarness()
	hs.y.conn = true
	hs.p.queue = Queue{Current: track("sp", "Spotify Song", 3)}
	hs.y.queue = Queue{Current: ytTrack("cur", "Tube Song")}
	hs.say(viewer("!song"))
	if hs.last() != "Now playing: Artist – Spotify Song" {
		t.Fatalf("nothing plays: the search service (Spotify) answers, got %q", hs.last())
	}
	hs.y.playing = true
	hs.now = hs.now.Add(infoCooldown)
	hs.say(viewer("!song"))
	if hs.last() != "Now playing: YT Artist – Tube Song" {
		t.Fatalf("reply %q", hs.last())
	}
	mod := viewer("!skip")
	mod.IsModerator = true
	hs.say(mod)
	if hs.y.skipped != 1 || hs.p.skipped != 0 {
		t.Fatalf("skip must hit the playing service: yt %d spotify %d", hs.y.skipped, hs.p.skipped)
	}
}

// Requester rights are per service: the same ID on another service is a
// different song.
func TestRequesterSkipIsPerService(t *testing.T) {
	hs := newHarness()
	hs.y.conn = true
	hs.y.search = &Track{ID: "same", Title: "YT – Same", Duration: time.Minute}
	hs.s.SearchService = config.ServiceYTMusic
	hs.say(viewer("!sr same"))
	hs.p.queue = Queue{Current: &Track{ID: "same", Title: "Sp – Same"}}
	hs.say(viewer("!skip"))
	if hs.p.skipped != 0 {
		t.Fatal("a YouTube request must not allow skipping a Spotify song")
	}
}

// The overlay shows the playing service's song, with who requested it.
func TestNowPlaying(t *testing.T) {
	hs := newHarness()
	hs.y.conn = true
	ctx := context.Background()
	if n, err := hs.h.NowPlaying(ctx); n != nil || err != nil {
		t.Fatalf("nothing loaded: %+v %v", n, err)
	}
	hs.p.now = &Now{ID: "spotify:track:a", Title: "Paused", Playing: false}
	hs.y.now = &Now{ID: "4D7u5KF7SP8", Title: "Get Lucky", Artist: "Daft Punk", Playing: true}
	hs.say(viewer("!sr https://music.youtube.com/watch?v=4D7u5KF7SP8"))
	n, err := hs.h.NowPlaying(ctx)
	if err != nil || n == nil || n.Title != "Get Lucky" || n.Service != config.ServiceYTMusic || n.ServiceName != "YouTube Music" || n.RequestedBy != "Viewer" {
		t.Fatalf("the playing one with its requester: %+v %v", n, err)
	}
	hs.y.now = nil
	if n, _ := hs.h.NowPlaying(ctx); n == nil || n.Title != "Paused" || n.RequestedBy != "" {
		t.Fatalf("else the paused one: %+v", n)
	}
	// One service failing doesn't hide the other's song; with no song at
	// all, the failure is reported.
	hs.y.nowErr = errors.New("pear closed")
	if n, err := hs.h.NowPlaying(ctx); n == nil || err != nil {
		t.Fatalf("spotify's song despite pear: %+v %v", n, err)
	}
	hs.p.now = nil
	if _, err := hs.h.NowPlaying(ctx); err == nil {
		t.Fatal("no song and a failure: report it")
	}
}

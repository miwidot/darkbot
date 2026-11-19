package clips

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/miwidot/darkbot/internal/config"
	"github.com/miwidot/darkbot/internal/twitch"
)

type fakeAPI struct {
	mu        sync.Mutex
	titles    []string // titles Create Clip was called with
	seconds   []int
	createErr func(title string) error
	listAfter int // Get Clips lists the clip from this call on (0 = never)
	gets      int
	getErr    error
	clipURL   string
	block     chan struct{} // Create Clip waits for it when set
}

func (f *fakeAPI) CreateClip(_ context.Context, _, title string, seconds int) (twitch.Clip, error) {
	if f.block != nil {
		<-f.block
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.titles = append(f.titles, title)
	f.seconds = append(f.seconds, seconds)
	if f.createErr != nil {
		if err := f.createErr(title); err != nil {
			return twitch.Clip{}, err
		}
	}
	return twitch.Clip{ID: "AwkwardHelplessSalamanderSwiftRage"}, nil
}

func (f *fakeAPI) GetClip(_ context.Context, id string) (twitch.Clip, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.gets++
	if f.getErr != nil {
		return twitch.Clip{}, false, f.getErr
	}
	if f.listAfter == 0 || f.gets < f.listAfter {
		return twitch.Clip{}, false, nil
	}
	return twitch.Clip{ID: id, URL: f.clipURL, Title: "Best moment"}, true, nil
}

type nopLog struct{}

func (nopLog) Infof(string, ...any) {}
func (nopLog) Warnf(string, ...any) {}

type harness struct {
	h       *Handler
	api     *fakeAPI
	s       config.Clips
	canClip bool
	now     time.Time
	mu      sync.Mutex
	replies []string
	acts    []Activity
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	oldP, oldC := pollInterval, confirmTimeout
	pollInterval, confirmTimeout = time.Millisecond, 200*time.Millisecond
	t.Cleanup(func() { pollInterval, confirmTimeout = oldP, oldC })
	x := &harness{api: &fakeAPI{listAfter: 2, clipURL: "https://clips.twitch.tv/AwkwardHelplessSalamanderSwiftRage"}, canClip: true,
		now: time.Date(2026, 11, 19, 20, 0, 0, 0, time.UTC)}
	x.s = config.DefaultClips()
	x.s.Enabled = true
	x.h = New(Deps{
		Settings: func() config.Clips { return x.s },
		API:      x.api,
		CanClip:  func() bool { return x.canClip },
		Reply: func(_ context.Context, text, _ string) {
			x.mu.Lock()
			x.replies = append(x.replies, text)
			x.mu.Unlock()
		},
		Lang:       func() string { return "en" },
		OnActivity: func(a Activity) { x.acts = append(x.acts, a) },
		Log:        nopLog{},
		Now:        func() time.Time { return x.now },
	})
	return x
}

func viewer(text string) twitch.ChatMessage {
	return twitch.ChatMessage{ID: "m1", BroadcasterID: "42", ChatterID: "7", ChatterName: "Anna", Text: text}
}

func (x *harness) last() string {
	x.mu.Lock()
	defer x.mu.Unlock()
	if len(x.replies) == 0 {
		return ""
	}
	return x.replies[len(x.replies)-1]
}

func TestClipPostsLink(t *testing.T) {
	x := newHarness(t)
	x.h.Handle(context.Background(), viewer("!Clips please"))
	if got := x.last(); got != "Clip by Anna: https://clips.twitch.tv/AwkwardHelplessSalamanderSwiftRage" {
		t.Fatalf("reply %q", got)
	}
	if len(x.api.seconds) != 1 || x.api.seconds[0] != 60 || x.api.titles[0] != "" {
		t.Fatalf("create calls: %v %v", x.api.seconds, x.api.titles)
	}
	if len(x.acts) != 1 || x.acts[0].Kind != ActClip || x.acts[0].URL == "" || x.acts[0].User != "Anna" {
		t.Fatalf("activity %+v", x.acts)
	}
}

func TestClipIgnores(t *testing.T) {
	x := newHarness(t)
	x.h.Handle(context.Background(), viewer("!clipsy"))
	x.h.Handle(context.Background(), viewer("clips"))
	x.s.Permission = config.PermSubscriber
	x.h.Handle(context.Background(), viewer("!clips"))
	x.s.Permission, x.s.Enabled = config.PermEveryone, false
	x.h.Handle(context.Background(), viewer("!clips"))
	if len(x.api.titles) != 0 || len(x.replies) != 0 {
		t.Fatalf("calls %v replies %v", x.api.titles, x.replies)
	}
}

func TestClipCooldown(t *testing.T) {
	x := newHarness(t)
	x.h.Handle(context.Background(), viewer("!clips"))
	x.now = x.now.Add(30 * time.Second)
	x.h.Handle(context.Background(), viewer("!clips")) // cooldown: silent
	if len(x.api.titles) != 1 || len(x.replies) != 1 {
		t.Fatalf("cooldown not kept: %v %v", x.api.titles, x.replies)
	}
	mod := viewer("!clips")
	mod.IsModerator = true
	x.h.Handle(context.Background(), mod) // moderators skip it (and restart it)
	x.now = x.now.Add(59 * time.Second)
	x.h.Handle(context.Background(), viewer("!clips"))
	if len(x.api.titles) != 2 {
		t.Fatalf("clips made: %d, want 2", len(x.api.titles))
	}
	x.now = x.now.Add(time.Second)
	x.h.Handle(context.Background(), viewer("!clips"))
	if len(x.api.titles) != 3 {
		t.Fatalf("clips made: %d, want 3", len(x.api.titles))
	}
}

func TestClipOneAtATime(t *testing.T) {
	x := newHarness(t)
	x.api.block = make(chan struct{})
	mod := viewer("!clips")
	mod.IsModerator = true
	done := make(chan struct{})
	go func() { x.h.Handle(context.Background(), mod); close(done) }()
	// Wait until the first one holds the slot.
	for {
		x.h.mu.Lock()
		busy := x.h.busy
		x.h.mu.Unlock()
		if busy {
			break
		}
		time.Sleep(time.Millisecond)
	}
	x.h.Handle(context.Background(), mod) // busy: dropped, even for a mod
	close(x.api.block)
	<-done
	if len(x.api.titles) != 1 {
		t.Fatalf("clips made: %d, want 1", len(x.api.titles))
	}
}

func TestClipTitleRetry(t *testing.T) {
	x := newHarness(t)
	x.s.Title = "Clip by {user}"
	x.api.createErr = func(title string) error {
		if title != "" {
			return twitch.ErrClipNotClippable // AutoMod refused the title
		}
		return nil
	}
	x.h.Handle(context.Background(), viewer("!clips"))
	if strings.Join(x.api.titles, "|") != "Clip by Anna|" || !strings.HasPrefix(x.last(), "Clip by Anna: https://") {
		t.Fatalf("titles %q reply %q", x.api.titles, x.last())
	}
}

func TestClipErrors(t *testing.T) {
	for _, tt := range []struct {
		name string
		err  error
		want string
	}{
		{"offline", twitch.ErrClipOffline, "Clips only work while the stream is live."},
		{"category", twitch.ErrClipNotClippable, "This category can't be clipped."},
		{"restricted", errors.Join(twitch.ErrClipNotAllowed, errors.New("403")), "Clips are turned off or limited in this channel right now."},
		{"network", errors.New("dial tcp: timeout"), "The clip didn't work, try again in a moment."},
	} {
		t.Run(tt.name, func(t *testing.T) {
			x := newHarness(t)
			x.api.createErr = func(string) error { return tt.err }
			x.h.Handle(context.Background(), viewer("!clips"))
			if x.last() != tt.want || len(x.acts) != 0 {
				t.Fatalf("reply %q acts %v", x.last(), x.acts)
			}
		})
	}
}

func TestClipNeverListed(t *testing.T) {
	x := newHarness(t)
	x.api.listAfter = 0
	x.h.Handle(context.Background(), viewer("!clips"))
	if x.last() != "The clip didn't work, try again in a moment." || x.api.gets < 2 {
		t.Fatalf("reply %q after %d checks", x.last(), x.api.gets)
	}
	x = newHarness(t)
	x.api.getErr = errors.New("503")
	x.h.Handle(context.Background(), viewer("!clips"))
	if x.last() != "The clip didn't work, try again in a moment." {
		t.Fatalf("reply %q", x.last())
	}
}

func TestClipNeedsScope(t *testing.T) {
	x := newHarness(t)
	x.canClip = false
	x.h.Handle(context.Background(), viewer("!clips"))
	if x.last() != "Clips aren't available right now." || len(x.api.titles) != 0 {
		t.Fatalf("reply %q calls %v", x.last(), x.api.titles)
	}
}

func TestLink(t *testing.T) {
	id := "AwkwardHelplessSalamanderSwiftRage"
	for raw, want := range map[string]string{
		"https://clips.twitch.tv/" + id:        "https://clips.twitch.tv/" + id,
		"https://www.twitch.tv/dev/clip/" + id: "https://www.twitch.tv/dev/clip/" + id,
		"":                                     "https://clips.twitch.tv/" + id,
		"http://clips.twitch.tv/" + id:         "https://clips.twitch.tv/" + id,
		"https://evil.example/" + id:           "https://clips.twitch.tv/" + id,
		"https://twitch.tv.evil.example/" + id: "https://clips.twitch.tv/" + id,
		"javascript:alert(1)":                  "https://clips.twitch.tv/" + id,
	} {
		if got := Link(raw, id); got != want {
			t.Errorf("Link(%q) = %q, want %q", raw, got, want)
		}
	}
}

func TestTitle(t *testing.T) {
	if got := Title("  Clip  by\n{user} ", "Anna"); got != "Clip by Anna" {
		t.Fatalf("%q", got)
	}
	if got := Title(strings.Repeat("ä", 150), "x"); len([]rune(got)) != config.MaxClipTitle {
		t.Fatalf("not cut: %d", len([]rune(got)))
	}
}

func TestNormalize(t *testing.T) {
	cmds := []config.ChatCommand{{ID: "1", Name: "discord", Aliases: []string{"dc"}, Trigger: config.TriggerCommand}, {ID: "2", Name: "clip", Trigger: config.TriggerWord}}
	reserved := []string{"sr", "song"}
	ok := config.DefaultClips()
	ok.Name = " !Clip "
	got, err := Normalize(ok, cmds, reserved)
	if err != nil || got.Name != "clip" { // a word trigger "clip" doesn't clash
		t.Fatalf("%+v %v", got, err)
	}
	for name, c := range map[string]config.Clips{
		"bad name": {Name: "my clip", Permission: config.PermEveryone},
		"song":     {Name: "sr", Permission: config.PermEveryone},
		"taken":    {Name: "dc", Permission: config.PermEveryone},
		"perm":     {Name: "clips", Permission: "admin"},
		"cooldown": {Name: "clips", Permission: config.PermEveryone, CooldownSeconds: -1},
		"title":    {Name: "clips", Permission: config.PermEveryone, Title: strings.Repeat("a", config.MaxClipTitle+1)},
	} {
		if _, err := Normalize(c, cmds, reserved); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

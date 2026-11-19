package builtin

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/miwidot/darkbot/internal/config"
	"github.com/miwidot/darkbot/internal/twitch"
)

type fakeAPI struct {
	descs     []string
	markerErr error
	snoozeErr error
	snoozes   int
}

func (f *fakeAPI) CreateMarker(_ context.Context, _, desc string) (twitch.Marker, error) {
	f.descs = append(f.descs, desc)
	return twitch.Marker{Position: 3723 * time.Second}, f.markerErr
}

func (f *fakeAPI) SnoozeAd(context.Context, string) (twitch.AdSchedule, error) {
	f.snoozes++
	return twitch.AdSchedule{Snoozes: 2}, f.snoozeErr
}

type nopLog struct{}

func (nopLog) Infof(string, ...any) {}
func (nopLog) Warnf(string, ...any) {}

type harness struct {
	h       *Handler
	api     *fakeAPI
	set     map[string]config.BuiltinCommand
	scopes  map[string]bool
	now     time.Time
	mu      sync.Mutex
	replies []string
}

func newHarness() *harness {
	x := &harness{api: &fakeAPI{}, now: time.Date(2026, 11, 19, 20, 0, 0, 0, time.UTC),
		scopes: map[string]bool{twitch.ScopeManageBroadcast: true, twitch.ScopeManageAds: true}}
	m, s := config.DefaultMarker(), config.DefaultSnooze()
	m.Enabled, s.Enabled = true, true
	x.set = map[string]config.BuiltinCommand{KeyMarker: m, KeySnooze: s}
	x.h = New(Deps{
		Settings: func(k string) config.BuiltinCommand { return x.set[k] },
		API:      x.api,
		HasScope: func(s string) bool { return x.scopes[s] },
		Reply: func(_ context.Context, text, _ string) {
			x.mu.Lock()
			x.replies = append(x.replies, text)
			x.mu.Unlock()
		},
		Lang: func() string { return "en" },
		Log:  nopLog{},
		Now:  func() time.Time { return x.now },
	})
	return x
}

func (x *harness) run(text string, mod bool) string {
	m := twitch.ChatMessage{ID: "m", BroadcasterID: "42", ChatterID: "7", ChatterName: "Mo", Text: text, IsModerator: mod}
	key, ok := x.h.Match(text)
	if !ok {
		return "<no match>"
	}
	n := len(x.replies)
	x.h.Handle(context.Background(), key, m)
	if len(x.replies) == n {
		return "<silent>"
	}
	return x.replies[len(x.replies)-1]
}

func TestMarker(t *testing.T) {
	x := newHarness()
	if got := x.run("!marker  great   play ", true); got != "Marker set at 1:02:03." || x.api.descs[0] != "great play" {
		t.Fatalf("%q %q", got, x.api.descs)
	}
	x.now = x.now.Add(time.Minute)
	x.run("!Marker", true)
	if x.api.descs[1] != "Marker by Mo" {
		t.Fatalf("default description %q", x.api.descs[1])
	}
	x.api.markerErr = errors.Join(twitch.ErrMarkerLater, errors.New("404"))
	x.now = x.now.Add(time.Minute)
	if got := x.run("!marker", true); got != "Markers only work live, with VODs on, and not in the stream's first seconds." {
		t.Fatal(got)
	}
}

func TestPermissionsAndCooldown(t *testing.T) {
	x := newHarness()
	if got := x.run("!marker", false); got != "<silent>" || len(x.api.descs) != 0 {
		t.Fatalf("viewers may not set markers by default: %q", got)
	}
	m := x.set[KeyMarker]
	m.Permission = config.PermEveryone
	x.set[KeyMarker] = m
	x.run("!marker", false)
	x.now = x.now.Add(5 * time.Second)
	if got := x.run("!marker", false); got != "<silent>" {
		t.Fatalf("cooldown: %q", got)
	}
	x.run("!marker", true) // moderators skip it
	if len(x.api.descs) != 2 {
		t.Fatalf("markers: %d", len(x.api.descs))
	}
	m.Enabled = false
	x.set[KeyMarker] = m
	if got := x.run("!marker", true); got != "<no match>" {
		t.Fatalf("off: %q", got)
	}
}

func TestSnooze(t *testing.T) {
	x := newHarness()
	if got := x.run("!snooze", true); got != "Ad pushed back by 5 minutes. 2 snoozes left." {
		t.Fatal(got)
	}
	for err, want := range map[error]string{
		twitch.ErrNoSnoozes:   "No snoozes left.",
		twitch.ErrNoAdPlanned: "No ad is scheduled right now.",
		errors.New("boom"):    "That didn't work, try again in a moment.",
	} {
		x.api.snoozeErr = err
		if got := x.run("!snooze", true); got != want {
			t.Errorf("%v: %q", err, got)
		}
	}
}

func TestNeedsScope(t *testing.T) {
	x := newHarness()
	x.scopes = map[string]bool{}
	if got := x.run("!snooze", true); got != "That isn't available right now." || x.api.snoozes != 0 {
		t.Fatalf("%q %d", got, x.api.snoozes)
	}
}

func TestClock(t *testing.T) {
	for d, want := range map[time.Duration]string{0: "0:00:00", 59 * time.Second: "0:00:59", 3723 * time.Second: "1:02:03", 36000 * time.Second: "10:00:00"} {
		if got := Clock(d); got != want {
			t.Errorf("%v: %s", d, got)
		}
	}
}

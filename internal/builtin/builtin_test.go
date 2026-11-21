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

	live      bool
	since     time.Time
	streamErr error
	follows   map[string]time.Time // user ID -> followed_at
	users     map[string]twitch.User
	lookups   int
}

func (f *fakeAPI) CreateMarker(_ context.Context, _, desc string) (twitch.Marker, error) {
	f.descs = append(f.descs, desc)
	return twitch.Marker{Position: 3723 * time.Second}, f.markerErr
}

func (f *fakeAPI) SnoozeAd(context.Context, string) (twitch.AdSchedule, error) {
	f.snoozes++
	return twitch.AdSchedule{Snoozes: 2}, f.snoozeErr
}

func (f *fakeAPI) LiveSince(context.Context, string) (bool, time.Time, error) {
	return f.live, f.since, f.streamErr
}

func (f *fakeAPI) FollowedAt(_ context.Context, _, userID string) (time.Time, bool, error) {
	at, ok := f.follows[userID]
	return at, ok, nil
}

func (f *fakeAPI) UserByLogin(_ context.Context, login string) (twitch.User, bool, error) {
	f.lookups++
	u, ok := f.users[login]
	return u, ok, nil
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
		scopes: map[string]bool{twitch.ScopeManageBroadcast: true, twitch.ScopeManageAds: true, twitch.ScopeReadFollowers: true}}
	m, s := config.DefaultMarker(), config.DefaultSnooze()
	u, f := config.DefaultUptime(), config.DefaultFollowage()
	m.Enabled, s.Enabled, u.Enabled, f.Enabled = true, true, true, true
	x.set = map[string]config.BuiltinCommand{KeyMarker: m, KeySnooze: s, KeyUptime: u, KeyFollowage: f}
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

func TestUptime(t *testing.T) {
	x := newHarness()
	if got := x.run("!uptime", false); got != "The stream isn't live right now." {
		t.Fatal(got)
	}
	x.api.live = true
	for d, want := range map[time.Duration]string{
		30 * time.Second:             "The stream just started.",
		time.Minute:                  "The stream has been live for 1 minute.",
		time.Hour:                    "The stream has been live for 1 hour.",
		2*time.Hour + 13*time.Minute: "The stream has been live for 2 hours and 13 minutes.",
		26*time.Hour + 1*time.Minute: "The stream has been live for 26 hours and 1 minute.",
	} {
		x.now = x.now.Add(time.Hour) // past the cooldown
		x.api.since = x.now.Add(-d)
		if got := x.run("!uptime", false); got != want {
			t.Errorf("%v: %q, want %q", d, got, want)
		}
	}
	x.api.streamErr = errors.New("boom")
	x.now = x.now.Add(time.Hour)
	if got := x.run("!uptime", false); got != "That didn't work, try again in a moment." {
		t.Fatal(got)
	}
}

func TestFollowage(t *testing.T) {
	x := newHarness() // now: 2026-11-19 20:00 UTC, asker "Mo" (ID 7)
	x.api.users = map[string]twitch.User{
		"anna": {ID: "8", Login: "anna", DisplayName: "Anna"},
		"chan": {ID: "42", Login: "chan", DisplayName: "Chan"},
		"ben":  {ID: "9", Login: "ben", DisplayName: "Ben"},
	}
	x.api.follows = map[string]time.Time{
		"7": time.Date(2025, 8, 4, 18, 0, 0, 0, time.UTC),
		"8": time.Date(2026, 11, 19, 9, 0, 0, 0, time.UTC),
	}
	cases := []struct{ text, want string }{
		{"!followage", "Mo has been following for 1 year and 3 months (since Aug 4, 2025)."},
		{"!followage @Anna", "Anna started following today (Nov 19, 2026)."},
		{"!followage ben", "Ben isn't following the channel."},
		{"!followage @chan", "Chan is the channel itself."},
		{"!followage @nobody", "There's no Twitch account called nobody."},
		{"!followage <script>", "Usage: !followage or !followage @name"},
	}
	for _, c := range cases {
		x.now = x.now.Add(time.Minute) // past the cooldown
		if got := x.run(c.text, true); got != c.want {
			t.Errorf("%q: %q, want %q", c.text, got, c.want)
		}
	}
	x.scopes = map[string]bool{}
	x.now = x.now.Add(time.Minute)
	if got := x.run("!followage", true); got != "That isn't available right now." {
		t.Fatal(got)
	}
}

func TestCalendarSince(t *testing.T) {
	d := func(y int, m time.Month, day int) time.Time { return time.Date(y, m, day, 12, 0, 0, 0, time.UTC) }
	for _, c := range []struct {
		from, to time.Time
		y, m, dd int
	}{
		{d(2025, 1, 31), d(2025, 3, 1), 0, 1, 1},
		{d(2024, 2, 29), d(2025, 2, 28), 1, 0, 0},
		{d(2025, 3, 30), d(2025, 3, 31), 0, 0, 1},
		{d(2025, 10, 26), d(2025, 11, 26), 0, 1, 0},
		{d(2024, 2, 29), d(2025, 3, 1), 1, 0, 1},
		{d(2025, 11, 19), d(2026, 11, 19), 1, 0, 0},
		{d(2025, 12, 31), d(2026, 1, 1), 0, 0, 1},
		{d(2026, 1, 2), d(2026, 1, 1), 0, 0, 0}, // clock skew
	} {
		y, m, dd := calendarSince(c.from, c.to)
		if y != c.y || m != c.m || dd != c.dd {
			t.Errorf("%s to %s: %d/%d/%d, want %d/%d/%d", c.from.Format(time.DateOnly), c.to.Format(time.DateOnly), y, m, dd, c.y, c.m, c.dd)
		}
	}
}

// Across a daylight saving change a day has 23 or 25 hours.
func TestCalendarSinceAcrossDST(t *testing.T) {
	berlin, err := time.LoadLocation("Europe/Berlin")
	if err != nil {
		t.Skip("no time zone data:", err)
	}
	from := time.Date(2025, 3, 29, 23, 30, 0, 0, berlin)
	to := time.Date(2025, 3, 31, 0, 30, 0, 0, berlin)
	if y, m, d := calendarSince(from, to); y != 0 || m != 0 || d != 2 {
		t.Fatalf("%d/%d/%d, want 2 days", y, m, d)
	}
}

func TestUnitsGerman(t *testing.T) {
	x := newHarness()
	h := x.h
	h.d.Lang = func() string { return "de" }
	for _, c := range []struct {
		us   []unit
		want string
	}{
		{[]unit{{"dur.year", 1}, {"dur.month", 0}, {"dur.day", 4}}, "1 Jahr"},
		{[]unit{{"dur.year", 2}, {"dur.month", 1}, {"dur.day", 4}}, "2 Jahren und 1 Monat"},
		{[]unit{{"dur.year", 0}, {"dur.month", 0}, {"dur.day", 3}}, "3 Tagen"},
	} {
		if got := h.units(c.us...); got != c.want {
			t.Errorf("%v: %q, want %q", c.us, got, c.want)
		}
	}
}

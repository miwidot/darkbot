package bot

import (
	"context"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/miwidot/darkbot/internal/config"
	"github.com/miwidot/darkbot/internal/twitch"
)

func TestFindSound(t *testing.T) {
	sounds := []config.Sound{
		{ID: "a", RewardID: "r1", RewardTitle: "Airhorn"},
		{ID: "b", RewardTitle: "Bruh"},
		{ID: "c", RewardID: "r3", RewardTitle: "Same Title"},
	}
	tests := []struct {
		name, rewardID, title, want string
		ok                          bool
	}{
		{"by id", "r1", "renamed", "a", true},
		{"title fallback, case-insensitive", "rX", " bruh ", "b", true},
		{"title does not match sounds bound to an id", "rY", "Same Title", "", false},
		{"unknown", "rZ", "nope", "", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s, ok := FindSound(sounds, tt.rewardID, tt.title)
			if ok != tt.ok || s.ID != tt.want {
				t.Fatalf("got (%q,%v), want (%q,%v)", s.ID, ok, tt.want, tt.ok)
			}
		})
	}
}

func TestCooldowns(t *testing.T) {
	now := time.Date(2026, 11, 16, 12, 0, 0, 0, time.UTC)
	s := config.Sound{ID: "s", CooldownSeconds: 10, UserCooldownSeconds: 60}

	c := NewCooldowns()
	if _, blocked := c.Try(s, "u1", now); blocked {
		t.Fatal("first redemption must pass")
	}
	if wait, blocked := c.Try(s, "u2", now.Add(5*time.Second)); !blocked || wait != 5*time.Second {
		t.Fatalf("global cooldown: blocked=%v wait=%s", blocked, wait)
	}
	if _, blocked := c.Try(s, "u2", now.Add(11*time.Second)); blocked {
		t.Fatal("other user after global cooldown must pass")
	}
	if wait, blocked := c.Try(s, "u1", now.Add(30*time.Second)); !blocked || wait != 30*time.Second {
		t.Fatalf("user cooldown: blocked=%v wait=%s", blocked, wait)
	}
	if _, blocked := c.Try(s, "u1", now.Add(61*time.Second)); blocked {
		t.Fatal("user cooldown must expire")
	}
}

func TestCooldownsZeroMeansNone(t *testing.T) {
	now := time.Now()
	s := config.Sound{ID: "s"}
	c := NewCooldowns()
	for i := 0; i < 3; i++ {
		if _, blocked := c.Try(s, "u", now); blocked {
			t.Fatal("no cooldown configured, must never block")
		}
	}
}

type fakeOutput struct{ played chan string }

func (f *fakeOutput) Play(_ context.Context, s config.Sound, _ float64) error {
	f.played <- s.ID
	return nil
}

func TestHandleRedemptionActivityFlow(t *testing.T) {
	store, err := config.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	_ = store.Update(func(c *config.Config) {
		c.Sounds = []config.Sound{
			{ID: "on", RewardID: "r1", RewardTitle: "Airhorn", File: "/a.mp3", CooldownSeconds: 30, Enabled: true},
			{ID: "off", RewardID: "r2", RewardTitle: "Bruh", File: "/b.mp3", Enabled: false},
		}
	})

	var mu sync.Mutex
	var kinds []string
	acts := make(chan Activity, 16)
	out := &fakeOutput{played: make(chan string, 4)}
	b := New(store, nopLogger{}, func() Output { return out }, func(a Activity) {
		mu.Lock()
		kinds = append(kinds, a.Kind)
		mu.Unlock()
		acts <- a
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go b.Run(ctx)

	b.HandleRedemption(twitch.Redemption{RewardID: "r1", RewardTitle: "Airhorn", UserID: "u1", UserName: "Kim"})
	b.HandleRedemption(twitch.Redemption{RewardID: "r1", RewardTitle: "Airhorn", UserID: "u2", UserName: "Max"})
	b.HandleRedemption(twitch.Redemption{RewardID: "r2", RewardTitle: "Bruh", UserID: "u3", UserName: "Lea"})
	b.HandleRedemption(twitch.Redemption{RewardID: "zz", RewardTitle: "Hydrate", UserID: "u4", UserName: "Ola"})

	if id := <-out.played; id != "on" {
		t.Fatalf("played %q", id)
	}
	// queued, cooldown, disabled, playing, done
	for i := 0; i < 5; i++ {
		select {
		case <-acts:
		case <-time.After(2 * time.Second):
			t.Fatalf("missing activity, got %v", kinds)
		}
	}
	mu.Lock()
	defer mu.Unlock()
	want := map[string]bool{ActQueued: true, ActCooldown: true, ActDisabled: true, ActPlaying: true, ActDone: true}
	for _, k := range kinds {
		delete(want, k)
	}
	if len(want) != 0 || len(kinds) != 5 {
		t.Fatalf("activities = %v", kinds)
	}
}

type nopLogger struct{}

func (nopLogger) Infof(string, ...any)  {}
func (nopLogger) Warnf(string, ...any)  {}
func (nopLogger) Errorf(string, ...any) {}

func TestManagedRedemptionsAreFulfilledOrRejected(t *testing.T) {
	store, _ := config.Open(t.TempDir())
	_ = store.Update(func(c *config.Config) {
		c.Sounds = []config.Sound{
			{ID: "m", RewardID: "rm", RewardTitle: "Managed", File: "/m.mp3", Managed: true, UserCooldownSeconds: 60, Enabled: true},
			{ID: "x", RewardID: "rx", RewardTitle: "Existing", File: "/x.mp3", Enabled: true},
		}
	})
	out := &fakeOutput{played: make(chan string, 4)}
	b := New(store, nopLogger{}, func() Output { return out }, nil)
	type res struct {
		id      string
		fulfill bool
	}
	got := make(chan res, 8)
	b.SetResolver(func(r twitch.Redemption, fulfill bool) { got <- res{r.ID, fulfill} })
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go b.Run(ctx)

	b.HandleRedemption(twitch.Redemption{ID: "1", RewardID: "rm", UserID: "u1"})
	b.HandleRedemption(twitch.Redemption{ID: "2", RewardID: "rm", UserID: "u1"}) // per-viewer cooldown
	b.HandleRedemption(twitch.Redemption{ID: "3", RewardID: "rx", UserID: "u1"}) // not managed: never resolved

	want := map[string]bool{"1": true, "2": false}
	for i := 0; i < 2; i++ {
		select {
		case r := <-got:
			if w, ok := want[r.id]; !ok || w != r.fulfill {
				t.Fatalf("unexpected resolution %+v", r)
			}
			delete(want, r.id)
		case <-time.After(2 * time.Second):
			t.Fatalf("missing resolutions: %v", want)
		}
	}
	select {
	case r := <-got:
		t.Fatalf("unmanaged reward must not be resolved: %+v", r)
	case <-time.After(200 * time.Millisecond):
	}
}

func TestPlayEventIsMarkedAndNeverResolved(t *testing.T) {
	store, err := config.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	acts := make(chan Activity, 8)
	out := &fakeOutput{played: make(chan string, 1)}
	b := New(store, nopLogger{}, func() Output { return out }, func(a Activity) { acts <- a })
	b.SetResolver(func(twitch.Redemption, bool) { t.Error("event sounds must never fulfill or reject a redemption") })
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go b.Run(ctx)

	b.PlayEvent(config.Sound{ID: "ev-follow", RewardTitle: "Follow", File: "/f.mp3", Managed: true, Enabled: true}, nil, "Kim")
	if id := <-out.played; id != "ev-follow" {
		t.Fatalf("played %q", id)
	}
	for range 2 { // playing, done
		select {
		case a := <-acts:
			if !a.Event || a.User != "Kim" {
				t.Fatalf("activity %+v must be marked as event", a)
			}
		case <-time.After(2 * time.Second):
			t.Fatal("missing activity")
		}
	}
}

// fakeOverlay records when an alert was shown and hidden.
type fakeOverlay struct {
	length time.Duration
	shown  chan Alert
	hidden chan time.Time
}

func (f *fakeOverlay) ShowAlert(_ context.Context, a Alert) (func(), time.Duration, error) {
	f.shown <- a
	return func() { f.hidden <- time.Now() }, f.length, nil
}

// slowOutput plays every sound for d.
type slowOutput struct{ d time.Duration }

func (s slowOutput) Play(ctx context.Context, _ config.Sound, _ float64) error {
	select {
	case <-ctx.Done():
	case <-time.After(s.d):
	}
	return nil
}

func alertBot(t *testing.T, out Output, ov Overlay) *Bot {
	t.Helper()
	store, err := config.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	b := New(store, nopLogger{}, func() Output { return out }, nil)
	b.SetOverlay(func() Overlay { return ov })
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go b.Run(ctx)
	return b
}

// timeTolerance absorbs scheduling jitter in the timing tests.
const timeTolerance = 150 * time.Millisecond

func hiddenAfter(t *testing.T, ov *fakeOverlay, start time.Time, want time.Duration) {
	t.Helper()
	select {
	case at := <-ov.hidden:
		if got := at.Sub(start); got < want-timeTolerance || got > want+timeTolerance*3 {
			t.Fatalf("hidden after %s, want about %s", got, want)
		}
	case <-time.After(want + 2*time.Second):
		t.Fatal("alert never hidden")
	}
}

// The alert stays its seconds even when the sound is shorter.
func TestAlertStaysItsSeconds(t *testing.T) {
	ov := &fakeOverlay{shown: make(chan Alert, 1), hidden: make(chan time.Time, 1)}
	b := alertBot(t, slowOutput{100 * time.Millisecond}, ov)
	start := time.Now()
	b.PlayEvent(config.Sound{File: "/f.mp3", Enabled: true}, &Alert{Text: "Anna", Seconds: 1}, "Anna")
	if a := <-ov.shown; a.Text != "Anna" {
		t.Fatalf("shown %+v", a)
	}
	hiddenAfter(t, ov, start, time.Second)
}

// A sound longer than the alert keeps the alert until the sound ends.
func TestLongSoundKeepsAlert(t *testing.T) {
	ov := &fakeOverlay{shown: make(chan Alert, 1), hidden: make(chan time.Time, 1)}
	b := alertBot(t, slowOutput{1500 * time.Millisecond}, ov)
	start := time.Now()
	b.PlayEvent(config.Sound{File: "/f.mp3", Enabled: true}, &Alert{Text: "x", Seconds: 1}, "x")
	<-ov.shown
	hiddenAfter(t, ov, start, 1500*time.Millisecond)
}

// No seconds and no video length: a picture stays DefaultAlertSeconds.
func TestDefaultAlertSeconds(t *testing.T) {
	if testing.Short() {
		t.Skip("waits DefaultAlertSeconds")
	}
	ov := &fakeOverlay{shown: make(chan Alert, 1), hidden: make(chan time.Time, 1)}
	b := alertBot(t, slowOutput{100 * time.Millisecond}, ov)
	start := time.Now()
	b.PlayEvent(config.Sound{File: "/f.mp3", Enabled: true}, &Alert{File: "/follow.gif"}, "x")
	<-ov.shown
	hiddenAfter(t, ov, start, DefaultAlertSeconds*time.Second)
}

// Without seconds a video stays as long as it plays; no sound needed.
func TestVideoAlertWithoutSound(t *testing.T) {
	ov := &fakeOverlay{length: 300 * time.Millisecond, shown: make(chan Alert, 1), hidden: make(chan time.Time, 1)}
	b := alertBot(t, slowOutput{time.Hour}, ov)
	start := time.Now()
	b.PlayEvent(config.Sound{}, &Alert{File: "/raid.webm"}, "x")
	<-ov.shown
	hiddenAfter(t, ov, start, 300*time.Millisecond)
}

// No OBS: the sound still plays.
func TestNoOverlayStillPlaysSound(t *testing.T) {
	out := &fakeOutput{played: make(chan string, 1)}
	b := alertBot(t, out, nil)
	b.PlayEvent(config.Sound{ID: "ev", File: "/f.mp3", Enabled: true}, &Alert{Text: "x"}, "x")
	select {
	case <-out.played:
	case <-time.After(2 * time.Second):
		t.Fatal("the sound must play without OBS")
	}
}

// countingOutput plays every sound for d and records the most sounds that
// ever played at the same time, and the order they started in.
type countingOutput struct {
	d       time.Duration
	mu      sync.Mutex
	now     int
	most    int
	started []string
}

func (c *countingOutput) Play(ctx context.Context, s config.Sound, _ float64) error {
	c.mu.Lock()
	c.now++
	c.most = max(c.most, c.now)
	c.started = append(c.started, s.ID)
	c.mu.Unlock()
	select {
	case <-ctx.Done():
	case <-time.After(c.d):
	}
	c.mu.Lock()
	c.now--
	c.mu.Unlock()
	return nil
}

// A raid, a sub, a redemption and a test arriving together play one after
// another in arrival order, never mixed.
func TestSoundsNeverOverlap(t *testing.T) {
	store, err := config.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	reward := config.Sound{ID: "airhorn", RewardID: "r1", RewardTitle: "Airhorn", File: "/a.mp3", Enabled: true, Volume: 1}
	if err := store.Update(func(c *config.Config) { c.Sounds = []config.Sound{reward} }); err != nil {
		t.Fatal(err)
	}
	const each = 80 * time.Millisecond
	out := &countingOutput{d: each}
	b := New(store, nopLogger{}, func() Output { return out }, nil)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go b.Run(ctx)

	b.PlayEvent(config.Sound{ID: "ev-raid", File: "/raid.mp3"}, nil, "Anna")
	b.PlayEvent(config.Sound{ID: "ev-sub", File: "/sub.mp3"}, nil, "Ben")
	b.HandleRedemption(twitch.Redemption{RewardID: "r1", RewardTitle: "Airhorn", UserID: "u1", UserName: "Cleo"})
	b.Test("airhorn")

	want := []string{"ev-raid", "ev-sub", "airhorn", "airhorn"}
	deadline := time.Now().Add(time.Duration(len(want))*each + 2*time.Second)
	for {
		out.mu.Lock()
		n, most, started := out.now, out.most, slices.Clone(out.started)
		out.mu.Unlock()
		if len(started) == len(want) && n == 0 {
			if most != 1 {
				t.Fatalf("%d sounds played at once", most)
			}
			if !slices.Equal(started, want) {
				t.Fatalf("order %v, want %v", started, want)
			}
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("only %v played", started)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// On shutdown the redemptions still waiting are rejected (points back).
func TestStopRefundsQueued(t *testing.T) {
	store, err := config.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	_ = store.Update(func(c *config.Config) {
		c.Sounds = []config.Sound{{ID: "m", RewardID: "rm", RewardTitle: "Managed", File: "/m.mp3", Managed: true, Enabled: true}}
	})
	b := New(store, nopLogger{}, func() Output { return slowOutput{d: time.Minute} }, nil)
	var mu sync.Mutex
	got := map[string]bool{}
	b.SetResolver(func(r twitch.Redemption, fulfill bool) {
		time.Sleep(50 * time.Millisecond) // a Twitch request
		mu.Lock()
		got[r.ID] = fulfill
		mu.Unlock()
	})
	ctx, cancel := context.WithCancel(context.Background())
	go b.Run(ctx)
	for _, id := range []string{"1", "2", "3"} {
		b.HandleRedemption(twitch.Redemption{ID: id, RewardID: "rm", UserID: "u" + id})
	}
	time.Sleep(100 * time.Millisecond) // "1" is playing, "2" and "3" wait
	cancel()
	b.Stop(2 * time.Second)

	mu.Lock()
	defer mu.Unlock()
	for _, id := range []string{"2", "3"} {
		if f, ok := got[id]; !ok || f {
			t.Errorf("queued redemption %s: resolved %v fulfill %v, want rejected", id, ok, f)
		}
	}
}

func TestCooldownRelease(t *testing.T) {
	now := time.Date(2026, 11, 16, 12, 0, 0, 0, time.UTC)
	s := config.Sound{ID: "s", CooldownSeconds: 10, UserCooldownSeconds: 60}
	c := NewCooldowns()
	if _, blocked := c.Try(s, "u1", now); blocked {
		t.Fatal("first must pass")
	}
	c.Release(s, "u1", now) // could not be queued
	if _, blocked := c.Try(s, "u1", now.Add(time.Second)); blocked {
		t.Fatal("a released try must not leave a cooldown")
	}
	c.Release(s, "u1", now) // stale release: must not undo the newer try
	if _, blocked := c.Try(s, "u2", now.Add(2*time.Second)); !blocked {
		t.Fatal("a release must only undo its own try")
	}
}

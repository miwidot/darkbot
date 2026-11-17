package bot

import (
	"context"
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

	b.PlayEvent(config.Sound{ID: "ev-follow", RewardTitle: "Follow", File: "/f.mp3", Managed: true, Enabled: true}, "Kim")
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

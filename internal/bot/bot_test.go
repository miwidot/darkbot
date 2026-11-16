package bot

import (
	"testing"
	"time"

	"github.com/miwi/darkbot/internal/config"
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

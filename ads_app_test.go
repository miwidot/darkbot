package main

import (
	"testing"
	"time"

	"github.com/miwidot/darkbot/internal/config"
	"github.com/miwidot/darkbot/internal/twitch"
)

func TestAdWarnDue(t *testing.T) {
	now := time.Date(2026, 11, 19, 20, 0, 0, 0, time.UTC)
	lead := 60 * time.Second
	for _, tt := range []struct {
		name         string
		next, warned time.Time
		due          bool
		left         int
	}{
		{"no ad scheduled", time.Time{}, time.Time{}, false, 0},
		{"too early", now.Add(90 * time.Second), time.Time{}, false, 0},
		{"exactly lead", now.Add(60 * time.Second), time.Time{}, true, 60},
		{"late poll still warns", now.Add(20*time.Second + 300*time.Millisecond), time.Time{}, true, 21},
		{"already warned", now.Add(30 * time.Second), now.Add(30 * time.Second), false, 0},
		{"snoozed: new time, new warning", now.Add(50 * time.Second), now.Add(-250 * time.Second), true, 50},
		{"ad already running", now.Add(-time.Second), time.Time{}, false, 0},
	} {
		due, left := adWarnDue(tt.next, lead, now, tt.warned)
		if due != tt.due || left != tt.left {
			t.Errorf("%s: due=%v left=%d, want %v %d", tt.name, due, left, tt.due, tt.left)
		}
	}
}

func TestAdWait(t *testing.T) {
	now := time.Date(2026, 11, 19, 20, 0, 0, 0, time.UTC)
	if got := adWait(time.Time{}, time.Minute, now); got != adPollInterval {
		t.Errorf("no ad: %v", got)
	}
	// Due in 5 s: sleep exactly until then, not a whole poll.
	if got := adWait(now.Add(65*time.Second), time.Minute, now); got != 5*time.Second {
		t.Errorf("soon: %v", got)
	}
	if got := adWait(now.Add(time.Hour), time.Minute, now); got != adPollInterval {
		t.Errorf("far: %v", got)
	}
}

func TestEventExtras(t *testing.T) {
	a, _ := newEventApp(t, nil)
	// Kind-only settings are dropped elsewhere and checked where they belong.
	if err := a.SaveEventAction(twitch.EvFollow, config.EventAction{Enabled: true, Shoutout: true, LeadSeconds: 90, Volume: 1}); err != nil {
		t.Fatal(err)
	}
	if got := a.cfg.Get().Events[twitch.EvFollow]; got.Shoutout || got.LeadSeconds != 0 {
		t.Fatalf("follow kept raid/ad settings: %+v", got)
	}
	if err := a.SaveEventAction(twitch.EvAdSoon, config.EventAction{Enabled: true, LeadSeconds: 5, Volume: 1}); err == nil {
		t.Fatal("a 5 s warning is below the minimum")
	}
	if err := a.SaveEventAction(twitch.EvRaid, config.EventAction{Enabled: true, Shoutout: true, Volume: 1}); err != nil || !a.cfg.Get().Events[twitch.EvRaid].Shoutout {
		t.Fatalf("raid shoutout not kept: %v", err)
	}
}

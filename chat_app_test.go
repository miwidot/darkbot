package main

import (
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/miwidot/darkbot/internal/activity"
	"github.com/miwidot/darkbot/internal/applog"
	"github.com/miwidot/darkbot/internal/config"
	"github.com/miwidot/darkbot/internal/stats"
)

func newChatApp(t *testing.T) *App {
	t.Helper()
	cfg, err := config.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	log := applog.New()
	a := &App{cfg: cfg, log: log, st: stats.Open(filepath.Join(t.TempDir(), "s.json"), log), act: activity.Open(filepath.Join(t.TempDir(), "a.json"), log)}
	a.cb = a.newChatBot()
	return a
}

func TestChatOffByDefault(t *testing.T) {
	c := newChatApp(t).cfg.Get().Chat
	if c.CommandsEnabled || c.TimersEnabled {
		t.Fatalf("commands and timers must start off: %+v", c)
	}
}

func TestSaveCommand(t *testing.T) {
	a := newChatApp(t)
	if err := a.SaveCommand(config.ChatCommand{Name: "!Discord", Response: "discord.gg/x", Permission: config.PermEveryone, Enabled: true}); err != nil {
		t.Fatal(err)
	}
	cmds := a.cfg.Get().Chat.Commands
	if len(cmds) != 1 || cmds[0].ID == "" || cmds[0].Name != "discord" {
		t.Fatalf("got %+v", cmds)
	}
	// Edit keeps the ID and replaces the command.
	edit := cmds[0]
	edit.Response = "new"
	if err := a.SaveCommand(edit); err != nil {
		t.Fatal(err)
	}
	if got := a.cfg.Get().Chat.Commands; len(got) != 1 || got[0].Response != "new" {
		t.Fatalf("after edit %+v", got)
	}
	// Refused: song command name; nothing changes.
	if err := a.SaveCommand(config.ChatCommand{Name: "sr", Response: "x", Permission: config.PermEveryone}); err == nil {
		t.Fatal("!sr belongs to song requests")
	}
	if err := a.SaveCommand(config.ChatCommand{Name: "x", Response: "x", Permission: config.PermEveryone, Sound: "relative.mp3"}); err == nil {
		t.Fatal("a relative sound path must be refused")
	}
	if got := a.cfg.Get().Chat.Commands; len(got) != 1 {
		t.Fatalf("a refused save changed the commands: %+v", got)
	}
	if err := a.DeleteCommand(edit.ID); err != nil || len(a.cfg.Get().Chat.Commands) != 0 {
		t.Fatalf("delete: %v %+v", err, a.cfg.Get().Chat.Commands)
	}
}

func TestSaveTimer(t *testing.T) {
	a := newChatApp(t)
	if err := a.SaveTimer(config.ChatTimer{Message: "Follow!", IntervalMinutes: 15, Enabled: true}); err != nil {
		t.Fatal(err)
	}
	if err := a.SaveTimer(config.ChatTimer{Message: "spam", IntervalMinutes: 1}); err == nil {
		t.Fatal("one minute is below the minimum")
	}
	tm := a.cfg.Get().Chat.Timers
	if len(tm) != 1 || tm[0].ID == "" || !slices.Equal(tm[0].Messages, []string{"Follow!"}) {
		t.Fatalf("got %+v", tm)
	}
	socials := config.ChatTimer{ID: tm[0].ID, Messages: []string{"Twitter", " ", "YouTube"}, Order: config.TimerInOrder, IntervalMinutes: 15}
	if err := a.SaveTimer(socials); err != nil {
		t.Fatal(err)
	}
	if got := a.cfg.Get().Chat.Timers[0]; !slices.Equal(got.Messages, []string{"Twitter", "YouTube"}) || got.Order != config.TimerInOrder {
		t.Fatalf("socials: %+v", got)
	}
	if err := a.SetTimersEnabled(true); err != nil || !a.cfg.Get().Chat.TimersEnabled {
		t.Fatalf("enable: %v", err)
	}
}

// Started darkbot during a stream that began earlier: a new dashboard
// session from the stream's start. Restarted during the same stream (the
// session began after the stream did): the session stays.
func TestLiveStartsSessionOfMissedStream(t *testing.T) {
	a := newChatApp(t)
	a.st.Follow("Anna")
	streamStart := a.st.Snapshot().Since.Add(time.Hour) // after the session began
	a.setLive(true, streamStart)
	snap := a.st.Snapshot()
	if !a.live.Load() || !snap.Since.Equal(streamStart) || snap.Session.Follows != 0 {
		t.Fatalf("missed stream: live=%v %+v", a.live.Load(), snap)
	}
	a.st.Follow("Ben")
	a.setLive(true, streamStart) // same stream, e.g. after an update
	if snap := a.st.Snapshot(); snap.Session.Follows != 1 {
		t.Fatalf("same stream must keep the session: %+v", snap.Session)
	}
	a.setLive(false, time.Time{})
	if a.live.Load() {
		t.Fatal("offline")
	}

	// stream.online started a session by the local clock, which runs a few
	// minutes behind Twitch's: the same stream must not reset it again.
	a.st.ResetSessionAt(streamStart.Add(2 * time.Hour))
	a.st.Follow("Cem")
	a.setLive(true, streamStart.Add(2*time.Hour+3*time.Minute))
	if snap := a.st.Snapshot(); snap.Session.Follows != 1 {
		t.Fatalf("clock skew wiped the session: %+v", snap.Session)
	}
}

func TestSaveClips(t *testing.T) {
	a := newChatApp(t)
	if err := a.SaveCommand(config.ChatCommand{Name: "discord", Response: "x", Permission: config.PermEveryone}); err != nil {
		t.Fatal(err)
	}
	s := config.DefaultClips()
	s.Enabled, s.Title = true, " Clip  by {user} "
	if err := a.SaveClips(s); err != nil {
		t.Fatal(err)
	}
	if got := a.cfg.Get().Clips; !got.Enabled || got.Name != "clips" || got.Title != "Clip by {user}" {
		t.Fatalf("got %+v", got)
	}
	// The clip command's name is taken for the streamer's own commands ...
	if err := a.SaveCommand(config.ChatCommand{Name: "clips", Response: "x", Permission: config.PermEveryone}); err == nil {
		t.Fatal("!clips belongs to the clip command")
	}
	// ... and theirs and the song commands for the clip command.
	for _, name := range []string{"discord", "sr"} {
		s.Name = name
		if err := a.SaveClips(s); err == nil {
			t.Fatalf("!%s was accepted for clips", name)
		}
	}
	if got := a.cfg.Get().Clips.Name; got != "clips" {
		t.Fatalf("a refused save changed the name to %q", got)
	}
}

func TestSaveBuiltin(t *testing.T) {
	a := newChatApp(t)
	m := config.DefaultMarker()
	m.Enabled = true
	if err := a.SaveBuiltin("marker", m); err != nil {
		t.Fatal(err)
	}
	if !a.cfg.Get().Marker.Enabled {
		t.Fatal("not saved")
	}
	// Names can't clash among built-ins, with songs or with own commands.
	for _, name := range []string{"clips", "snooze", "sr"} {
		m.Name = name
		if err := a.SaveBuiltin("marker", m); err == nil {
			t.Errorf("!%s accepted for marker", name)
		}
	}
	if err := a.SaveCommand(config.ChatCommand{Name: "snooze", Response: "x", Permission: config.PermEveryone}); err == nil {
		t.Error("!snooze belongs to the built-in command")
	}
	if err := a.SaveBuiltin("nope", m); err == nil {
		t.Error("unknown key accepted")
	}
}

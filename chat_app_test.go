package main

import (
	"path/filepath"
	"testing"
	"time"

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
	return &App{cfg: cfg, log: log, st: stats.Open(filepath.Join(t.TempDir(), "s.json"), log)}
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
	if len(tm) != 1 || tm[0].ID == "" {
		t.Fatalf("got %+v", tm)
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
}

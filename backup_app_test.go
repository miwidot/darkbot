package main

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/miwidot/darkbot/internal/config"
)

func TestImportBackup(t *testing.T) {
	a := newChatApp(t)
	snd := filepath.Join(t.TempDir(), "horn.mp3")
	if err := os.WriteFile(snd, []byte("ID3"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := a.cfg.Update(func(c *config.Config) {
		c.Language = "de"
		c.Sounds = []config.Sound{{ID: "s1", RewardTitle: "Horn", File: snd, Enabled: true}}
	}); err != nil {
		t.Fatal(err)
	}
	zipPath := filepath.Join(t.TempDir(), "b.zip")
	rep, err := writeBackup(zipPath, a.cfg.Get())
	if err != nil || rep.Files != 1 {
		t.Fatalf("backup: %+v, %v", rep, err)
	}

	// Change everything, then restore.
	if err := a.cfg.Update(func(c *config.Config) { c.Language, c.Sounds = "en", nil }); err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 11, 24, 21, 30, 0, 0, time.Local)
	res, err := a.importBackup(zipPath, now)
	if err != nil {
		t.Fatal(err)
	}
	got := a.cfg.Get()
	if got.Language != "de" || len(got.Sounds) != 1 || got.Sounds[0].File != snd || res.Report.Kept != 1 {
		t.Fatalf("restored %+v, report %+v", got, res.Report)
	}
	var prev config.Config
	if err := config.LoadJSON(res.Previous, &prev); err != nil || prev.Language != "en" {
		t.Fatalf("settings before the import not kept: %v, %+v", err, prev.Language)
	}

	// A broken backup changes nothing.
	bad := filepath.Join(t.TempDir(), "bad.zip")
	os.WriteFile(bad, []byte("nope"), 0o600)
	if _, err := a.importBackup(bad, now); err == nil {
		t.Fatal("a broken backup must fail")
	}
	if a.cfg.Get().Language != "de" {
		t.Error("a failed import must not change the settings")
	}

	// A failed export leaves no file behind and keeps an older backup.
	if _, err := writeBackup(filepath.Join(t.TempDir(), "missing-dir", "x.zip"), got); err == nil {
		t.Error("writing into a missing folder must fail")
	}
}

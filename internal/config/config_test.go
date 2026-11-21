package config

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

func TestOpenMissingGivesDefaults(t *testing.T) {
	s, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	c := s.Get()
	if c.OutputMode != OutputDevice || c.OBS.Port != DefaultOBSPort || !c.OBS.Tracks[0] || c.OBS.InputName != DefaultOBSInputName {
		t.Fatalf("unexpected defaults: %+v", c)
	}
}

func TestUpdatePersistsAndNormalizes(t *testing.T) {
	dir := t.TempDir()
	s, _ := Open(dir)
	var notified Config
	s.Subscribe(func(c Config) { notified = c })

	err := s.Update(func(c *Config) {
		c.OutputMode = "bogus"
		c.OBS.Port = 0
		c.Sounds = append(c.Sounds, Sound{RewardTitle: "Airhorn", File: "/x.mp3", Volume: 7, CooldownSeconds: -3})
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(notified.Sounds) != 1 {
		t.Fatal("subscriber not notified")
	}

	re, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	c := re.Get()
	snd := c.Sounds[0]
	if c.OutputMode != OutputDevice || c.OBS.Port != DefaultOBSPort || snd.ID == "" || snd.Volume != DefaultSoundVolume || snd.CooldownSeconds != 0 {
		t.Fatalf("not normalized: %+v", c)
	}

	if runtime.GOOS != "windows" {
		fi, err := os.Stat(filepath.Join(dir, configFileName))
		if err != nil {
			t.Fatal(err)
		}
		if fi.Mode().Perm() != 0o600 { // only the user may read it
			t.Fatalf("config perm = %v, want 0600", fi.Mode().Perm())
		}
	}
}

func TestGetReturnsCopy(t *testing.T) {
	s, _ := Open(t.TempDir())
	_ = s.Update(func(c *Config) { c.Sounds = []Sound{{RewardTitle: "a", File: "/a.mp3"}} })
	c := s.Get()
	c.Sounds[0].RewardTitle = "changed"
	if s.Get().Sounds[0].RewardTitle != "a" {
		t.Fatal("Get must return a deep copy")
	}
}

func TestLanguageNormalized(t *testing.T) {
	s, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	for in, want := range map[string]string{"de": "de", "en": "en", "": "", "klingon": ""} {
		if err := s.Update(func(c *Config) { c.Language = in }); err != nil {
			t.Fatal(err)
		}
		if got := s.Get().Language; got != want {
			t.Errorf("language %q stored as %q, want %q", in, got, want)
		}
	}
}

// Configs from v0.2.6 and older carry the old default sound source name;
// they move to the new one (OBS's source is renamed by internal/obs). A
// name the streamer chose stays.
func TestLegacyOBSInputNameMoves(t *testing.T) {
	for _, tc := range []struct{ stored, want string }{
		{LegacyOBSInputName, DefaultOBSInputName},
		{"My Alerts", "My Alerts"},
	} {
		dir := t.TempDir()
		data := []byte(`{"obs":{"input_name":"` + tc.stored + `"}}`)
		if err := os.WriteFile(filepath.Join(dir, configFileName), data, 0o600); err != nil {
			t.Fatal(err)
		}
		s, err := Open(dir)
		if err != nil {
			t.Fatal(err)
		}
		if got := s.Get().OBS.InputName; got != tc.want {
			t.Errorf("stored %q: got %q, want %q", tc.stored, got, tc.want)
		}
	}
}

// Timers from v0.7.3 and older have one "message"; it becomes the first of
// their messages, in random order. Get never shares a timer's messages.
func TestLegacyTimerMessageMoves(t *testing.T) {
	dir := t.TempDir()
	data := []byte(`{"chat":{"timers":[{"id":"a","message":"Follow!","interval_minutes":15,"enabled":true}]}}`)
	if err := os.WriteFile(filepath.Join(dir, configFileName), data, 0o600); err != nil {
		t.Fatal(err)
	}
	s, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	tm := s.Get().Chat.Timers[0]
	if len(tm.Messages) != 1 || tm.Messages[0] != "Follow!" || tm.Message != "" || tm.Order != TimerRandom {
		t.Fatalf("got %+v", tm)
	}
	tm.Messages[0] = "changed"
	if s.Get().Chat.Timers[0].Messages[0] != "Follow!" {
		t.Fatal("Get must copy a timer's messages")
	}
}

// A config cut off by a crash must not stop darkbot from starting: it is
// set aside and darkbot starts with defaults.
func TestCorruptConfigIsSetAside(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, configFileName)
	if err := os.WriteFile(path, []byte(`{"master_volume": 0.5,`), 0o600); err != nil {
		t.Fatal(err)
	}
	s, err := Open(dir)
	if err != nil {
		t.Fatalf("a corrupt config must not stop the start: %v", err)
	}
	if s.Recovered == "" {
		t.Fatal("Recovered must say what happened")
	}
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("the corrupt file must be moved away: %v", err)
	}
	backups, _ := filepath.Glob(path + ".corrupt-*")
	if len(backups) != 1 {
		t.Fatalf("backups %v", backups)
	}
	if err := s.Update(func(c *Config) { c.MasterVolume = 0.7 }); err != nil {
		t.Fatal(err)
	}
	again, err := Open(dir)
	if err != nil || again.Recovered != "" || again.Get().MasterVolume != 0.7 {
		t.Fatalf("after saving, the config must load cleanly: %v %q", err, again.Recovered)
	}
}

// Settings saved before the overlay existed get its defaults, not all
// switches off; an unknown theme falls back to the first.
func TestNowPlayingDefaults(t *testing.T) {
	c := Config{}
	c.normalize()
	if c.NowPlaying != DefaultNowPlaying() || c.NowPlaying.Enabled {
		t.Fatalf("got %+v", c.NowPlaying)
	}
	c.NowPlaying = NowPlaying{Theme: "neon", ShowCover: false}
	c.normalize()
	if c.NowPlaying.Theme != NowPlayingThemes[0] || c.NowPlaying.ShowCover {
		t.Fatalf("theme fallback must keep the switches: %+v", c.NowPlaying)
	}
}

func TestClipsDefaults(t *testing.T) {
	c := Config{} // from before the clip command
	c.normalize()
	if c.Clips != DefaultClips() || c.Clips.Enabled || c.Clips.Name != "clips" {
		t.Fatalf("got %+v", c.Clips)
	}
	c.Clips = Clips{Name: "clip", Permission: "admin", CooldownSeconds: MaxCommandCooldown + 1, Enabled: true}
	c.normalize()
	if c.Clips.Permission != PermBroadcaster || c.Clips.CooldownSeconds != MaxCommandCooldown || !c.Clips.Enabled {
		t.Fatalf("got %+v", c.Clips)
	}
}

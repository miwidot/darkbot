package config

import (
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
		if fi.Mode().Perm() != filePerm {
			t.Fatalf("config perm = %v, want %v", fi.Mode().Perm(), os.FileMode(filePerm))
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

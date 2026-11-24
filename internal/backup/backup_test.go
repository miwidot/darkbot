package backup

import (
	"archive/zip"
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/miwidot/darkbot/internal/config"
	"github.com/miwidot/darkbot/internal/i18n"
)

var testTime = time.Date(2026, 11, 24, 21, 0, 0, 0, time.UTC)

func write(t *testing.T, path, content string) string {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// setup returns settings naming a reward sound, an event sound and
// picture, a command sound and an answer file (the sound shared by the
// reward and the command, so it is stored once).
func setup(t *testing.T) (config.Config, string) {
	t.Helper()
	dir := t.TempDir()
	horn := write(t, filepath.Join(dir, "air horn.mp3"), "ID3 horn")
	follow := write(t, filepath.Join(dir, "follow.ogg"), "OggS follow")
	gif := write(t, filepath.Join(dir, "follow.gif"), "GIF89a")
	lines := write(t, filepath.Join(dir, "answers.txt"), "one\ntwo\n")
	cfg := config.Default()
	cfg.OBS.Password = "hunter2"
	cfg.Sounds = []config.Sound{{ID: "s1", RewardTitle: "Horn", File: horn, Enabled: true}}
	cfg.Events = map[string]config.EventAction{"follow": {Enabled: true, Sound: follow, Visual: gif}}
	cfg.Chat.Commands = []config.ChatCommand{
		{ID: "c1", Name: "horn", Sound: horn, Enabled: true},
		{ID: "c2", Name: "gn", ResponseFile: lines, Enabled: true},
	}
	return cfg, dir
}

func export(t *testing.T, cfg config.Config) (string, ExportReport) {
	t.Helper()
	var buf bytes.Buffer
	rep, err := Export(&buf, cfg, "0.7.8", testTime)
	if err != nil {
		t.Fatal(err)
	}
	return write(t, filepath.Join(t.TempDir(), "backup.zip"), buf.String()), rep
}

func TestRoundTripSameComputer(t *testing.T) {
	cfg, _ := setup(t)
	zipPath, rep := export(t, cfg)
	if rep.Files != 4 || len(rep.Skipped) != 0 {
		t.Fatalf("export report %+v, want 4 files", rep)
	}
	got, rr, err := Restore(zipPath, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if rr.Kept != 4 || rr.Unpacked != 0 || len(rr.Missing) != 0 || rr.Version != "0.7.8" || !rr.Created.Equal(testTime) {
		t.Fatalf("restore report %+v", rr)
	}
	if got.Sounds[0].File != cfg.Sounds[0].File || got.Chat.Commands[1].ResponseFile != cfg.Chat.Commands[1].ResponseFile {
		t.Error("paths of files still in place must not change")
	}
	if got.OBS.Password != "" {
		t.Error("a password must never come back from a backup")
	}
}

func TestRoundTripOtherComputer(t *testing.T) {
	cfg, dir := setup(t)
	zipPath, _ := export(t, cfg)
	if err := os.RemoveAll(dir); err != nil { // the files are not on the new computer
		t.Fatal(err)
	}
	// The horn file is there, but different: it must not be taken.
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	write(t, cfg.Sounds[0].File, "another file")
	restoreDir := t.TempDir()
	got, rr, err := Restore(zipPath, restoreDir)
	if err != nil {
		t.Fatal(err)
	}
	if rr.Kept != 0 || rr.Unpacked != 4 {
		t.Fatalf("restore report %+v, want 4 unpacked", rr)
	}
	for _, want := range []struct{ path, content string }{
		{got.Sounds[0].File, "ID3 horn"},
		{got.Chat.Commands[0].Sound, "ID3 horn"},
		{got.Events["follow"].Sound, "OggS follow"},
		{got.Events["follow"].Visual, "GIF89a"},
		{got.Chat.Commands[1].ResponseFile, "one\ntwo\n"},
	} {
		if filepath.Dir(want.path) != restoreDir {
			t.Errorf("%s is not in the restore folder", want.path)
		}
		if b, err := os.ReadFile(want.path); err != nil || string(b) != want.content {
			t.Errorf("%s: %q, %v", want.path, b, err)
		}
	}
	if got.Sounds[0].File != got.Chat.Commands[0].Sound {
		t.Error("a file named twice must be unpacked once")
	}
	if !strings.HasSuffix(got.Sounds[0].File, "air_horn.mp3") {
		t.Errorf("unpacked name %q should keep a readable name", filepath.Base(got.Sounds[0].File))
	}
	// Restoring again doesn't overwrite: the second copy gets a new name.
	again, _, err := Restore(zipPath, restoreDir)
	if err != nil {
		t.Fatal(err)
	}
	if again.Sounds[0].File == got.Sounds[0].File {
		t.Error("a second restore must not overwrite the first one's files")
	}
}

func TestMissingFileIsDroppedAndTurnedOff(t *testing.T) {
	cfg, _ := setup(t)
	gone := cfg.Events["follow"].Visual
	if err := os.Remove(gone); err != nil {
		t.Fatal(err)
	}
	zipPath, rep := export(t, cfg)
	if rep.Files != 3 || len(rep.Skipped) != 1 || rep.Skipped[0].Path != gone {
		t.Fatalf("export report %+v, want the picture skipped", rep)
	}
	got, rr, err := Restore(zipPath, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	ev := got.Events["follow"]
	if len(rr.Missing) != 1 || ev.Visual != "" || ev.Enabled {
		t.Fatalf("missing picture: report %+v, event %+v", rr, ev)
	}
	if !got.Sounds[0].Enabled || !got.Chat.Commands[0].Enabled {
		t.Error("items with all their files must stay on")
	}
}

// A shared backup must not point darkbot at files of this computer that
// it doesn't contain (an answer file would be posted line by line).
func TestCraftedPathIsDropped(t *testing.T) {
	secret := write(t, filepath.Join(t.TempDir(), "passwords.txt"), "my secret")
	m := Manifest{Format: FormatVersion, Version: "evil", Config: config.Default()}
	m.Config.Chat.Commands = []config.ChatCommand{{ID: "x", Name: "leak", ResponseFile: secret, Enabled: true}}
	zipPath := writeZip(t, m, nil)
	got, rr, err := Restore(zipPath, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if c := got.Chat.Commands[0]; c.ResponseFile != "" || c.Enabled || len(rr.Missing) != 1 {
		t.Fatalf("crafted answer file kept: %+v", c)
	}
}

// writeZip builds a backup by hand with the given extra entries.
func writeZip(t *testing.T, m Manifest, entries map[string]string) string {
	t.Helper()
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	for name, content := range entries {
		w, err := zw.Create(name)
		if err != nil {
			t.Fatal(err)
		}
		w.Write([]byte(content))
	}
	data, _ := json.Marshal(m)
	w, _ := zw.Create(ManifestName)
	w.Write(data)
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return write(t, filepath.Join(t.TempDir(), "b.zip"), buf.String())
}

func wantKey(t *testing.T, name string, err error, key string) {
	t.Helper()
	if e, ok := errors.AsType[*i18n.Error](err); !ok || e.Key != key {
		t.Errorf("%s: err = %v, want %s", name, err, key)
	}
}

func TestDamagedBackups(t *testing.T) {
	cfg, dir := setup(t)
	good, _ := export(t, cfg)
	os.RemoveAll(dir) // force unpacking

	// A changed file (checksum mismatch) fails, and nothing stays behind.
	zr, err := zip.OpenReader(good)
	if err != nil {
		t.Fatal(err)
	}
	var m Manifest
	entries := map[string]string{}
	for _, f := range zr.File {
		rc, _ := f.Open()
		var b bytes.Buffer
		b.ReadFrom(rc)
		rc.Close()
		if f.Name == ManifestName {
			json.Unmarshal(b.Bytes(), &m)
			continue
		}
		entries[f.Name] = b.String()
	}
	zr.Close()
	first := m.Files[0].Entry
	tampered := map[string]string{}
	for k, v := range entries {
		tampered[k] = v
	}
	tampered[first] = strings.Repeat("x", len(entries[first]))
	restoreDir := t.TempDir()
	_, _, err = Restore(writeZip(t, m, tampered), restoreDir)
	wantKey(t, "checksum", err, "err.backupDamaged")
	if left, _ := os.ReadDir(restoreDir); len(left) != 0 {
		t.Errorf("a failed restore left %d files behind", len(left))
	}

	// An entry name with ../ is only a label: the file still lands in dir.
	evil := m
	evil.Files = []File{m.Files[0]}
	evil.Files[0].Entry = "files/../../../escape.mp3"
	_, _, err = Restore(writeZip(t, evil, map[string]string{evil.Files[0].Entry: entries[first]}), restoreDir)
	if err != nil {
		t.Fatalf("../ entry: %v", err)
	}
	if _, err := os.Stat(filepath.Join(restoreDir, "escape.mp3")); err != nil {
		t.Errorf("../ entry must be unpacked inside the restore folder: %v", err)
	}

	newer := m
	newer.Format = FormatVersion + 1
	_, _, err = Restore(writeZip(t, newer, entries), t.TempDir())
	wantKey(t, "newer format", err, "err.backupNewer")

	noEntry := m
	noEntry.Files = append([]File(nil), m.Files...)
	noEntry.Files[0].Entry = "files/999-missing.mp3"
	_, _, err = Restore(writeZip(t, noEntry, entries), t.TempDir())
	wantKey(t, "missing entry", err, "err.backupDamaged")

	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	zw.Create("something.txt")
	zw.Close()
	_, _, err = Restore(write(t, filepath.Join(t.TempDir(), "x.zip"), buf.String()), t.TempDir())
	wantKey(t, "no manifest", err, "err.backupNoManifest")

	_, _, err = Restore(write(t, filepath.Join(t.TempDir(), "x.zip"), "not a zip"), t.TempDir())
	wantKey(t, "not a zip", err, "err.backupNotZip")
}

func TestSafeName(t *testing.T) {
	for in, want := range map[string]string{
		`/home/a/air horn.mp3`:           "air_horn.mp3",
		`C:\Users\a\Böller (1).wav`:      "Böller__1_.wav",
		`../../.hidden`:                  "hidden",
		`/x/`:                            "file",
		`/x/` + strings.Repeat("a", 200): strings.Repeat("a", maxNameRunes),
	} {
		if got := safeName(in); got != want {
			t.Errorf("safeName(%q) = %q, want %q", in, got, want)
		}
	}
}

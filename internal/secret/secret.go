// Package secret stores login tokens in the OS credential store (Windows
// Credential Manager, macOS Keychain, Linux Secret Service). If the keyring is
// unavailable (e.g. headless Linux without a secret service) it falls back to
// a 0600 JSON file next to the config.
package secret

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"

	"github.com/zalando/go-keyring"
)

const (
	keyringService = "darkbot"
	filePerm       = 0o600
)

// Logger is the minimal logging interface this package needs.
type Logger interface {
	Warnf(format string, args ...any)
}

// Store keeps one JSON value under Key in the keyring, or in FallbackPath.
type Store struct {
	Key          string // keyring account name, e.g. "twitch-token"
	FallbackPath string
	Log          Logger
}

// Load decodes the stored value into v and reports whether one was found.
// An unreadable value is treated as "nothing stored" with a warning, so a
// damaged token never stops darkbot from starting; the next login replaces it.
//
// The fallback file only exists while the keyring failed at the last save
// (a working keyring save deletes it), so if it exists it is the newest
// value and wins over an older one still in the keyring. That matters for
// tokens: Twitch refresh tokens work once, so the older one is dead.
func (s *Store) Load(v any) (bool, error) {
	if _, err := os.Stat(s.FallbackPath); err == nil {
		return s.loadFile(v)
	}
	raw, err := keyring.Get(keyringService, s.Key)
	switch {
	case err == nil:
		if err := json.Unmarshal([]byte(raw), v); err != nil {
			s.Log.Warnf("Ignoring unreadable %s in the OS keyring (%v), please log in again", s.Key, err)
			return false, nil
		}
		return true, nil
	case errors.Is(err, keyring.ErrNotFound):
		return s.loadFile(v)
	default:
		s.Log.Warnf("OS keyring unavailable (%v), using file for %s", err, s.Key)
		return s.loadFile(v)
	}
}

// Save stores v.
func (s *Store) Save(v any) error {
	data, err := json.Marshal(v)
	if err != nil {
		return err
	}
	err = keyring.Set(keyringService, s.Key, string(data))
	if err == nil {
		_ = os.Remove(s.FallbackPath) // never keep a plaintext copy next to the keyring
		return nil
	}
	s.Log.Warnf("OS keyring unavailable (%v), saving %s to %s", err, s.Key, s.FallbackPath)
	if err := WriteFileAtomic(s.FallbackPath, data); err != nil {
		return err
	}
	_ = keyring.Delete(keyringService, s.Key) // best effort: an older copy must not linger
	return nil
}

// Delete removes the value from keyring and file.
func (s *Store) Delete() error {
	err := keyring.Delete(keyringService, s.Key)
	if errors.Is(err, keyring.ErrNotFound) {
		err = nil
	}
	if ferr := os.Remove(s.FallbackPath); ferr != nil && !errors.Is(ferr, os.ErrNotExist) && err == nil {
		err = ferr
	}
	return err
}

func (s *Store) loadFile(v any) (bool, error) {
	data, err := os.ReadFile(s.FallbackPath)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if err := json.Unmarshal(data, v); err != nil {
		s.Log.Warnf("Ignoring unreadable file %s (%v), please log in again", s.FallbackPath, err)
		return false, nil
	}
	return true, nil
}

// WriteFileAtomic writes via temp file + rename so a crash never leaves a
// half-written file, and enforces 0600 even if the file existed. The data
// is flushed to disk before the rename: otherwise a power cut right after
// a save can leave an empty file under the real name.
func WriteFileAtomic(path string, data []byte) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), filepath.Base(path)+".*.tmp")
	if err != nil {
		return err
	}
	name := tmp.Name()
	fail := func(err error) error {
		tmp.Close()
		os.Remove(name)
		return err
	}
	if err := tmp.Chmod(filePerm); err != nil && !errors.Is(err, errors.ErrUnsupported) {
		return fail(err)
	}
	if _, err := tmp.Write(data); err != nil {
		return fail(err)
	}
	if err := tmp.Sync(); err != nil {
		return fail(err)
	}
	if err := tmp.Close(); err != nil {
		os.Remove(name)
		return err
	}
	if err := os.Rename(name, path); err != nil {
		os.Remove(name)
		return err
	}
	syncDir(filepath.Dir(path))
	return nil
}

// syncDir makes the rename itself durable where the system supports it
// (not on Windows, where opening a directory for sync fails; NTFS
// journals the rename). Best effort: the file's data is already synced.
func syncDir(dir string) {
	d, err := os.Open(dir)
	if err != nil {
		return
	}
	_ = d.Sync()
	_ = d.Close()
}

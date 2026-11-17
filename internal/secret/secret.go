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
func (s *Store) Load(v any) (bool, error) {
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
	return WriteFileAtomic(s.FallbackPath, data)
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
// half-written file, and enforces 0600 even if the file existed.
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
	if err := tmp.Close(); err != nil {
		os.Remove(name)
		return err
	}
	if err := os.Rename(name, path); err != nil {
		os.Remove(name)
		return err
	}
	return nil
}

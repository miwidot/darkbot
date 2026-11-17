package twitch

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"

	"github.com/zalando/go-keyring"
)

const (
	keyringService = "darkbot"
	keyringUser    = "twitch-token"
	fileStorePerm  = 0o600
)

// KeyringStore keeps the token in the OS credential store (Windows
// Credential Manager, macOS Keychain, Linux Secret Service). If the keyring
// is unavailable (e.g. headless Linux without a secret service) it falls back
// to a 0600 JSON file and logs a warning once.
type KeyringStore struct {
	FallbackPath string
	Log          Logger
}

// Load returns the stored token or nil.
func (s *KeyringStore) Load() (*Token, error) {
	raw, err := keyring.Get(keyringService, keyringUser)
	switch {
	case err == nil:
		var t Token
		if err := json.Unmarshal([]byte(raw), &t); err != nil {
			s.Log.Warnf("Ignoring unreadable token in the OS keyring (%v), please log in again", err)
			return nil, nil
		}
		return &t, nil
	case errors.Is(err, keyring.ErrNotFound):
		return s.loadFile()
	default:
		s.Log.Warnf("OS keyring unavailable (%v), using token file", err)
		return s.loadFile()
	}
}

// Save stores the token.
func (s *KeyringStore) Save(t *Token) error {
	data, err := json.Marshal(t)
	if err != nil {
		return err
	}
	err = keyring.Set(keyringService, keyringUser, string(data))
	if err == nil {
		_ = os.Remove(s.FallbackPath) // never keep a plaintext copy next to the keyring
		return nil
	}
	s.Log.Warnf("OS keyring unavailable (%v), saving token to %s", err, s.FallbackPath)
	return writeFileAtomic(s.FallbackPath, data)
}

// writeFileAtomic writes via temp file + rename so a crash never leaves a
// half-written token file, and enforces 0600 even if the file existed.
func writeFileAtomic(path string, data []byte) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), filepath.Base(path)+".*.tmp")
	if err != nil {
		return err
	}
	name := tmp.Name()
	if err := tmp.Chmod(fileStorePerm); err != nil && !errors.Is(err, errors.ErrUnsupported) {
		tmp.Close()
		os.Remove(name)
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		os.Remove(name)
		return err
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

// Delete removes the token from keyring and file.
func (s *KeyringStore) Delete() error {
	err := keyring.Delete(keyringService, keyringUser)
	if errors.Is(err, keyring.ErrNotFound) {
		err = nil
	}
	if ferr := os.Remove(s.FallbackPath); ferr != nil && !errors.Is(ferr, os.ErrNotExist) && err == nil {
		err = ferr
	}
	return err
}

func (s *KeyringStore) loadFile() (*Token, error) {
	data, err := os.ReadFile(s.FallbackPath)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var t Token
	if err := json.Unmarshal(data, &t); err != nil {
		// A damaged token file must not stop darkbot from starting: treat it
		// as logged out; the next login overwrites it.
		s.Log.Warnf("Ignoring unreadable token file %s (%v), please log in again", s.FallbackPath, err)
		return nil, nil
	}
	return &t, nil
}

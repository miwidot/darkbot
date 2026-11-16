package twitch

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"

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
			return nil, fmt.Errorf("parse token from keyring: %w", err)
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
	if err := keyring.Set(keyringService, keyringUser, string(data)); err == nil {
		_ = os.Remove(s.FallbackPath) // never keep a plaintext copy next to the keyring
		return nil
	} else {
		s.Log.Warnf("OS keyring unavailable (%v), saving token to %s", err, s.FallbackPath)
	}
	return os.WriteFile(s.FallbackPath, data, fileStorePerm)
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
		return nil, fmt.Errorf("parse token file: %w", err)
	}
	return &t, nil
}

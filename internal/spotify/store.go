package spotify

import "github.com/miwidot/darkbot/internal/secret"

const keyringUser = "spotify-token"

// KeyringStore keeps the Spotify token in the OS keyring (file fallback).
type KeyringStore struct {
	FallbackPath string
	Log          secret.Logger
}

func (s *KeyringStore) store() *secret.Store {
	return &secret.Store{Key: keyringUser, FallbackPath: s.FallbackPath, Log: s.Log}
}

// Load returns the stored token or nil.
func (s *KeyringStore) Load() (*Token, error) {
	var t Token
	ok, err := s.store().Load(&t)
	if err != nil || !ok {
		return nil, err
	}
	return &t, nil
}

// Save stores the token.
func (s *KeyringStore) Save(t *Token) error { return s.store().Save(t) }

// Delete removes the token.
func (s *KeyringStore) Delete() error { return s.store().Delete() }

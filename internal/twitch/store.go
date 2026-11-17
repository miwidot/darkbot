package twitch

import "github.com/miwidot/darkbot/internal/secret"

// keyringUser is the keyring account name of the Twitch token. Kept from
// earlier versions so existing logins survive updates.
const keyringUser = "twitch-token"

// KeyringStore keeps the Twitch token in the OS keyring (file fallback).
type KeyringStore struct {
	FallbackPath string
	Log          Logger
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

// Delete removes the token from keyring and file.
func (s *KeyringStore) Delete() error { return s.store().Delete() }

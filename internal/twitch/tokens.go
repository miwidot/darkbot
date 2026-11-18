package twitch

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"
)

// How the automatic token refresh works (all three paths share one mutex,
// because device-flow refresh tokens are SINGLE-USE — two parallel refreshes
// would burn the token and log the user out):
//
//  1. Proactive: a ticker checks every refreshCheckEvery whether the access
//     token expires within refreshMargin and refreshes it before it dies.
//  2. Validation: Twitch requires validating the token on start and hourly.
//     If validation says 401, we refresh immediately.
//  3. Reactive: any Helix call that gets a 401 calls RefreshIfStale and
//     retries once.
//
// Only an explicit 400/401 from Twitch (ErrTokenRejected) logs the user out.
// Network errors are retried on the next tick, so a short internet outage or
// sleep/wake never throws the user out.
//
// Public-client refresh tokens expire 30 days after they were generated.
// Every refresh issues a new one, so as long as darkbot runs at least once
// in 30 days the login stays alive indefinitely.
const (
	refreshCheckEvery = time.Minute
	refreshMargin     = 15 * time.Minute
	validateEvery     = time.Hour
	refreshTimeout    = 20 * time.Second
	// RefreshTokenLifetime: refresh tokens of public clients (device flow)
	// expire 30 days after they were issued
	// (https://dev.twitch.tv/docs/authentication/refresh-tokens/).
	RefreshTokenLifetime = 30 * 24 * time.Hour
)

// Health is what the dashboard shows about the Twitch login.
type Health struct {
	LoggedIn     bool      `json:"loggedIn"`
	ExpiresAt    time.Time `json:"expiresAt"`    // access token
	NextRefresh  time.Time `json:"nextRefresh"`  // when darkbot renews it by itself
	LastRefresh  time.Time `json:"lastRefresh"`  // zero = not since start
	LastValidate time.Time `json:"lastValidate"` // zero = not yet
	LoginUntil   time.Time `json:"loginUntil"`   // refresh token expiry; zero = unknown
	LastError    string    `json:"lastError"`    // last failed refresh/validation, "" after success
}

// ErrNotLoggedIn is returned when no token is available.
var ErrNotLoggedIn = errors.New("not logged in to Twitch")

// TokenStore persists the token (OS keyring or file).
type TokenStore interface {
	Load() (*Token, error) // returns (nil, nil) when nothing is stored
	Save(*Token) error
	Delete() error
}

// Logger is the minimal logging interface the twitch package needs.
type Logger interface {
	Infof(format string, args ...any)
	Warnf(format string, args ...any)
	Errorf(format string, args ...any)
}

// TokenManager owns the current token and keeps it fresh.
type TokenManager struct {
	clientID string
	store    TokenStore
	log      Logger

	refreshMu sync.Mutex // serializes refreshes (single-use refresh tokens!)
	mu        sync.RWMutex
	tok       *Token

	onChange func(*Token) // called with nil on logout

	// health bookkeeping, guarded by mu
	lastRefresh  time.Time
	lastValidate time.Time
	lastError    string
}

// Health reports the state of the token for the dashboard.
func (m *TokenManager) Health() Health {
	m.mu.RLock()
	defer m.mu.RUnlock()
	h := Health{LastRefresh: m.lastRefresh, LastValidate: m.lastValidate, LastError: m.lastError}
	if m.tok == nil {
		return h
	}
	h.LoggedIn = true
	h.ExpiresAt = m.tok.ExpiresAt
	h.NextRefresh = m.tok.ExpiresAt.Add(-refreshMargin)
	if !m.tok.RefreshIssuedAt.IsZero() {
		h.LoginUntil = m.tok.RefreshIssuedAt.Add(RefreshTokenLifetime)
	}
	return h
}

func (m *TokenManager) noteError(err error) {
	m.mu.Lock()
	m.lastError = err.Error()
	m.mu.Unlock()
}

// RefreshNow renews the access token right away (dashboard button).
func (m *TokenManager) RefreshNow(ctx context.Context) error {
	cur := m.Current()
	if cur == nil {
		return ErrNotLoggedIn
	}
	return m.RefreshIfStale(ctx, cur.AccessToken)
}

// NewTokenManager loads a stored token (if any).
func NewTokenManager(clientID string, store TokenStore, log Logger) (*TokenManager, error) {
	tok, err := store.Load()
	if err != nil {
		return nil, fmt.Errorf("load twitch token: %w", err)
	}
	return &TokenManager{clientID: clientID, store: store, log: log, tok: tok}, nil
}

// OnChange registers a callback for login/refresh/logout.
func (m *TokenManager) OnChange(fn func(*Token)) {
	m.mu.Lock()
	m.onChange = fn
	m.mu.Unlock()
}

// SetClientID updates the client ID (used for device flow and refresh).
func (m *TokenManager) SetClientID(id string) {
	m.mu.Lock()
	m.clientID = id
	m.mu.Unlock()
}

// ClientID returns the configured client ID.
func (m *TokenManager) ClientID() string {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.clientID
}

// Current returns a copy of the current token or nil.
func (m *TokenManager) Current() *Token {
	m.mu.RLock()
	defer m.mu.RUnlock()
	if m.tok == nil {
		return nil
	}
	t := *m.tok
	return &t
}

// AccessToken returns the current access token.
func (m *TokenManager) AccessToken() (string, error) {
	t := m.Current()
	if t == nil || t.AccessToken == "" {
		return "", ErrNotLoggedIn
	}
	return t.AccessToken, nil
}

// Login completes a device-code login: validates the new token to learn the
// user ID/login, stores it and notifies listeners.
func (m *TokenManager) Login(ctx context.Context, tok *Token) error {
	// Hold refreshMu so a refresh still running for the previous token can't
	// overwrite the new login afterwards.
	m.refreshMu.Lock()
	defer m.refreshMu.Unlock()
	v, err := validateToken(ctx, tok.AccessToken)
	if err != nil {
		return fmt.Errorf("validate new token: %w", err)
	}
	tok.UserID, tok.Login, tok.Scopes = v.UserID, v.Login, v.Scopes
	return m.set(tok)
}

// Logout deletes the token and revokes it at Twitch, so a copy left
// anywhere (a backup of the config dir, the old keyring entry) is useless.
// It waits for a running refresh, so a refresh can't write a fresh token
// back after the user logged out. A failed revoke is only logged: the local
// logout counts.
func (m *TokenManager) Logout() error {
	m.refreshMu.Lock()
	defer m.refreshMu.Unlock()
	m.mu.RLock()
	tok := m.tok
	m.mu.RUnlock()
	err := m.logoutLocked()
	if tok != nil {
		ctx, cancel := context.WithTimeout(context.Background(), revokeTimeout)
		defer cancel()
		if rerr := RevokeToken(ctx, m.clientID, tok.AccessToken); rerr != nil {
			m.log.Warnf("Logged out, but Twitch could not be told to invalidate the login: %v", rerr)
		}
	}
	return err
}

// revokeTimeout bounds telling Twitch about a logout.
const revokeTimeout = 10 * time.Second

// logoutLocked must be called with refreshMu held.
func (m *TokenManager) logoutLocked() error {
	m.mu.Lock()
	m.tok = nil
	m.lastRefresh, m.lastValidate, m.lastError = time.Time{}, time.Time{}, ""
	cb := m.onChange
	m.mu.Unlock()
	err := m.store.Delete()
	if cb != nil {
		cb(nil)
	}
	return err
}

func (m *TokenManager) set(tok *Token) error {
	m.mu.Lock()
	m.tok = tok
	cb := m.onChange
	m.mu.Unlock()
	// Persist BEFORE anyone uses it: the old refresh token is already burned.
	err := m.store.Save(tok)
	if cb != nil {
		t := *tok
		cb(&t)
	}
	if err != nil {
		return fmt.Errorf("save twitch token: %w", err)
	}
	return nil
}

// RefreshIfStale refreshes the token unless another goroutine already did so
// since the caller read staleAccess. This dedupes concurrent 401 handling.
func (m *TokenManager) RefreshIfStale(ctx context.Context, staleAccess string) error {
	m.refreshMu.Lock()
	defer m.refreshMu.Unlock()

	cur := m.Current()
	if cur == nil {
		return ErrNotLoggedIn
	}
	if staleAccess != "" && cur.AccessToken != staleAccess {
		return nil // someone else refreshed already
	}
	return m.refreshLocked(ctx, cur)
}

// refreshLocked must be called with refreshMu held.
func (m *TokenManager) refreshLocked(ctx context.Context, cur *Token) error {
	if cur.RefreshToken == "" {
		return m.reject(errors.New("no refresh token stored"))
	}
	ctx, cancel := context.WithTimeout(ctx, refreshTimeout)
	defer cancel()

	next, err := refreshToken(ctx, m.ClientID(), cur.RefreshToken)
	if err != nil {
		if errors.Is(err, ErrTokenRejected) {
			return m.reject(err)
		}
		m.noteError(err)
		return err // transient: keep the old token, retry later
	}
	next.UserID, next.Login = cur.UserID, cur.Login
	m.mu.Lock()
	m.lastRefresh, m.lastError = time.Now(), ""
	m.mu.Unlock()
	if err := m.set(next); err != nil {
		m.log.Errorf("Twitch token refreshed but could not be saved: %v", err)
		return err
	}
	m.log.Infof("Twitch token refreshed (valid until %s)", next.ExpiresAt.Local().Format("15:04"))
	return nil
}

func (m *TokenManager) reject(cause error) error {
	m.log.Errorf("Twitch login expired or was revoked, please log in again (%v)", cause)
	if err := m.logoutLocked(); err != nil { // reject runs with refreshMu held
		m.log.Warnf("delete stored token: %v", err)
	}
	return fmt.Errorf("%w: %v", ErrTokenRejected, cause)
}

// validate runs the mandatory validation and refreshes on 401.
func (m *TokenManager) validate(ctx context.Context) {
	cur := m.Current()
	if cur == nil {
		return
	}
	vctx, cancel := context.WithTimeout(ctx, refreshTimeout)
	v, err := validateToken(vctx, cur.AccessToken)
	cancel()
	switch {
	case err == nil:
		// Keep ExpiresAt in sync with what Twitch says.
		m.mu.Lock()
		if m.tok != nil && m.tok.AccessToken == cur.AccessToken {
			m.tok.ExpiresAt = time.Now().Add(time.Duration(v.ExpiresIn) * time.Second)
			m.tok.UserID, m.tok.Login = v.UserID, v.Login
			m.tok.Scopes = v.Scopes
		}
		m.lastValidate, m.lastError = time.Now(), ""
		cb := m.onChange
		var snapshot *Token
		if m.tok != nil {
			t := *m.tok
			snapshot = &t
		}
		m.mu.Unlock()
		if cb != nil && snapshot != nil {
			cb(snapshot) // let the dashboard show the new check time
		}
	case errors.Is(err, ErrTokenRejected):
		m.log.Infof("Twitch access token invalid, refreshing")
		if err := m.RefreshIfStale(ctx, cur.AccessToken); err != nil && !errors.Is(err, ErrTokenRejected) {
			m.log.Warnf("Twitch token refresh failed, will retry: %v", err)
		}
	default:
		m.noteError(err)
		m.log.Warnf("Twitch token validation failed (network?), will retry: %v", err)
	}
}

// Run keeps the token alive until ctx is cancelled. Call it once in a goroutine.
func (m *TokenManager) Run(ctx context.Context) {
	m.validate(ctx) // on start, as required by Twitch
	lastValidate := time.Now()

	ticker := time.NewTicker(refreshCheckEvery)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
		cur := m.Current()
		if cur == nil {
			continue
		}
		if time.Until(cur.ExpiresAt) < refreshMargin {
			if err := m.RefreshIfStale(ctx, cur.AccessToken); err != nil && !errors.Is(err, ErrTokenRejected) {
				m.log.Warnf("Twitch token refresh failed, will retry: %v", err)
			}
		}
		if time.Since(lastValidate) >= validateEvery {
			m.validate(ctx)
			lastValidate = time.Now()
		}
	}
}

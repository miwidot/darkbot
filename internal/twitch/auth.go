// Package twitch implements the small slice of the Twitch API darkbot needs:
// device-code login, token refresh/validation, a few Helix calls and an
// EventSub WebSocket client for channel point redemptions.
//
// Docs (verified 2026-11-16):
//   - Device code flow: https://dev.twitch.tv/docs/authentication/getting-tokens-oauth/#device-code-grant-flow
//   - Refresh tokens:   https://dev.twitch.tv/docs/authentication/refresh-tokens/
//   - Validate tokens:  https://dev.twitch.tv/docs/authentication/validate-tokens/
package twitch

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// Endpoints are variables only so tests can point them at a fake server.
var (
	deviceURL   = "https://id.twitch.tv/oauth2/device"
	tokenURL    = "https://id.twitch.tv/oauth2/token"
	validateURL = "https://id.twitch.tv/oauth2/validate"
	// pollUnit is the unit of the "interval" field Twitch returns (seconds).
	pollUnit = time.Second
)

const (
	deviceGrantType = "urn:ietf:params:oauth:grant-type:device_code"

	// authTimeout bounds every call to id.twitch.tv so a stalled connection
	// (e.g. right after wake from sleep) can never hang the app.
	authTimeout = 15 * time.Second
	// maxErrorBody limits how much of an error response we read/log.
	maxErrorBody = 4 << 10
	// slowDownStep is how much we add to the poll interval on "slow_down"
	// (RFC 8628 §3.5 value; Twitch doesn't document it but we handle it).
	slowDownStep = 5 * time.Second
)

// Scopes darkbot requests (https://dev.twitch.tv/docs/authentication/scopes/):
//   - channel:read:redemptions: receive redemptions (EventSub) and list rewards
//   - channel:manage:redemptions: create/update/delete darkbot's own rewards
//     and fulfill or reject their redemptions
//   - user:read:chat: receive chat messages (EventSub channel.chat.message)
//     for the chat commands (song requests)
//   - user:write:chat: answer chat commands (Helix Send Chat Message)
const (
	ScopeReadRedemptions   = "channel:read:redemptions"
	ScopeManageRedemptions = "channel:manage:redemptions"
	ScopeReadChat          = "user:read:chat"
	ScopeWriteChat         = "user:write:chat"
)

// Scopes is everything darkbot asks for at login.
var Scopes = []string{ScopeReadRedemptions, ScopeManageRedemptions, ScopeReadChat, ScopeWriteChat}

// HasScope reports whether the token was granted scope.
func (t *Token) HasScope(scope string) bool {
	for _, s := range t.Scopes {
		if s == scope {
			return true
		}
	}
	return false
}

// HasScopes reports whether the token was granted every scope darkbot needs.
// Tokens from older darkbot versions lack some; the UI then asks the user to
// reconnect once.
func (t *Token) HasScopes() bool {
	for _, s := range Scopes {
		if !t.HasScope(s) {
			return false
		}
	}
	return true
}

// ErrTokenRejected means Twitch actively refused the token (HTTP 400/401):
// it is expired, revoked or invalid and retrying will not help — the user has
// to log in again. All other errors (network, timeout, 5xx, 429) are transient.
var ErrTokenRejected = errors.New("token rejected by Twitch")

var authHTTP = &http.Client{Timeout: authTimeout}

// Token is a user access token plus the data needed to keep it alive.
type Token struct {
	AccessToken  string    `json:"access_token"`
	RefreshToken string    `json:"refresh_token"`
	ExpiresAt    time.Time `json:"expires_at"`
	Scopes       []string  `json:"scopes"`
	UserID       string    `json:"user_id"`
	Login        string    `json:"login"`
	// RefreshIssuedAt is when the current refresh token was issued; public
	// client refresh tokens expire RefreshTokenLifetime later.
	RefreshIssuedAt time.Time `json:"refresh_issued_at,omitzero"`
}

// tokenResponse is the JSON body of a successful /oauth2/token call.
type tokenResponse struct {
	AccessToken  string   `json:"access_token"`
	RefreshToken string   `json:"refresh_token"`
	ExpiresIn    int      `json:"expires_in"`
	Scope        []string `json:"scope"`
	TokenType    string   `json:"token_type"`
}

func (r tokenResponse) toToken(now time.Time) *Token {
	return &Token{
		AccessToken:     r.AccessToken,
		RefreshToken:    r.RefreshToken,
		ExpiresAt:       now.Add(time.Duration(r.ExpiresIn) * time.Second),
		Scopes:          r.Scope,
		RefreshIssuedAt: now,
	}
}

// twitchError is Twitch's error body: {"status":400,"message":"..."}.
type twitchError struct {
	Status  int    `json:"status"`
	Message string `json:"message"`
	Error   string `json:"error"`
}

func readTwitchError(resp *http.Response) twitchError {
	body, _ := io.ReadAll(io.LimitReader(resp.Body, maxErrorBody))
	var te twitchError
	if json.Unmarshal(body, &te) != nil || te.Message == "" {
		te.Message = strings.TrimSpace(string(body))
	}
	te.Status = resp.StatusCode
	return te
}

func classify(op string, te twitchError) error {
	if te.Status == http.StatusBadRequest || te.Status == http.StatusUnauthorized {
		return fmt.Errorf("%s: %w (status %d: %s)", op, ErrTokenRejected, te.Status, te.Message)
	}
	return fmt.Errorf("%s: twitch answered status %d: %s", op, te.Status, te.Message)
}

func postForm(ctx context.Context, endpoint string, form url.Values) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, strings.NewReader(form.Encode()))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	return authHTTP.Do(req)
}

// DeviceCode is what the user needs to authorize darkbot.
type DeviceCode struct {
	DeviceCode      string `json:"device_code"`
	UserCode        string `json:"user_code"`
	VerificationURI string `json:"verification_uri"`
	ExpiresIn       int    `json:"expires_in"`
	Interval        int    `json:"interval"`
}

// StartDeviceFlow requests a device code (step 1 of the device code flow).
func StartDeviceFlow(ctx context.Context, clientID string) (*DeviceCode, error) {
	if clientID == "" {
		return nil, errors.New("twitch client ID is empty")
	}
	resp, err := postForm(ctx, deviceURL, url.Values{
		"client_id": {clientID},
		"scopes":    {strings.Join(Scopes, " ")},
	})
	if err != nil {
		return nil, fmt.Errorf("start device flow: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		te := readTwitchError(resp)
		return nil, fmt.Errorf("start device flow: status %d: %s", te.Status, te.Message)
	}
	var dc DeviceCode
	if err := json.NewDecoder(resp.Body).Decode(&dc); err != nil {
		return nil, fmt.Errorf("parse device code: %w", err)
	}
	if dc.Interval <= 0 {
		dc.Interval = 5 // documented example value
	}
	return &dc, nil
}

// PollDeviceFlow polls until the user authorized the device code, it expired,
// or ctx is cancelled (step 2 of the device code flow).
func PollDeviceFlow(ctx context.Context, clientID string, dc *DeviceCode) (*Token, error) {
	interval := time.Duration(dc.Interval) * pollUnit
	deadline := time.Now().Add(time.Duration(dc.ExpiresIn) * pollUnit)

	for {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(interval):
		}
		if time.Now().After(deadline) {
			return nil, errors.New("login code expired, please start the login again")
		}

		resp, err := postForm(ctx, tokenURL, url.Values{
			"client_id":   {clientID},
			"scopes":      {strings.Join(Scopes, " ")},
			"device_code": {dc.DeviceCode},
			"grant_type":  {deviceGrantType},
		})
		if err != nil {
			// Network hiccup: keep polling until the code expires.
			continue
		}
		if resp.StatusCode == http.StatusOK {
			var tr tokenResponse
			err := json.NewDecoder(resp.Body).Decode(&tr)
			resp.Body.Close()
			if err != nil {
				return nil, fmt.Errorf("parse token: %w", err)
			}
			return tr.toToken(time.Now()), nil
		}
		te := readTwitchError(resp)
		resp.Body.Close()

		switch te.Message {
		case "authorization_pending":
			// user hasn't confirmed yet
		case "slow_down":
			interval += slowDownStep * pollUnit / time.Second
		default:
			return nil, fmt.Errorf("login failed: status %d: %s", te.Status, te.Message)
		}
	}
}

// refreshToken exchanges a refresh token for a new token pair. Public
// clients (device flow) need no client_secret. Device-flow refresh tokens are
// single-use: the response contains a NEW refresh token that must be stored.
func refreshToken(ctx context.Context, clientID, refresh string) (*Token, error) {
	resp, err := postForm(ctx, tokenURL, url.Values{
		"client_id":     {clientID},
		"grant_type":    {"refresh_token"},
		"refresh_token": {refresh},
	})
	if err != nil {
		return nil, fmt.Errorf("refresh token: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, classify("refresh token", readTwitchError(resp))
	}
	var tr tokenResponse
	if err := json.NewDecoder(resp.Body).Decode(&tr); err != nil {
		return nil, fmt.Errorf("parse refresh response: %w", err)
	}
	return tr.toToken(time.Now()), nil
}

// validation is the body of a successful /oauth2/validate call.
type validation struct {
	ClientID  string   `json:"client_id"`
	Login     string   `json:"login"`
	Scopes    []string `json:"scopes"`
	UserID    string   `json:"user_id"`
	ExpiresIn int      `json:"expires_in"`
}

// validateToken checks an access token. Twitch requires apps to do this on
// start and then hourly.
func validateToken(ctx context.Context, access string) (*validation, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, validateURL, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "OAuth "+access)
	resp, err := authHTTP.Do(req)
	if err != nil {
		return nil, fmt.Errorf("validate token: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, classify("validate token", readTwitchError(resp))
	}
	var v validation
	if err := json.NewDecoder(resp.Body).Decode(&v); err != nil {
		return nil, fmt.Errorf("parse validate response: %w", err)
	}
	return &v, nil
}

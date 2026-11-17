// Package spotify connects darkbot to the streamer's own Spotify app.
//
// Spotify apps in Development Mode serve at most 5 users and extended quota
// is only granted to large organisations
// (https://developer.spotify.com/documentation/web-api/concepts/quota-modes),
// so darkbot cannot ship one shared Client ID. Every streamer registers
// their own app and enters its Client ID.
//
// Login: Authorization Code with PKCE, the flow for apps that cannot keep a
// client secret (verified 2026-11-17,
// https://developer.spotify.com/documentation/web-api/tutorials/code-pkce-flow).
//
// No port: Spotify only redirects to a registered URI, and for apps on the
// user's computer that is a loopback address
// (https://developer.spotify.com/documentation/web-api/concepts/redirect_uri,
// "localhost" is not allowed, 127.0.0.1 is). darkbot does NOT listen there.
// The browser shows "can't connect", and the user pastes the address from
// the address bar into darkbot. The code in it is useless without the PKCE
// verifier that only darkbot holds, so a program that happens to listen on
// that port cannot use it.
package spotify

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/miwidot/darkbot/internal/i18n"
)

// RedirectURI must be registered exactly like this in the streamer's Spotify
// app. The port is arbitrary (nothing listens there); it only has to be the
// same in the app settings and here.
const RedirectURI = "http://127.0.0.1:43117/callback"

// Scopes darkbot asks for (Spotify Web API reference):
//   - user-modify-playback-state: add to queue, skip (both Spotify Premium only)
//   - user-read-playback-state + user-read-currently-playing: read the queue
//     and the current song
var Scopes = []string{"user-modify-playback-state", "user-read-playback-state", "user-read-currently-playing"}

// Endpoints are variables so tests can point them at a local server.
var (
	authorizeURL = "https://accounts.spotify.com/authorize"
	tokenURL     = "https://accounts.spotify.com/api/token"
)

const (
	// verifierBytes random bytes give a 64 character base64url verifier
	// (Spotify allows 43–128 characters).
	verifierBytes = 48
	stateBytes    = 16
	authTimeout   = 20 * time.Second
	maxErrorBody  = 4 << 10
)

// ErrRejected means Spotify refused the login or refresh token
// (invalid_grant): the user has to connect again.
var ErrRejected = i18n.E("err.spotifyRejected")

var authHTTP = &http.Client{Timeout: authTimeout}

// Token is a Spotify user token.
type Token struct {
	AccessToken  string    `json:"access_token"`
	RefreshToken string    `json:"refresh_token"`
	ExpiresAt    time.Time `json:"expires_at"`
	Scope        string    `json:"scope"`
	UserID       string    `json:"user_id"`
	DisplayName  string    `json:"display_name"`
}

// PendingLogin is a started login waiting for the pasted redirect address.
type PendingLogin struct {
	ClientID string
	Verifier string
	State    string
	URL      string // open this in the browser
}

func randomString(n int) (string, error) {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

// NewLogin prepares the PKCE values and the authorize URL.
func NewLogin(clientID string) (*PendingLogin, error) {
	clientID = strings.TrimSpace(clientID)
	if clientID == "" {
		return nil, i18n.E("err.spotifyClientID")
	}
	verifier, err := randomString(verifierBytes)
	if err != nil {
		return nil, err
	}
	state, err := randomString(stateBytes)
	if err != nil {
		return nil, err
	}
	sum := sha256.Sum256([]byte(verifier))
	q := url.Values{
		"client_id":             {clientID},
		"response_type":         {"code"},
		"redirect_uri":          {RedirectURI},
		"state":                 {state},
		"scope":                 {strings.Join(Scopes, " ")},
		"code_challenge_method": {"S256"},
		"code_challenge":        {base64.RawURLEncoding.EncodeToString(sum[:])},
	}
	return &PendingLogin{ClientID: clientID, Verifier: verifier, State: state, URL: authorizeURL + "?" + q.Encode()}, nil
}

// CodeFromPaste extracts the authorization code from what the user pasted:
// the full address from the browser (state is checked) or just the code.
func (p *PendingLogin) CodeFromPaste(pasted string) (string, error) {
	pasted = strings.TrimSpace(pasted)
	if pasted == "" {
		return "", i18n.E("err.spotifyPasteEmpty")
	}
	if !strings.Contains(pasted, "?") && !strings.Contains(pasted, "=") {
		return pasted, nil // the bare code
	}
	u, err := url.Parse(pasted)
	if err != nil {
		return "", i18n.E("err.spotifyPasteInvalid")
	}
	q := u.Query()
	if e := q.Get("error"); e != "" {
		return "", i18n.E("err.spotifyDenied", "reason", e)
	}
	if q.Get("state") != p.State {
		return "", i18n.E("err.spotifyPasteOld")
	}
	code := q.Get("code")
	if code == "" {
		return "", i18n.E("err.spotifyPasteInvalid")
	}
	return code, nil
}

type tokenResponse struct {
	AccessToken  string `json:"access_token"`
	TokenType    string `json:"token_type"`
	Scope        string `json:"scope"`
	ExpiresIn    int    `json:"expires_in"`
	RefreshToken string `json:"refresh_token"`
}

// Exchange trades the code for a token.
func (p *PendingLogin) Exchange(ctx context.Context, code string) (*Token, error) {
	return postToken(ctx, url.Values{
		"grant_type":    {"authorization_code"},
		"code":          {code},
		"redirect_uri":  {RedirectURI},
		"client_id":     {p.ClientID},
		"code_verifier": {p.Verifier},
	}, "")
}

// refresh gets a new access token. Spotify may or may not send a new
// refresh token; without one the old one stays valid
// (https://developer.spotify.com/documentation/web-api/tutorials/refreshing-tokens).
func refresh(ctx context.Context, clientID, refreshToken string) (*Token, error) {
	return postToken(ctx, url.Values{
		"grant_type":    {"refresh_token"},
		"refresh_token": {refreshToken},
		"client_id":     {clientID},
	}, refreshToken)
}

func postToken(ctx context.Context, form url.Values, oldRefresh string) (*Token, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, tokenURL, strings.NewReader(form.Encode()))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := authHTTP.Do(req)
	if err != nil {
		return nil, fmt.Errorf("spotify token request: %w", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, maxErrorBody))
	if resp.StatusCode != http.StatusOK {
		var e struct {
			Error       string `json:"error"`
			Description string `json:"error_description"`
		}
		_ = json.Unmarshal(body, &e)
		// 400 invalid_grant / invalid_client: retrying won't help.
		if resp.StatusCode == http.StatusBadRequest || resp.StatusCode == http.StatusUnauthorized {
			return nil, fmt.Errorf("%w (%s: %s)", ErrRejected, e.Error, e.Description)
		}
		return nil, fmt.Errorf("spotify token request: status %d: %s %s", resp.StatusCode, e.Error, e.Description)
	}
	var tr tokenResponse
	if err := json.Unmarshal(body, &tr); err != nil {
		return nil, fmt.Errorf("parse spotify token: %w", err)
	}
	if tr.AccessToken == "" {
		return nil, errors.New("spotify sent no access token")
	}
	t := &Token{
		AccessToken:  tr.AccessToken,
		RefreshToken: tr.RefreshToken,
		ExpiresAt:    time.Now().Add(time.Duration(tr.ExpiresIn) * time.Second),
		Scope:        tr.Scope,
	}
	if t.RefreshToken == "" {
		t.RefreshToken = oldRefresh
	}
	return t, nil
}

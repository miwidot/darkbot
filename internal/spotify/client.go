package spotify

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/miwidot/darkbot/internal/i18n"
)

// Web API reference (verified 2026-11-17):
// https://developer.spotify.com/documentation/web-api/reference/
var apiBase = "https://api.spotify.com/v1"

const (
	apiTimeout = 15 * time.Second
	// refreshMargin renews the access token (valid 1 hour) a bit early so a
	// request never starts with a token that dies on the way.
	refreshMargin = 5 * time.Minute
	// searchLimit: we only use the best match.
	searchLimit = 1
)

// Errors the UI and chat replies translate.
var (
	ErrNotConnected   = i18n.E("err.spotifyOffline")
	ErrNoDevice       = i18n.E("err.spotifyNoDevice")
	ErrPremium        = i18n.E("err.spotifyPremium")
	ErrNotFound       = i18n.E("err.spotifyNotFound")
	ErrRateLimited    = i18n.E("err.spotifyRateLimit")
	ErrNotTrackLink   = i18n.E("err.spotifyNotTrack")
	ErrUnsupportedURL = i18n.E("err.spotifyLinkType")
)

// TokenStore persists the token (OS keyring or file).
type TokenStore interface {
	Load() (*Token, error) // (nil, nil) when nothing is stored
	Save(*Token) error
	Delete() error
}

// Logger is the minimal logging interface this package needs.
type Logger interface {
	Infof(format string, args ...any)
	Warnf(format string, args ...any)
	Errorf(format string, args ...any)
}

// Client talks to the Web API as the connected user and keeps the token
// fresh. Safe for concurrent use.
type Client struct {
	store TokenStore
	log   Logger
	http  *http.Client

	refreshMu sync.Mutex // one refresh at a time
	mu        sync.RWMutex
	clientID  string
	tok       *Token
	onChange  func()

	// health bookkeeping, guarded by mu
	lastRefresh time.Time
	lastError   string
}

// Health is what the dashboard shows about the Spotify login.
type Health struct {
	LoggedIn    bool      `json:"loggedIn"`
	ExpiresAt   time.Time `json:"expiresAt"`   // access token; renewed on the next use after this
	LastRefresh time.Time `json:"lastRefresh"` // zero = not since start
	LoginUntil  time.Time `json:"loginUntil"`  // connect again after this; zero = unknown
	LastError   string    `json:"lastError"`
}

// Health reports the state of the token for the dashboard.
func (c *Client) Health() Health {
	c.mu.RLock()
	defer c.mu.RUnlock()
	h := Health{LastRefresh: c.lastRefresh, LastError: c.lastError}
	if c.tok == nil {
		return h
	}
	h.LoggedIn = true
	h.ExpiresAt = c.tok.ExpiresAt
	if !c.tok.AuthorizedAt.IsZero() {
		h.LoginUntil = c.tok.AuthorizedAt.AddDate(0, RefreshTokenLifetimeMonths, 0)
	}
	return h
}

// RefreshNow renews the access token right away (dashboard button).
func (c *Client) RefreshNow(ctx context.Context) error {
	cur := c.Current()
	if cur == nil {
		return ErrNotConnected
	}
	_, err := c.accessToken(ctx, cur.AccessToken)
	return err
}

// NewClient loads a stored token (if any).
func NewClient(clientID string, store TokenStore, log Logger) (*Client, error) {
	tok, err := store.Load()
	if err != nil {
		return nil, fmt.Errorf("load spotify token: %w", err)
	}
	return &Client{store: store, log: log, http: &http.Client{Timeout: apiTimeout}, clientID: clientID, tok: tok}, nil
}

// OnChange is called after login, logout and when Spotify rejected the login.
func (c *Client) OnChange(fn func()) {
	c.mu.Lock()
	c.onChange = fn
	c.mu.Unlock()
}

func (c *Client) changed() {
	c.mu.RLock()
	fn := c.onChange
	c.mu.RUnlock()
	if fn != nil {
		fn()
	}
}

// SetClientID changes the app used for refreshing (a new login follows).
func (c *Client) SetClientID(id string) {
	c.mu.Lock()
	c.clientID = strings.TrimSpace(id)
	c.mu.Unlock()
}

// Current returns a copy of the token or nil.
func (c *Client) Current() *Token {
	c.mu.RLock()
	defer c.mu.RUnlock()
	if c.tok == nil {
		return nil
	}
	t := *c.tok
	return &t
}

// Login finishes a pending login: exchanges the code, reads the profile and
// stores the token.
func (c *Client) Login(ctx context.Context, p *PendingLogin, code string) (*Token, error) {
	tok, err := p.Exchange(ctx, code)
	if err != nil {
		return nil, err
	}
	me, err := c.profile(ctx, tok.AccessToken)
	if err != nil {
		return nil, err
	}
	tok.UserID, tok.DisplayName = me.ID, me.DisplayName
	tok.AuthorizedAt = time.Now()
	if tok.DisplayName == "" {
		tok.DisplayName = me.ID
	}
	c.refreshMu.Lock()
	defer c.refreshMu.Unlock()
	if err := c.store.Save(tok); err != nil {
		return nil, err
	}
	c.mu.Lock()
	c.tok, c.clientID = tok, p.ClientID
	c.mu.Unlock()
	c.changed()
	return tok, nil
}

// Logout forgets the token.
func (c *Client) Logout() error {
	c.refreshMu.Lock()
	err := c.logoutLocked()
	c.refreshMu.Unlock()
	c.changed()
	return err
}

func (c *Client) logoutLocked() error {
	c.mu.Lock()
	c.tok = nil
	c.lastRefresh, c.lastError = time.Time{}, ""
	c.mu.Unlock()
	return c.store.Delete()
}

// accessToken returns a valid access token, refreshing it when it is about
// to expire (force: the API just said 401).
//
// rejected is the access token Spotify just refused (401), or "" for a
// normal call. A refresh only happens if the rejected token is still the
// current one: otherwise another call already renewed it while this one
// waited, and refreshing again would waste (or, with rotation, burn) the
// refresh token.
func (c *Client) accessToken(ctx context.Context, rejected string) (string, error) {
	cur := c.Current()
	if cur == nil {
		return "", ErrNotConnected
	}
	force := rejected != ""
	if force && cur.AccessToken != rejected {
		return cur.AccessToken, nil
	}
	if !force && time.Until(cur.ExpiresAt) > refreshMargin {
		return cur.AccessToken, nil
	}
	c.refreshMu.Lock()
	defer c.refreshMu.Unlock()
	// Another caller may have refreshed while we waited for the lock.
	if now := c.Current(); now == nil {
		return "", ErrNotConnected
	} else if now.AccessToken != cur.AccessToken || (!force && time.Until(now.ExpiresAt) > refreshMargin) {
		return now.AccessToken, nil
	}
	c.mu.RLock()
	clientID := c.clientID
	c.mu.RUnlock()
	// Not tied to the caller's context: if a chat command's timeout hit after
	// Spotify issued (and maybe rotated) the refresh token, the new token
	// would be lost and the next refresh would fail with invalid_grant.
	rctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), authTimeout)
	defer cancel()
	nt, err := refresh(rctx, clientID, cur.RefreshToken)
	if errors.Is(err, ErrRejected) {
		c.log.Errorf("Spotify rejected the login (%v), please connect Spotify again", err)
		_ = c.logoutLocked()
		go c.changed()
		return "", err
	}
	if err != nil {
		c.mu.Lock()
		c.lastError = err.Error()
		c.mu.Unlock()
		return "", err
	}
	nt.UserID, nt.DisplayName, nt.AuthorizedAt = cur.UserID, cur.DisplayName, cur.AuthorizedAt
	if err := c.store.Save(nt); err != nil {
		c.log.Warnf("Could not save the refreshed Spotify token: %v", err)
	}
	c.mu.Lock()
	c.tok = nt
	c.lastRefresh, c.lastError = time.Now(), ""
	c.mu.Unlock()
	go c.changed() // dashboard shows the new times
	return nt.AccessToken, nil
}

type apiError struct {
	Error struct {
		Status  int    `json:"status"`
		Message string `json:"message"`
		Reason  string `json:"reason"`
	} `json:"error"`
}

// do calls the Web API; on 401 it refreshes once and retries.
func (c *Client) do(ctx context.Context, method, path string, query url.Values, out any) error {
	rejected := ""
	for attempt := 0; attempt < 2; attempt++ {
		access, err := c.accessToken(ctx, rejected)
		if err != nil {
			return err
		}
		err = c.doWith(ctx, access, method, path, query, out)
		var se *statusError
		if errors.As(err, &se) && se.status == http.StatusUnauthorized && attempt == 0 {
			rejected = access // retry with a token newer than this one
			continue
		}
		return err
	}
	return ErrNotConnected
}

type statusError struct {
	status  int
	message string
	path    string
}

func (e *statusError) Error() string {
	return fmt.Sprintf("spotify %s: status %d: %s", e.path, e.status, e.message)
}

func (c *Client) doWith(ctx context.Context, access, method, path string, query url.Values, out any) error {
	u := apiBase + path
	if len(query) > 0 {
		u += "?" + query.Encode()
	}
	req, err := http.NewRequestWithContext(ctx, method, u, http.NoBody)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+access)
	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("spotify %s: %w", path, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 200 && resp.StatusCode <= 299 {
		if out == nil || resp.StatusCode == http.StatusNoContent {
			_, _ = io.Copy(io.Discard, resp.Body)
			return nil
		}
		if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
			return fmt.Errorf("parse spotify %s: %w", path, err)
		}
		return nil
	}
	body, _ := io.ReadAll(io.LimitReader(resp.Body, maxErrorBody))
	var ae apiError
	_ = json.Unmarshal(body, &ae)
	return classify(path, resp, ae.Error.Message)
}

// classify maps API errors to messages a streamer can act on. The reference
// pages only list the status codes; the wording of player errors is not
// documented, so we look at both the status and Spotify's message.
func classify(path string, resp *http.Response, message string) error {
	se := &statusError{status: resp.StatusCode, message: message, path: path}
	lower := strings.ToLower(message)
	isPlayer := strings.HasPrefix(path, "/me/player")
	switch {
	case resp.StatusCode == http.StatusTooManyRequests:
		wait, _ := strconv.Atoi(resp.Header.Get("Retry-After"))
		return fmt.Errorf("%w (retry after %d s): %v", ErrRateLimited, wait, se)
	case isPlayer && strings.Contains(lower, "device"):
		return fmt.Errorf("%w: %v", ErrNoDevice, se)
	case isPlayer && strings.Contains(lower, "premium"):
		return fmt.Errorf("%w: %v", ErrPremium, se)
	case isPlayer && resp.StatusCode == http.StatusNotFound:
		return fmt.Errorf("%w: %v", ErrNoDevice, se)
	case isPlayer && resp.StatusCode == http.StatusForbidden:
		return fmt.Errorf("%w: %v", ErrPremium, se)
	case resp.StatusCode == http.StatusNotFound || resp.StatusCode == http.StatusBadRequest && strings.Contains(lower, "invalid"):
		return fmt.Errorf("%w: %v", ErrNotFound, se)
	}
	return se
}

type profile struct {
	ID          string `json:"id"`
	DisplayName string `json:"display_name"`
}

func (c *Client) profile(ctx context.Context, access string) (*profile, error) {
	var p profile
	if err := c.doWith(ctx, access, http.MethodGet, "/me", nil, &p); err != nil {
		return nil, err
	}
	return &p, nil
}

// Track is the part of a Spotify track darkbot uses.
type Track struct {
	ID         string `json:"id"`
	URI        string `json:"uri"`
	Name       string `json:"name"`
	DurationMS int    `json:"duration_ms"`
	Explicit   bool   `json:"explicit"`
	IsPlayable *bool  `json:"is_playable"` // only set with track relinking (market given)
	Artists    []struct {
		Name string `json:"name"`
	} `json:"artists"`
}

// Title is "Artist, Artist – Name".
func (t Track) Title() string {
	names := make([]string, 0, len(t.Artists))
	for _, a := range t.Artists {
		names = append(names, a.Name)
	}
	if len(names) == 0 {
		return t.Name
	}
	return strings.Join(names, ", ") + " – " + t.Name
}

// Duration of the track.
func (t Track) Duration() time.Duration { return time.Duration(t.DurationMS) * time.Millisecond }

// marketFromToken: "from_token" applies track relinking for the user's
// country, so is_playable tells whether the user can actually play it.
const marketFromToken = "from_token"

// GetTrack loads one track by Spotify ID.
func (c *Client) GetTrack(ctx context.Context, id string) (*Track, error) {
	var t Track
	q := url.Values{"market": {marketFromToken}}
	if err := c.do(ctx, http.MethodGet, "/tracks/"+url.PathEscape(id), q, &t); err != nil {
		return nil, err
	}
	return &t, nil
}

// SearchTrack returns the best matching track for free text.
func (c *Client) SearchTrack(ctx context.Context, query string) (*Track, error) {
	var out struct {
		Tracks struct {
			Items []Track `json:"items"`
		} `json:"tracks"`
	}
	q := url.Values{"q": {query}, "type": {"track"}, "limit": {strconv.Itoa(searchLimit)}, "market": {marketFromToken}}
	if err := c.do(ctx, http.MethodGet, "/search", q, &out); err != nil {
		return nil, err
	}
	if len(out.Tracks.Items) == 0 {
		return nil, ErrNotFound
	}
	return &out.Tracks.Items[0], nil
}

// AddToQueue queues a track on the user's active device (Premium only).
func (c *Client) AddToQueue(ctx context.Context, uri string) error {
	return c.do(ctx, http.MethodPost, "/me/player/queue", url.Values{"uri": {uri}}, nil)
}

// Next skips to the next track (Premium only).
func (c *Client) Next(ctx context.Context) error {
	return c.do(ctx, http.MethodPost, "/me/player/next", nil, nil)
}

// Queue is what plays now and what comes next. Episodes appear with an
// empty artist list.
type Queue struct {
	Current *Track  `json:"currently_playing"`
	Next    []Track `json:"queue"`
}

// GetQueue reads the user's queue.
func (c *Client) GetQueue(ctx context.Context) (*Queue, error) {
	var q Queue
	if err := c.do(ctx, http.MethodGet, "/me/player/queue", nil, &q); err != nil {
		return nil, err
	}
	return &q, nil
}

// Spotify IDs are base62 (https://developer.spotify.com/documentation/web-api/concepts/spotify-uris-ids).
var (
	trackURL = regexp.MustCompile(`^https?://open\.spotify\.com/(?:intl-[a-zA-Z-]+/)?(track|album|playlist|artist|episode|show)/([0-9A-Za-z]+)`)
	trackURI = regexp.MustCompile(`^spotify:(track|album|playlist|artist|episode|show):([0-9A-Za-z]+)$`)
)

// ParseLink returns the track ID of a Spotify track link or URI. ok is false
// when text isn't a Spotify link at all (then it is a search). Links to
// albums, playlists etc. and spotify.link short links give an error.
func ParseLink(text string) (id string, ok bool, err error) {
	text = strings.TrimSpace(text)
	m := trackURL.FindStringSubmatch(text)
	if m == nil {
		m = trackURI.FindStringSubmatch(text)
	}
	if m != nil {
		if m[1] != "track" {
			return "", true, ErrNotTrackLink
		}
		return m[2], true, nil
	}
	lower := strings.ToLower(text)
	if strings.Contains(lower, "spotify.link/") || strings.Contains(lower, "spotify.com/") || strings.HasPrefix(lower, "spotify:") {
		return "", true, ErrUnsupportedURL
	}
	return "", false, nil
}

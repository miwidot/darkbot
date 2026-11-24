package spotify

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type memStore struct {
	mu  sync.Mutex
	tok *Token
}

func (m *memStore) Load() (*Token, error) { m.mu.Lock(); defer m.mu.Unlock(); return m.tok, nil }
func (m *memStore) Save(t *Token) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	c := *t
	m.tok = &c
	return nil
}
func (m *memStore) Delete() error { m.mu.Lock(); defer m.mu.Unlock(); m.tok = nil; return nil }

type nopLog struct{}

func (nopLog) Infof(string, ...any)  {}
func (nopLog) Warnf(string, ...any)  {}
func (nopLog) Errorf(string, ...any) {}

func TestNewLoginPKCE(t *testing.T) {
	p, err := NewLogin(" cid ")
	if err != nil {
		t.Fatal(err)
	}
	if n := len(p.Verifier); n < 43 || n > 128 {
		t.Fatalf("verifier length %d outside 43..128", n)
	}
	u, _ := url.Parse(p.URL)
	q := u.Query()
	sum := sha256.Sum256([]byte(p.Verifier))
	if q.Get("code_challenge") != base64.RawURLEncoding.EncodeToString(sum[:]) || q.Get("code_challenge_method") != "S256" {
		t.Fatal("bad code challenge")
	}
	if q.Get("client_id") != "cid" || q.Get("redirect_uri") != RedirectURI || q.Get("response_type") != "code" || q.Get("state") != p.State {
		t.Fatalf("bad authorize query %v", q)
	}
	if _, err := NewLogin("  "); err == nil {
		t.Fatal("empty client id must fail")
	}
}

func TestPendingLoginMatches(t *testing.T) {
	p, err := NewLogin("cid")
	if err != nil {
		t.Fatal(err)
	}
	var none *PendingLogin
	for _, tt := range []struct {
		p    *PendingLogin
		id   string
		want bool
	}{
		{p, "cid", true},
		{p, " cid ", true}, // typed with spaces, like NewLogin accepts
		{p, "other", false},
		{none, "cid", false},
	} {
		if got := tt.p.Matches(tt.id); got != tt.want {
			t.Errorf("Matches(%q) on %v = %v, want %v", tt.id, tt.p != nil, got, tt.want)
		}
	}
}

func TestCodeFromPaste(t *testing.T) {
	p := &PendingLogin{State: "S1"}
	cases := []struct {
		in, code string
		fail     bool
	}{
		{RedirectURI + "?code=ABC&state=S1", "ABC", false},
		{"  " + RedirectURI + "?code=ABC&state=S1  ", "ABC", false},
		{RedirectURI + "?code=ABC&state=OLD", "", true},
		{RedirectURI + "?error=access_denied&state=S1", "", true},
		{RedirectURI + "?state=S1", "", true},
		{"BARECODE", "BARECODE", false},
		{"", "", true},
	}
	for _, c := range cases {
		code, err := p.CodeFromPaste(c.in)
		if (err != nil) != c.fail || code != c.code {
			t.Errorf("CodeFromPaste(%q) = %q, %v", c.in, code, err)
		}
	}
}

func TestParseLink(t *testing.T) {
	cases := []struct {
		in, id   string
		link, ok bool
	}{
		{"https://open.spotify.com/track/4uLU6hMCjMI75M1A2tKUQC?si=x", "4uLU6hMCjMI75M1A2tKUQC", true, true},
		{"https://open.spotify.com/intl-de/track/4uLU6hMCjMI75M1A2tKUQC", "4uLU6hMCjMI75M1A2tKUQC", true, true},
		{"spotify:track:4uLU6hMCjMI75M1A2tKUQC", "4uLU6hMCjMI75M1A2tKUQC", true, true},
		{"https://open.spotify.com/album/1DFixLWuPkv3KT3TnV35m3", "", true, false},
		{"https://spotify.link/abcdef", "", true, false},
		{"never gonna give you up", "", false, true},
	}
	for _, c := range cases {
		id, link, err := ParseLink(c.in)
		if id != c.id || link != c.link || (err == nil) != c.ok {
			t.Errorf("ParseLink(%q) = %q, %v, %v", c.in, id, link, err)
		}
	}
}

// tokenServer answers the token endpoint; it rotates the refresh token only
// when rotate is set, like Spotify ("might not include a new refresh token").
func tokenServer(t *testing.T, rotate bool, refreshes *atomic.Int32) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseForm(); err != nil {
			t.Error(err)
		}
		out := map[string]any{"access_token": "A2", "token_type": "Bearer", "expires_in": 3600}
		switch r.Form.Get("grant_type") {
		case "refresh_token":
			refreshes.Add(1)
			if r.Form.Get("refresh_token") == "dead" {
				w.WriteHeader(http.StatusBadRequest)
				_, _ = w.Write([]byte(`{"error":"invalid_grant","error_description":"Refresh token revoked"}`))
				return
			}
			if rotate {
				out["refresh_token"] = "R2"
			}
		case "authorization_code":
			if r.Form.Get("code_verifier") == "" {
				t.Error("missing code_verifier")
			}
			out["access_token"], out["refresh_token"] = "A1", "R1"
		}
		_ = json.NewEncoder(w).Encode(out)
	}))
	t.Cleanup(srv.Close)
	old := tokenURL
	tokenURL = srv.URL
	t.Cleanup(func() { tokenURL = old })
}

func TestRefreshKeepsOldRefreshToken(t *testing.T) {
	var n atomic.Int32
	tokenServer(t, false, &n)
	tok, err := refresh(context.Background(), "cid", "R1")
	if err != nil {
		t.Fatal(err)
	}
	if tok.RefreshToken != "R1" || tok.AccessToken != "A2" {
		t.Fatalf("got %+v", tok)
	}
}

// apiServer answers the Web API; the first call with an old token gets 401.
func apiServer(t *testing.T, handler http.HandlerFunc) {
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	old := apiBase
	apiBase = srv.URL
	t.Cleanup(func() { apiBase = old })
}

func TestClientRefreshesOn401AndConcurrently(t *testing.T) {
	var n atomic.Int32
	tokenServer(t, true, &n)
	apiServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer A2" {
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte(`{"error":{"status":401,"message":"The access token expired"}}`))
			return
		}
		_ = json.NewEncoder(w).Encode(Queue{Current: &Track{Name: "x"}})
	})
	store := &memStore{tok: &Token{AccessToken: "A1", RefreshToken: "R1", ExpiresAt: time.Now().Add(time.Hour)}}
	c, err := NewClient("cid", store, nopLog{})
	if err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	for range 8 {
		wg.Go(func() {
			if _, err := c.GetQueue(context.Background()); err != nil {
				t.Error(err)
			}
		})
	}
	wg.Wait()
	if got := n.Load(); got != 1 {
		t.Fatalf("%d refreshes, want exactly 1", got)
	}
	if s, _ := store.Load(); s.RefreshToken != "R2" || s.AccessToken != "A2" {
		t.Fatalf("stored %+v", s)
	}
}

func TestRejectedRefreshLogsOut(t *testing.T) {
	var n atomic.Int32
	tokenServer(t, false, &n)
	store := &memStore{tok: &Token{AccessToken: "A1", RefreshToken: "dead", ExpiresAt: time.Now()}}
	c, _ := NewClient("cid", store, nopLog{})
	changed := make(chan struct{}, 1)
	c.OnChange(func() { changed <- struct{}{} })
	_, err := c.GetQueue(context.Background())
	if !errors.Is(err, ErrRejected) {
		t.Fatalf("err %v", err)
	}
	if c.Current() != nil {
		t.Fatal("rejected login must log out")
	}
	select {
	case <-changed:
	case <-time.After(time.Second):
		t.Fatal("OnChange not called")
	}
}

func TestPlayerErrorMapping(t *testing.T) {
	apiServer(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/me/player/queue":
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"error":{"status":404,"message":"Player command failed: No active device found"}}`))
		case "/me/player/next":
			w.WriteHeader(http.StatusForbidden)
			_, _ = w.Write([]byte(`{"error":{"status":403,"message":"Player command failed: Premium required"}}`))
		case "/search":
			w.Header().Set("Retry-After", "3")
			w.WriteHeader(http.StatusTooManyRequests)
		case "/me/player/currently-playing":
			w.WriteHeader(http.StatusForbidden)
			_, _ = w.Write([]byte(`{"error":{"status":403,"message":"Insufficient client scope"}}`))
		}
	})
	c, _ := NewClient("cid", &memStore{tok: &Token{AccessToken: "A", ExpiresAt: time.Now().Add(time.Hour)}}, nopLog{})
	ctx := context.Background()
	if err := c.AddToQueue(ctx, "spotify:track:x"); !errors.Is(err, ErrNoDevice) {
		t.Errorf("queue: %v", err)
	}
	if err := c.Next(ctx); !errors.Is(err, ErrPremium) {
		t.Errorf("next: %v", err)
	}
	if _, err := c.SearchTrack(ctx, "x"); !errors.Is(err, ErrRateLimited) {
		t.Errorf("search: %v", err)
	}
	// A missing permission is no "Premium required", even on the player.
	if _, err := c.CurrentlyPlaying(ctx); !errors.Is(err, ErrScope) {
		t.Errorf("scope: %v", err)
	}
}

// Search sends no market (see SearchTrack).
func TestSearchWithoutMarket(t *testing.T) {
	apiServer(t, func(w http.ResponseWriter, r *http.Request) {
		if q := r.URL.Query(); r.URL.Path != "/search" || q.Has("market") || q.Get("q") != "onkelz" || q.Get("type") != "track" {
			t.Errorf("request %s", r.URL)
		}
		_, _ = w.Write([]byte(`{"tracks":{"items":[{"id":"1","uri":"spotify:track:1","name":"Song"}]}}`))
	})
	c, _ := NewClient("cid", &memStore{tok: &Token{AccessToken: "A", ExpiresAt: time.Now().Add(time.Hour)}}, nopLog{})
	if tr, err := c.SearchTrack(context.Background(), "onkelz"); err != nil || tr.ID != "1" {
		t.Fatalf("%v %+v", err, tr)
	}
}

func TestNotConnected(t *testing.T) {
	c, _ := NewClient("cid", &memStore{}, nopLog{})
	if _, err := c.GetQueue(context.Background()); !errors.Is(err, ErrNotConnected) {
		t.Fatalf("err %v", err)
	}
}

func TestHealthKeepsLoginDateAcrossRefresh(t *testing.T) {
	var n atomic.Int32
	tokenServer(t, false, &n)
	authorized := time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC)
	store := &memStore{tok: &Token{AccessToken: "A1", RefreshToken: "R1", ExpiresAt: time.Now(), AuthorizedAt: authorized}}
	c, _ := NewClient("cid", store, nopLog{})
	if err := c.RefreshNow(context.Background()); err != nil {
		t.Fatal(err)
	}
	h := c.Health()
	if !h.LoggedIn || h.LastRefresh.IsZero() || h.LastError != "" {
		t.Fatalf("health %+v", h)
	}
	if want := authorized.AddDate(0, RefreshTokenLifetimeMonths, 0); !h.LoginUntil.Equal(want) {
		t.Fatalf("login until %v, want %v (refreshing must not extend it)", h.LoginUntil, want)
	}
}

func TestPlaying(t *testing.T) {
	var answer atomic.Value
	apiServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/me/player" || r.Method != http.MethodGet {
			t.Errorf("%s %s", r.Method, r.URL.Path)
		}
		a := answer.Load().(string)
		if a == "" {
			w.WriteHeader(http.StatusNoContent) // no playback at all
			return
		}
		_, _ = w.Write([]byte(a))
	})
	c, _ := NewClient("cid", &memStore{tok: &Token{AccessToken: "A", ExpiresAt: time.Now().Add(time.Hour)}}, nopLog{})
	for body, want := range map[string]bool{"": false, `{"is_playing":false}`: false, `{"is_playing":true}`: true} {
		answer.Store(body)
		if got, err := c.Playing(context.Background()); err != nil || got != want {
			t.Errorf("%q: got %v, %v", body, got, err)
		}
	}
}

func TestCurrentlyPlaying(t *testing.T) {
	var answer atomic.Value
	apiServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/me/player/currently-playing" {
			t.Errorf("path %s", r.URL.Path)
		}
		a := answer.Load().(string)
		if a == "" {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		_, _ = w.Write([]byte(a))
	})
	c, _ := NewClient("cid", &memStore{tok: &Token{AccessToken: "A", ExpiresAt: time.Now().Add(time.Hour)}}, nopLog{})
	ctx := context.Background()
	answer.Store("")
	if p, err := c.CurrentlyPlaying(ctx); p != nil || err != nil {
		t.Fatalf("nothing playing: %+v %v", p, err)
	}
	answer.Store(`{"is_playing":true,"currently_playing_type":"ad","item":null}`)
	if p, err := c.CurrentlyPlaying(ctx); p != nil || err != nil {
		t.Fatalf("an ad is no song: %+v %v", p, err)
	}
	answer.Store(`{"is_playing":true,"progress_ms":61000,"currently_playing_type":"track","item":{"id":"x","uri":"spotify:track:x","name":"Around the World","duration_ms":429000,
		"artists":[{"name":"Daft Punk"}],"album":{"images":[{"url":"big","width":640},{"url":"mid","width":300},{"url":"small","width":64}]}}}`)
	p, err := c.CurrentlyPlaying(ctx)
	if err != nil || p == nil {
		t.Fatalf("got %+v %v", p, err)
	}
	if !p.IsPlaying || p.ProgressMS != 61000 || p.Item.Name != "Around the World" || p.Item.Title() != "Daft Punk – Around the World" || p.Item.DurationMS != 429000 {
		t.Fatalf("parsed %+v", p.Item)
	}
	if p.Cover(200) != "mid" || p.Cover(1000) != "big" || p.Cover(0) != "small" {
		t.Fatalf("covers %q %q %q", p.Cover(200), p.Cover(1000), p.Cover(0))
	}
}

package twitch

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
	"unicode/utf8"
)

type memStore struct {
	mu      sync.Mutex
	tok     *Token
	deleted bool
}

func (m *memStore) Load() (*Token, error) { m.mu.Lock(); defer m.mu.Unlock(); return m.tok, nil }
func (m *memStore) Save(t *Token) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	c := *t
	m.tok = &c
	return nil
}
func (m *memStore) Delete() error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.tok, m.deleted = nil, true
	return nil
}

type nopLog struct{}

func (nopLog) Infof(string, ...any)  {}
func (nopLog) Warnf(string, ...any)  {}
func (nopLog) Errorf(string, ...any) {}

func fakeTwitch(t *testing.T, h http.HandlerFunc) {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	oldD, oldT, oldV, oldH, oldP := deviceURL, tokenURL, validateURL, helixBase, pollUnit
	deviceURL, tokenURL, validateURL, helixBase = srv.URL+"/device", srv.URL+"/token", srv.URL+"/validate", srv.URL+"/helix"
	pollUnit = time.Millisecond
	t.Cleanup(func() { deviceURL, tokenURL, validateURL, helixBase, pollUnit = oldD, oldT, oldV, oldH, oldP })
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func TestPollDeviceFlow(t *testing.T) {
	var polls atomic.Int32
	fakeTwitch(t, func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		if r.Form.Get("grant_type") != deviceGrantType || r.Form.Get("device_code") != "dev" {
			writeJSON(w, 400, map[string]any{"status": 400, "message": "bad request"})
			return
		}
		switch polls.Add(1) {
		case 1:
			writeJSON(w, 400, map[string]any{"status": 400, "message": "authorization_pending"})
		case 2:
			writeJSON(w, 400, map[string]any{"status": 400, "message": "slow_down"})
		default:
			writeJSON(w, 200, map[string]any{"access_token": "A", "refresh_token": "R", "expires_in": 14400, "scope": Scopes, "token_type": "bearer"})
		}
	})

	tok, err := PollDeviceFlow(context.Background(), "cid", &DeviceCode{DeviceCode: "dev", Interval: 1, ExpiresIn: 100000})
	if err != nil {
		t.Fatal(err)
	}
	if tok.AccessToken != "A" || tok.RefreshToken != "R" || time.Until(tok.ExpiresAt) < 3*time.Hour {
		t.Fatalf("unexpected token %+v", tok)
	}
	if polls.Load() != 3 {
		t.Fatalf("polls = %d, want 3", polls.Load())
	}
}

func TestPollDeviceFlowDenied(t *testing.T) {
	fakeTwitch(t, func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, 400, map[string]any{"status": 400, "message": "access_denied"})
	})
	_, err := PollDeviceFlow(context.Background(), "cid", &DeviceCode{DeviceCode: "dev", Interval: 1, ExpiresIn: 100000})
	if err == nil {
		t.Fatal("expected error")
	}
}

// Device-flow refresh tokens are single-use: many concurrent 401s must
// result in exactly ONE refresh call, and everyone gets the new token.
func TestRefreshIsSerializedAndDeduped(t *testing.T) {
	var refreshes atomic.Int32
	fakeTwitch(t, func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		if r.Form.Get("refresh_token") != "R1" {
			writeJSON(w, 400, map[string]any{"status": 400, "message": "Invalid refresh token"})
			return
		}
		refreshes.Add(1)
		time.Sleep(20 * time.Millisecond) // make overlap likely
		writeJSON(w, 200, map[string]any{"access_token": "A2", "refresh_token": "R2", "expires_in": 14400})
	})
	store := &memStore{tok: &Token{AccessToken: "A1", RefreshToken: "R1", UserID: "42", Login: "miwi"}}
	m, err := NewTokenManager("cid", store, nopLog{})
	if err != nil {
		t.Fatal(err)
	}

	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := m.RefreshIfStale(context.Background(), "A1"); err != nil {
				t.Errorf("refresh: %v", err)
			}
		}()
	}
	wg.Wait()

	if refreshes.Load() != 1 {
		t.Fatalf("refresh calls = %d, want 1", refreshes.Load())
	}
	cur := m.Current()
	if cur.AccessToken != "A2" || cur.RefreshToken != "R2" || cur.UserID != "42" || cur.Login != "miwi" {
		t.Fatalf("token not updated correctly: %+v", cur)
	}
	if store.tok.RefreshToken != "R2" {
		t.Fatal("new refresh token was not persisted")
	}
}

func TestRefreshRejectedLogsOut(t *testing.T) {
	fakeTwitch(t, func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, 400, map[string]any{"status": 400, "message": "Invalid refresh token"})
	})
	store := &memStore{tok: &Token{AccessToken: "A1", RefreshToken: "R1"}}
	m, _ := NewTokenManager("cid", store, nopLog{})
	var notified atomic.Bool
	m.OnChange(func(t *Token) { notified.Store(t == nil) })

	err := m.RefreshIfStale(context.Background(), "A1")
	if !errors.Is(err, ErrTokenRejected) {
		t.Fatalf("err = %v, want ErrTokenRejected", err)
	}
	if m.Current() != nil || !store.deleted || !notified.Load() {
		t.Fatal("rejected refresh must log out and notify")
	}
}

func TestRefreshTransientKeepsToken(t *testing.T) {
	fakeTwitch(t, func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, 503, map[string]any{"status": 503, "message": "unavailable"})
	})
	store := &memStore{tok: &Token{AccessToken: "A1", RefreshToken: "R1"}}
	m, _ := NewTokenManager("cid", store, nopLog{})

	err := m.RefreshIfStale(context.Background(), "A1")
	if err == nil || errors.Is(err, ErrTokenRejected) {
		t.Fatalf("err = %v, want transient error", err)
	}
	if m.Current() == nil || m.Current().RefreshToken != "R1" {
		t.Fatal("transient error must keep the token")
	}
}

func TestHelixRetriesAfter401(t *testing.T) {
	var helixCalls atomic.Int32
	fakeTwitch(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/token":
			writeJSON(w, 200, map[string]any{"access_token": "A2", "refresh_token": "R2", "expires_in": 14400})
		case "/helix/channel_points/custom_rewards":
			helixCalls.Add(1)
			if r.Header.Get("Authorization") != "Bearer A2" {
				writeJSON(w, 401, map[string]any{"status": 401, "message": "Invalid OAuth token"})
				return
			}
			if r.Header.Get("Client-Id") != "cid" || r.URL.Query().Get("broadcaster_id") != "42" {
				writeJSON(w, 400, map[string]any{"status": 400, "message": "bad"})
				return
			}
			writeJSON(w, 200, map[string]any{"data": []map[string]any{{"id": "r1", "title": "Airhorn", "cost": 100, "is_enabled": true}}})
		default:
			http.NotFound(w, r)
		}
	})
	m, _ := NewTokenManager("cid", &memStore{tok: &Token{AccessToken: "A1", RefreshToken: "R1", UserID: "42"}}, nopLog{})
	rewards, err := NewHelix(m).GetCustomRewards(context.Background(), "42")
	if err != nil {
		t.Fatal(err)
	}
	if len(rewards) != 1 || rewards[0].Title != "Airhorn" || helixCalls.Load() != 2 {
		t.Fatalf("rewards=%+v calls=%d", rewards, helixCalls.Load())
	}
}

// Notification sample shaped like the docs' example payload.
const sampleNotification = `{
  "metadata": {"message_id": "m1", "message_type": "notification", "message_timestamp": "2026-11-16T10:11:12.123Z",
    "subscription_type": "channel.channel_points_custom_reward_redemption.add", "subscription_version": "1"},
  "payload": {
    "subscription": {"id": "s1", "type": "channel.channel_points_custom_reward_redemption.add", "version": "1", "status": "enabled",
      "cost": 0, "condition": {"broadcaster_user_id": "1337"}, "transport": {"method": "websocket", "session_id": "x"}, "created_at": "2026-11-16T10:00:00Z"},
    "event": {"id": "red-1", "broadcaster_user_id": "1337", "broadcaster_user_login": "cool_user", "broadcaster_user_name": "Cool_User",
      "user_id": "9001", "user_login": "cooler_user", "user_name": "Cooler_User", "user_input": "pogchamp", "status": "unfulfilled",
      "reward": {"id": "92af127c-7326-4483-a52b-b0da0be61c01", "title": "title", "cost": 100, "prompt": "reward prompt"},
      "redeemed_at": "2020-07-15T17:16:03.17106713Z"}
  }
}`

func TestHandleNotification(t *testing.T) {
	var got []Redemption
	e := NewEventSub(nil, nil, nopLog{}, func(r Redemption) { got = append(got, r) }, func(bool) {})
	var msg wsMessage
	if err := json.Unmarshal([]byte(sampleNotification), &msg); err != nil {
		t.Fatal(err)
	}
	e.handleNotification(&msg)
	e.handleNotification(&msg) // duplicate message_id must be ignored

	if len(got) != 1 {
		t.Fatalf("got %d redemptions, want 1", len(got))
	}
	r := got[0]
	if r.ID != "red-1" || r.RewardID != "92af127c-7326-4483-a52b-b0da0be61c01" || r.RewardTitle != "title" ||
		r.UserID != "9001" || r.UserName != "Cooler_User" || r.UserInput != "pogchamp" || r.RedeemedAt.IsZero() {
		t.Fatalf("unexpected redemption %+v", r)
	}
}

// A logout while a refresh is in flight must win: the refreshed token must
// not be written back afterwards.
func TestLogoutDuringRefreshStaysLoggedOut(t *testing.T) {
	started := make(chan struct{})
	release := make(chan struct{})
	fakeTwitch(t, func(w http.ResponseWriter, r *http.Request) {
		close(started)
		<-release
		writeJSON(w, 200, map[string]any{"access_token": "A2", "refresh_token": "R2", "expires_in": 14400})
	})
	store := &memStore{tok: &Token{AccessToken: "A1", RefreshToken: "R1"}}
	m, _ := NewTokenManager("cid", store, nopLog{})

	refreshDone := make(chan error, 1)
	go func() { refreshDone <- m.RefreshIfStale(context.Background(), "A1") }()
	<-started
	logoutDone := make(chan error, 1)
	go func() { logoutDone <- m.Logout() }()
	close(release)

	if err := <-refreshDone; err != nil {
		t.Fatal(err)
	}
	if err := <-logoutDone; err != nil {
		t.Fatal(err)
	}
	if m.Current() != nil || store.tok != nil {
		t.Fatal("logout must win over a concurrent refresh")
	}
}

func TestIsTwitchWSURL(t *testing.T) {
	ok := []string{"wss://eventsub.wss.twitch.tv/ws?x=1", "wss://twitch.tv/ws"}
	bad := []string{"ws://eventsub.wss.twitch.tv/ws", "wss://evil.example/ws", "wss://twitch.tv.evil.example/ws", "https://eventsub.wss.twitch.tv/ws", "::"}
	for _, u := range ok {
		if !isTwitchWSURL(u) {
			t.Errorf("%q should be allowed", u)
		}
	}
	for _, u := range bad {
		if isTwitchWSURL(u) {
			t.Errorf("%q should be refused", u)
		}
	}
}

func TestParseChatEvent(t *testing.T) {
	raw := `{"broadcaster_user_id":"1","chatter_user_id":"2","chatter_user_login":"mod","chatter_user_name":"Mod",
		"message_id":"m","message":{"text":"!skip"},"badges":[{"set_id":"moderator","id":"1","info":""}],
		"source_broadcaster_user_id":null}`
	m, ok, err := parseChatEvent(json.RawMessage(raw))
	if err != nil || !ok {
		t.Fatalf("ok=%v err=%v", ok, err)
	}
	if !m.IsModerator || m.IsBroadcaster || m.Text != "!skip" || m.ChatterName != "Mod" || m.ID != "m" {
		t.Fatalf("got %+v", m)
	}

	own := `{"broadcaster_user_id":"1","chatter_user_id":"1","message":{"text":"hi"},"badges":[],"source_broadcaster_user_id":"1"}`
	if m, ok, _ := parseChatEvent(json.RawMessage(own)); !ok || !m.IsBroadcaster {
		t.Fatalf("broadcaster message: ok=%v %+v", ok, m)
	}

	shared := `{"broadcaster_user_id":"1","chatter_user_id":"9","message":{"text":"!sr x"},"badges":[],"source_broadcaster_user_id":"5"}`
	if _, ok, _ := parseChatEvent(json.RawMessage(shared)); ok {
		t.Fatal("Shared Chat messages from other channels must be ignored")
	}
}

func TestTruncateChat(t *testing.T) {
	short := "hällo"
	if TruncateChat(short) != short {
		t.Fatal("short text changed")
	}
	long := strings.Repeat("ä", MaxChatMessage+10)
	got := TruncateChat(long)
	if n := utf8.RuneCountInString(got); n != MaxChatMessage || !strings.HasSuffix(got, "…") {
		t.Fatalf("length %d", n)
	}
}

func TestHasScope(t *testing.T) {
	tok := &Token{Scopes: []string{ScopeReadRedemptions, ScopeManageRedemptions}}
	if !tok.HasScope(ScopeManageRedemptions) || tok.HasScope(ScopeReadChat) || tok.HasScopes() {
		t.Fatal("old token: has reward scopes, lacks chat scopes")
	}
	tok.Scopes = append(tok.Scopes, ScopeReadChat, ScopeWriteChat)
	if !tok.HasScopes() {
		t.Fatal("all scopes granted")
	}
}

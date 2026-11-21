package twitch

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strconv"
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
	oldD, oldT, oldV, oldR, oldH, oldP := deviceURL, tokenURL, validateURL, revokeURL, helixBase, pollUnit
	deviceURL, tokenURL, validateURL, revokeURL, helixBase = srv.URL+"/device", srv.URL+"/token", srv.URL+"/validate", srv.URL+"/revoke", srv.URL+"/helix"
	pollUnit = time.Millisecond
	t.Cleanup(func() {
		deviceURL, tokenURL, validateURL, revokeURL, helixBase, pollUnit = oldD, oldT, oldV, oldR, oldH, oldP
	})
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

func TestHealthAfterRefresh(t *testing.T) {
	fakeTwitch(t, func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, 200, map[string]any{"access_token": "A2", "refresh_token": "R2", "expires_in": 14400})
	})
	m, _ := NewTokenManager("cid", &memStore{tok: &Token{AccessToken: "A1", RefreshToken: "R1"}}, nopLog{})
	if h := m.Health(); !h.LoggedIn || !h.LoginUntil.IsZero() || !h.LastRefresh.IsZero() {
		t.Fatalf("before refresh %+v (old tokens don't know their refresh date)", h)
	}
	before := time.Now()
	if err := m.RefreshNow(context.Background()); err != nil {
		t.Fatal(err)
	}
	h := m.Health()
	if h.LastRefresh.Before(before) || h.LastError != "" {
		t.Fatalf("health %+v", h)
	}
	if d := h.LoginUntil.Sub(before); d < RefreshTokenLifetime || d > RefreshTokenLifetime+time.Minute {
		t.Fatalf("login until %v, want about 30 days after the refresh", h.LoginUntil)
	}
	if !h.NextRefresh.Equal(h.ExpiresAt.Add(-refreshMargin)) {
		t.Fatalf("next refresh %v, expires %v", h.NextRefresh, h.ExpiresAt)
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
	if m.Health().LastError == "" {
		t.Fatal("a failed refresh must show up in the health")
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

	for _, tc := range []struct {
		badge    string
		vip, sub bool
	}{{"vip", true, false}, {"subscriber", false, true}, {"founder", false, true}, {"sub-gifter", false, false}} {
		raw := `{"broadcaster_user_id":"1","chatter_user_id":"3","message":{"text":"x"},"badges":[{"set_id":"` + tc.badge + `","id":"1","info":""}]}`
		m, _, _ := parseChatEvent(json.RawMessage(raw))
		if m.IsVIP != tc.vip || m.IsSubscriber != tc.sub || m.IsModerator {
			t.Errorf("badge %s: %+v", tc.badge, m)
		}
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
	tok.Scopes = append([]string(nil), Scopes...)
	if !tok.HasScopes() {
		t.Fatal("all scopes granted")
	}
}

func TestParseChannelEvents(t *testing.T) {
	cases := []struct {
		subType, raw string
		want         ChannelEvent
	}{
		{"channel.follow", `{"user_id":"1","user_login":"anna","user_name":"Anna","followed_at":"2026-11-17T10:00:00Z"}`,
			ChannelEvent{Kind: EvFollow, User: "Anna", UserID: "1"}},
		{"channel.subscribe", `{"user_id":"2","user_name":"Ben","tier":"2000","is_gift":true}`,
			ChannelEvent{Kind: EvSub, User: "Ben", UserID: "2", Tier: "2", Gift: true}},
		{"channel.subscription.message", `{"user_id":"3","user_name":"Cleo","tier":"1000","message":{"text":"hi","emotes":[]},"cumulative_months":15,"streak_months":null,"duration_months":1}`,
			ChannelEvent{Kind: EvResub, User: "Cleo", UserID: "3", Tier: "1", Months: 15, Message: "hi"}},
		{"channel.subscription.gift", `{"user_id":null,"user_login":null,"user_name":null,"total":5,"tier":"1000","cumulative_total":null,"is_anonymous":true}`,
			ChannelEvent{Kind: EvGift, Amount: 5, Tier: "1", Anonymous: true}},
		// channel.bits.use, shaped like the reference's event fields.
		{"channel.bits.use", `{"broadcaster_user_id":"9","user_id":"4","user_login":"dan","user_name":"Dan","bits":100,"type":"cheer",
			"message":{"text":"Cheer100 gg","fragments":[{"text":"Cheer100","type":"cheermote","emote":null,"cheermote":{"prefix":"cheer","bits":100,"tier":100}},{"text":" gg","type":"text","emote":null,"cheermote":null}]},
			"power_up":null}`,
			ChannelEvent{Kind: EvCheer, User: "Dan", UserID: "4", Amount: 100, Message: "Cheer100 gg"}},
		{"channel.bits.use", `{"user_id":"4","user_name":"Dan","bits":50,"type":"power_up","message":null,
			"power_up":{"type":"gigantify_an_emote","emote":{"id":"e1","name":"Kappa"},"message_effect_id":null}}`,
			ChannelEvent{Kind: EvPowerUp, User: "Dan", UserID: "4", Amount: 50, PowerUpType: "gigantify_an_emote"}},
		{"channel.bits.use", `{"user_id":"4","user_name":"Dan","bits":300,"type":"custom_power_up","message":{"text":"","fragments":[]},
			"power_up":null,"custom_power_up":{"title":"Hydrate!","reward_id":"r1"}}`,
			ChannelEvent{Kind: EvPowerUp, User: "Dan", UserID: "4", Amount: 300, PowerUpType: PowerUpCustom, PowerUpTitle: "Hydrate!"}},
		// A kind of Bits use Twitch adds later: still Bits spent.
		{"channel.bits.use", `{"user_id":"4","user_name":"Dan","bits":10,"type":"something_new","message":null,"power_up":null}`,
			ChannelEvent{Kind: EvPowerUp, User: "Dan", UserID: "4", Amount: 10}},
		{"channel.raid", `{"from_broadcaster_user_id":"5","from_broadcaster_user_name":"Raider","to_broadcaster_user_id":"9","viewers":42}`,
			ChannelEvent{Kind: EvRaid, User: "Raider", UserID: "5", Amount: 42}},
		{"channel.hype_train.begin", `{"id":"x","level":2,"total":700}`, ChannelEvent{Kind: EvHypeBegin, Amount: 2, TrainID: "x"}},
		{"channel.ad_break.begin", `{"duration_seconds":90,"is_automatic":true}`, ChannelEvent{Kind: EvAdBreak, Amount: 90}},
		// The reference's own example sends the numbers as strings.
		{"channel.ad_break.begin", `{"duration_seconds":"60","started_at":"2019-11-16T10:11:12.634234626Z","is_automatic":"false"}`, ChannelEvent{Kind: EvAdBreak, Amount: 60}},
		{"channel.shoutout.receive", `{"from_broadcaster_user_id":"6","from_broadcaster_user_name":"Friend","viewer_count":300}`,
			ChannelEvent{Kind: EvShoutout, User: "Friend", UserID: "6", Amount: 300}},
		{"stream.online", `{"id":"1","type":"live"}`, ChannelEvent{Kind: EvOnline}},
		{"channel.update", `{"broadcaster_user_id":"1337","title":"Best Stream Ever","language":"en","category_id":"12453","category_name":"Grand Theft Auto","content_classification_labels":["MatureGame"]}`,
			ChannelEvent{Kind: EvChannelUpdate, Title: "Best Stream Ever", Category: "Grand Theft Auto"}},
		{"channel.channel_points_automatic_reward_redemption.add", `{"broadcaster_user_id":"12826","user_id":"141981764","user_login":"twitchdev","user_name":"TwitchDev","id":"f024099a","reward":{"type":"send_highlighted_message","channel_points":100,"emote":null},"message":{"text":"Hello world! VoHiYo","fragments":[]},"redeemed_at":"2024-08-12T21:14:34.260398045Z"}`,
			ChannelEvent{Kind: EvAutoReward, User: "TwitchDev", UserID: "141981764", Amount: 100, Reward: "send_highlighted_message"}},
		// A rerun or premiere is not the streamer going live.
		{"stream.online", `{"id":"1","type":"rerun"}`, ChannelEvent{}},
		{"stream.online", `{"id":"1","type":"premiere"}`, ChannelEvent{}},
		{"channel.chat.notification", `{"broadcaster_user_id":"9","chatter_user_id":"7","chatter_user_name":"Eva","chatter_is_anonymous":false,"notice_type":"watch_streak","source_broadcaster_user_id":null,"watch_streak":{"streak_count":5,"channel_points_awarded":450}}`,
			ChannelEvent{Kind: EvWatchStreak, User: "Eva", UserID: "7", Amount: 5, Points: 450}},
		// Other notices come through their own subscriptions: ignored.
		{"channel.chat.notification", `{"broadcaster_user_id":"9","chatter_user_id":"7","notice_type":"resub","watch_streak":null}`, ChannelEvent{}},
		// From another channel in Shared Chat: ignored.
		{"channel.chat.notification", `{"broadcaster_user_id":"9","chatter_user_id":"7","notice_type":"watch_streak","source_broadcaster_user_id":"5","watch_streak":{"streak_count":3,"channel_points_awarded":0}}`, ChannelEvent{}},
	}
	for _, c := range cases {
		got, err := parseChannelEvent(c.subType, json.RawMessage(c.raw))
		if err != nil || got != c.want {
			t.Errorf("%s: got %+v, %v; want %+v", c.subType, got, err, c.want)
		}
	}
	if _, err := parseChannelEvent("channel.unknown", nil); err == nil {
		t.Error("unknown types must fail")
	}
}

func TestEventSpecsAreComplete(t *testing.T) {
	seen := map[string]bool{}
	for _, s := range eventSpecs {
		if seen[s.Type] {
			t.Errorf("duplicate spec %s", s.Type)
		}
		seen[s.Type] = true
		if s.Scope != "" && !slices.Contains(Scopes, s.Scope) {
			t.Errorf("%s needs scope %s, but darkbot doesn't request it at login", s.Type, s.Scope)
		}
		cond := s.condition("42")
		if len(cond) == 0 {
			t.Errorf("%s has no condition", s.Type)
		}
	}
	// Subscriptions without a scope cost 1 each; Twitch allows a total cost of 10.
	cost := 0
	for _, s := range eventSpecs {
		if s.Scope == "" {
			cost++
		}
	}
	if cost > maxUnscopedCost {
		t.Errorf("unscoped subscriptions cost %d, more than %d", cost, maxUnscopedCost)
	}
}

// fakeFollowers serves Get Channel Followers from a list the test changes:
// pages of followerPageSize (cursor = offset) and the single-account check.
type fakeFollowers struct {
	mu   sync.Mutex
	list []map[string]any // newest first
	fail bool
	// skip hides this user from the paged read only, like the list shifting
	// while it is read; the single-account check still finds it.
	skip string
}

func newFakeFollowers(t *testing.T, n int, followedAt time.Time) *fakeFollowers {
	f := &fakeFollowers{}
	for i := range n {
		f.list = append(f.list, map[string]any{"user_id": strconv.Itoa(i), "user_name": "u" + strconv.Itoa(i), "followed_at": followedAt})
	}
	fakeTwitch(t, func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		if f.fail {
			writeJSON(w, 500, map[string]any{"status": 500, "message": "boom"})
			return
		}
		if id := r.URL.Query().Get("user_id"); id != "" {
			data := []any{}
			for _, e := range f.list {
				if e["user_id"] == id {
					data = append(data, e)
				}
			}
			writeJSON(w, 200, map[string]any{"data": data, "total": len(f.list), "pagination": map[string]any{}})
			return
		}
		list := []map[string]any{}
		for _, e := range f.list {
			if e["user_id"] != f.skip {
				list = append(list, e)
			}
		}
		start := 0
		if c := r.URL.Query().Get("after"); c != "" {
			start, _ = strconv.Atoi(c)
		}
		end := min(start+followerPageSize, len(list))
		cursor := ""
		if end < len(list) {
			cursor = strconv.Itoa(end)
		}
		writeJSON(w, 200, map[string]any{"data": list[start:end], "total": len(f.list), "pagination": map[string]any{"cursor": cursor}})
	})
	return f
}

func (f *fakeFollowers) change(fn func(f *fakeFollowers)) {
	f.mu.Lock()
	defer f.mu.Unlock()
	fn(f)
}

func followerWatcher(t *testing.T, path string, got *[]ChannelEvent) *FollowerWatcher {
	t.Helper()
	tok := &Token{AccessToken: "A", RefreshToken: "R", UserID: "9", Scopes: Scopes, ExpiresAt: time.Now().Add(time.Hour)}
	m, _ := NewTokenManager("cid", &memStore{tok: tok}, nopLog{})
	return NewFollowerWatcher(NewHelix(m), m, nopLog{}, path, func(e ChannelEvent) { *got = append(*got, e) })
}

func TestFollowerWatcherReportsUnfollows(t *testing.T) {
	f := newFakeFollowers(t, 150, time.Now().Add(-time.Hour)) // two pages
	var got []ChannelEvent
	w := followerWatcher(t, "", &got)

	w.check(context.Background()) // baseline
	if len(got) != 0 || w.Status().Tracking != 150 {
		t.Fatalf("baseline: events %v status %+v", got, w.Status())
	}
	f.change(func(f *fakeFollowers) {
		f.list = append(f.list[:10], f.list[11:]...) // u10 unfollowed
		f.fail = true
	})
	w.check(context.Background())
	if len(got) != 0 {
		t.Fatal("a failed read must not be taken as mass unfollows")
	}
	f.change(func(f *fakeFollowers) { f.fail = false })
	w.check(context.Background())
	if len(got) != 1 || got[0].Kind != EvUnfollow || got[0].User != "u10" || got[0].Missed {
		t.Fatalf("got %+v", got)
	}
}

func TestFollowerWatcherChecksVanishedAccounts(t *testing.T) {
	f := newFakeFollowers(t, 150, time.Now().Add(-time.Hour))
	var got []ChannelEvent
	w := followerWatcher(t, "", &got)
	w.check(context.Background())
	f.change(func(f *fakeFollowers) { f.skip = "99" }) // hidden at a page edge
	w.check(context.Background())
	if len(got) != 0 {
		t.Fatalf("an account hidden by the list shifting is no unfollow: %+v", got)
	}
	f.change(func(f *fakeFollowers) { f.skip = "" })
	w.check(context.Background())
	if len(got) != 0 || w.Status().Tracking != 150 {
		t.Fatalf("got %+v, status %+v", got, w.Status())
	}
}

func TestFollowerWatcherFindsWhatHappenedWhileOff(t *testing.T) {
	path := filepath.Join(t.TempDir(), "followers.json")
	before := time.Now().Add(-time.Hour)
	f := newFakeFollowers(t, 20, before)
	var got []ChannelEvent
	followerWatcher(t, path, &got).check(context.Background()) // darkbot ran once
	if len(got) != 0 {
		t.Fatalf("baseline: %+v", got)
	}

	// darkbot is off: u3 unfollows, "new" follows.
	offFollow := time.Now().Add(-time.Minute)
	f.change(func(f *fakeFollowers) {
		f.list = append(f.list[:3], f.list[4:]...)
		f.list = append([]map[string]any{{"user_id": "new", "user_name": "Newbie", "followed_at": offFollow}}, f.list...)
	})

	w := followerWatcher(t, path, &got) // next start
	// A follow after the start came through EventSub: not reported again.
	f.change(func(f *fakeFollowers) {
		f.list = append([]map[string]any{{"user_id": "live", "user_name": "Live", "followed_at": time.Now().Add(time.Minute)}}, f.list...)
	})
	w.check(context.Background())
	if len(got) != 2 {
		t.Fatalf("got %+v", got)
	}
	slices.SortFunc(got, func(a, b ChannelEvent) int { return strings.Compare(a.Kind, b.Kind) })
	if fo := got[0]; fo.Kind != EvFollow || fo.User != "Newbie" || !fo.Missed || !fo.At.Equal(offFollow) {
		t.Errorf("missed follow: %+v", fo)
	}
	if un := got[1]; un.Kind != EvUnfollow || un.User != "u3" || !un.Missed {
		t.Errorf("missed unfollow: %+v", un)
	}

	// Later rounds are live again: nothing is "missed" any more.
	got = nil
	f.change(func(f *fakeFollowers) { f.list = f.list[1:] }) // "live" unfollows
	w.check(context.Background())
	if len(got) != 1 || got[0].User != "Live" || got[0].Missed {
		t.Fatalf("got %+v", got)
	}
}

func TestFollowerWatcherIgnoresOtherAccountsList(t *testing.T) {
	path := filepath.Join(t.TempDir(), "followers.json")
	data, _ := json.Marshal(savedFollowers{BroadcasterID: "someone-else", Followers: map[string]string{"x": "X"}})
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	newFakeFollowers(t, 5, time.Now().Add(-time.Hour))
	var got []ChannelEvent
	followerWatcher(t, path, &got).check(context.Background())
	if len(got) != 0 {
		t.Fatalf("another account's list must not produce events: %+v", got)
	}
}

func TestFollowerWatcherSkipsBigChannels(t *testing.T) {
	fakeTwitch(t, func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, 200, map[string]any{"data": []any{}, "total": MaxTrackedFollowers + 1, "pagination": map[string]any{}})
	})
	tok := &Token{AccessToken: "A", RefreshToken: "R", UserID: "9", Scopes: Scopes, ExpiresAt: time.Now().Add(time.Hour)}
	m, _ := NewTokenManager("cid", &memStore{tok: tok}, nopLog{})
	w := NewFollowerWatcher(NewHelix(m), m, nopLog{}, "", func(ChannelEvent) { t.Fatal("no events expected") })
	w.check(context.Background())
	if !w.Status().TooMany {
		t.Fatal("channels above the limit must be skipped")
	}
}

func TestNormalizeRewardColor(t *testing.T) {
	for in, want := range map[string]string{"": "", " #9147ff ": "#9147FF", "00e5cb": "#00E5CB", "#ABCDEF": "#ABCDEF"} {
		if got, ok := NormalizeRewardColor(in); !ok || got != want {
			t.Errorf("%q: got %q %v, want %q", in, got, ok, want)
		}
	}
	for _, bad := range []string{"#12345", "#1234567", "red", "#GGGGGG", "##123456", "rgb(1,2,3)"} {
		if _, ok := NormalizeRewardColor(bad); ok {
			t.Errorf("%q must be rejected", bad)
		}
	}
}

func TestRewardBodyColor(t *testing.T) {
	// Empty color is omitted: Twitch then picks a random one on create and
	// keeps the current one on update (an empty string would re-randomize).
	if _, has := (RewardSettings{Title: "a", Cost: 1}).body()["background_color"]; has {
		t.Fatal("empty color must not be sent")
	}
	if got := (RewardSettings{Title: "a", Cost: 1, BackgroundColor: "#9147FF"}).body()["background_color"]; got != "#9147FF" {
		t.Fatalf("got %v", got)
	}
}

// Limits go as pairs (Update resets the half left out) and are switched
// off with 0, never sent enabled with 0 (Twitch's minimum is 1).
func TestRewardBodyLimits(t *testing.T) {
	b := (RewardSettings{Title: "a", Cost: 1, MaxPerStream: 5, MaxPerUserPerStream: 1}).body()
	if b["is_max_per_stream_enabled"] != true || b["max_per_stream"] != 5 || b["is_max_per_user_per_stream_enabled"] != true || b["max_per_user_per_stream"] != 1 {
		t.Fatalf("limits: %v", b)
	}
	b = (RewardSettings{Title: "a", Cost: 1, MaxPerStream: -3}).body()
	if b["is_max_per_stream_enabled"] != false || b["max_per_stream"] != 0 || b["is_max_per_user_per_stream_enabled"] != false || b["max_per_user_per_stream"] != 0 {
		t.Fatalf("no limits: %v", b)
	}
}

func TestHypeTrainPayloads(t *testing.T) {
	end := `{"id":"t1","level":4,"total":5400,"top_contributions":[
		{"user_id":"1","user_login":"a","user_name":"Anna","type":"bits","total":800},
		{"user_id":"2","user_login":"b","user_name":"Ben","type":"subscription","total":2500},
		{"user_id":"3","user_login":"c","user_name":"Cleo","type":"other","total":100}],
		"shared_train_participants":null,"type":"regular","is_shared_train":false,
		"started_at":"2026-11-18T10:00:00Z","ended_at":"2026-11-18T10:09:00Z","cooldown_ends_at":"2026-11-18T11:09:00Z"}`
	got, err := parseChannelEvent("channel.hype_train.end", json.RawMessage(end))
	if err != nil || got != (ChannelEvent{Kind: EvHypeEnd, Amount: 4, TrainID: "t1", Top: "Ben"}) {
		t.Fatalf("end: %+v %v (top = most points: Ben's tier 3 gifts beat Anna's 800 Bits)", got, err)
	}
	prog, err := parseChannelEvent("channel.hype_train.progress", json.RawMessage(`{"id":"t1","level":2,"total":1900,"progress":200,"goal":1800}`))
	if err != nil || prog != (ChannelEvent{Kind: evHypeProgress, Amount: 2, TrainID: "t1"}) {
		t.Fatalf("progress: %+v %v", prog, err)
	}
}

func TestHypeTrackerLevelUps(t *testing.T) {
	var h hypeTracker
	step := func(kind, train string, level int) (string, bool) {
		ev, ok := h.filter(ChannelEvent{Kind: kind, TrainID: train, Amount: level})
		return ev.Kind, ok
	}
	type want struct {
		kind string
		ok   bool
	}
	steps := []struct {
		kind, train string
		level       int
		want        want
	}{
		{EvHypeBegin, "t1", 1, want{EvHypeBegin, true}},
		{evHypeProgress, "t1", 1, want{evHypeProgress, false}}, // progress within level 1
		{evHypeProgress, "t1", 2, want{EvHypeLevel, true}},     // level up
		{evHypeProgress, "t1", 2, want{evHypeProgress, false}}, // same level again
		{evHypeProgress, "t1", 4, want{EvHypeLevel, true}},     // skipped level 3: one event
		{EvHypeEnd, "t1", 4, want{EvHypeEnd, true}},
		// connected mid-train: the first progress is only the baseline
		{evHypeProgress, "t2", 3, want{evHypeProgress, false}},
		{evHypeProgress, "t2", 3, want{evHypeProgress, false}},
		{evHypeProgress, "t2", 4, want{EvHypeLevel, true}},
		{EvFollow, "", 0, want{EvFollow, true}}, // other events pass through
	}
	for i, s := range steps {
		if k, ok := step(s.kind, s.train, s.level); k != s.want.kind || ok != s.want.ok {
			t.Errorf("step %d (%s %s L%d): got %s %v, want %+v", i, s.kind, s.train, s.level, k, ok, s.want)
		}
	}
}

func TestChannelTotals(t *testing.T) {
	fakeTwitch(t, func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		if q.Get("broadcaster_id") != "42" || q.Get("first") != "1" {
			writeJSON(w, 400, map[string]any{"status": 400, "message": "only the totals are needed: first=1"})
			return
		}
		switch r.URL.Path {
		case "/helix/channels/followers":
			writeJSON(w, 200, map[string]any{"total": 1834, "data": []map[string]any{{"user_id": "1", "user_name": "a"}}, "pagination": map[string]any{"cursor": "x"}})
		case "/helix/subscriptions":
			writeJSON(w, 200, map[string]any{"total": 42, "points": 58, "data": []map[string]any{{"user_id": "1", "tier": "1000"}}})
		default:
			http.NotFound(w, r)
		}
	})
	m, _ := NewTokenManager("cid", &memStore{tok: &Token{AccessToken: "A1", RefreshToken: "R1", UserID: "42"}}, nopLog{})
	h := NewHelix(m)
	if n, err := h.GetFollowerCount(context.Background(), "42"); err != nil || n != 1834 {
		t.Fatalf("followers %d %v", n, err)
	}
	if n, p, err := h.GetSubscriptionTotals(context.Background(), "42"); err != nil || n != 42 || p != 58 {
		t.Fatalf("subs %d points %d %v", n, p, err)
	}
}

func TestLiveSince(t *testing.T) {
	live := true
	fakeTwitch(t, func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		if r.URL.Path != "/helix/streams" || q.Get("user_id") != "42" || q.Get("type") != "live" {
			writeJSON(w, 400, map[string]any{"status": 400, "message": "unexpected request"})
			return
		}
		data := []map[string]any{}
		if live {
			data = append(data, map[string]any{"id": "1", "user_id": "42", "type": "live", "started_at": "2026-11-18T19:00:00Z"})
		}
		writeJSON(w, 200, map[string]any{"data": data, "pagination": map[string]any{}})
	})
	m, _ := NewTokenManager("cid", &memStore{tok: &Token{AccessToken: "A1", RefreshToken: "R1", UserID: "42"}}, nopLog{})
	h := NewHelix(m)
	on, since, err := h.LiveSince(context.Background(), "42")
	if err != nil || !on || !since.Equal(time.Date(2026, 11, 18, 19, 0, 0, 0, time.UTC)) {
		t.Fatalf("live: %v %v %v", on, since, err)
	}
	live = false // offline: Twitch returns an empty list
	if on, _, err := h.LiveSince(context.Background(), "42"); err != nil || on {
		t.Fatalf("offline: %v %v", on, err)
	}
}

// Examples shaped like the reference's responses for Get Channel Followers
// (user_id given) and Get Users (login given).
func TestFollowedAtAndUserByLogin(t *testing.T) {
	fakeTwitch(t, func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		switch {
		case r.URL.Path == "/helix/channels/followers" && q.Get("broadcaster_id") == "42" && q.Get("user_id") == "11111":
			writeJSON(w, 200, map[string]any{"total": 8, "data": []map[string]any{{"user_id": "11111", "user_name": "UserDisplayName", "user_login": "userloginname", "followed_at": "2022-05-24T22:22:08Z"}}, "pagination": map[string]any{}})
		case r.URL.Path == "/helix/channels/followers" && q.Get("user_id") != "":
			writeJSON(w, 200, map[string]any{"total": 8, "data": []any{}, "pagination": map[string]any{}})
		case r.URL.Path == "/helix/users" && q.Get("login") == "twitchdev":
			writeJSON(w, 200, map[string]any{"data": []map[string]any{{"id": "141981764", "login": "twitchdev", "display_name": "TwitchDev", "type": "", "broadcaster_type": "partner"}}})
		case r.URL.Path == "/helix/users":
			writeJSON(w, 200, map[string]any{"data": []any{}})
		default:
			writeJSON(w, 400, map[string]any{"status": 400, "message": "unexpected request"})
		}
	})
	m, _ := NewTokenManager("cid", &memStore{tok: &Token{AccessToken: "A1", RefreshToken: "R1", UserID: "42"}}, nopLog{})
	h := NewHelix(m)
	ctx := context.Background()
	at, ok, err := h.FollowedAt(ctx, "42", "11111")
	if err != nil || !ok || !at.Equal(time.Date(2022, 5, 24, 22, 22, 8, 0, time.UTC)) {
		t.Fatalf("follower: %v %v %v", at, ok, err)
	}
	if _, ok, err := h.FollowedAt(ctx, "42", "999"); err != nil || ok {
		t.Fatalf("not following: %v %v", ok, err)
	}
	u, ok, err := h.UserByLogin(ctx, "TwitchDev")
	if err != nil || !ok || u.ID != "141981764" || u.DisplayName != "TwitchDev" {
		t.Fatalf("user: %+v %v %v", u, ok, err)
	}
	if _, ok, err := h.UserByLogin(ctx, "nobody_here"); err != nil || ok {
		t.Fatalf("unknown login: %v %v", ok, err)
	}
}

func TestRevocation(t *testing.T) {
	e := &EventSub{log: nopLog{}}
	msg := func(typ, status string) json.RawMessage {
		return json.RawMessage(`{"subscription":{"type":"` + typ + `","status":"` + status + `"}}`)
	}
	if err := e.revoked(msg("channel.follow", "version_removed")); err != nil {
		t.Fatalf("one event type ending must not stop EventSub: %v", err)
	}
	if err := e.revoked(msg(redemptionAddType, "websocket_disconnected")); err == nil || errors.Is(err, ErrTokenRejected) {
		t.Fatalf("lost redemptions must reconnect, not log out: %v", err)
	}
	// A version Twitch no longer supports: subscribing again would fail
	// forever, so no reconnect loop.
	if err := e.revoked(msg(redemptionAddType, revokedVersionRemoved)); err != nil {
		t.Fatalf("a removed version must not reconnect: %v", err)
	}
	if err := e.revoked(msg("channel.follow", revokedAuthorization)); !errors.Is(err, ErrTokenRejected) {
		t.Fatalf("a revoked login must stop EventSub: %v", err)
	}
}

// A follow that arrived live just before darkbot closed is saved with the
// list, so the next start doesn't report it again as missed.
func TestFollowerWatcherKeepsLiveFollows(t *testing.T) {
	path := filepath.Join(t.TempDir(), "followers.json")
	f := newFakeFollowers(t, 5, time.Now().Add(-time.Hour))
	var got []ChannelEvent
	w := followerWatcher(t, path, &got)
	w.check(context.Background())

	// Live follow (EventSub) after the last check; darkbot closes before
	// the next check (Run's exit flushes the same way).
	when := time.Now()
	w.Observe("late", "Late")
	w.Flush()
	f.change(func(f *fakeFollowers) {
		f.list = append([]map[string]any{{"user_id": "late", "user_name": "Late", "followed_at": when}}, f.list...)
	})

	next := followerWatcher(t, path, &got)
	next.started = when.Add(time.Minute) // the next start is later
	next.check(context.Background())
	if len(got) != 0 {
		t.Fatalf("the live follow was reported again: %+v", got)
	}
}

func TestLogoutRevokes(t *testing.T) {
	var revoked atomic.Value
	fakeTwitch(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/revoke" {
			_ = r.ParseForm()
			revoked.Store(r.Form.Get("client_id") + "|" + r.Form.Get("token"))
		}
		w.WriteHeader(http.StatusOK)
	})
	store := &memStore{tok: &Token{AccessToken: "A", RefreshToken: "R", UserID: "9", Scopes: Scopes, ExpiresAt: time.Now().Add(time.Hour)}}
	m, _ := NewTokenManager("cid", store, nopLog{})
	if err := m.Logout(); err != nil {
		t.Fatal(err)
	}
	if m.Current() != nil || !store.deleted {
		t.Fatal("not logged out locally")
	}
	if got, _ := revoked.Load().(string); got != "cid|A" {
		t.Fatalf("revoked %q", got)
	}
}

func TestFlexInt(t *testing.T) {
	for raw, want := range map[string]int{`60`: 60, `"60"`: 60, `0`: 0, `null`: 0} {
		var n flexInt
		if err := json.Unmarshal([]byte(raw), &n); err != nil || int(n) != want {
			t.Errorf("%s: %d %v", raw, n, err)
		}
	}
	for _, raw := range []string{`"abc"`, `1.5`, `true`} {
		var n flexInt
		if err := json.Unmarshal([]byte(raw), &n); err == nil {
			t.Errorf("%s was accepted", raw)
		}
	}
}

func TestIsDeprecated(t *testing.T) {
	dep := &APIError{Status: 400, Message: "The subscription type and version combination is deprecated."}
	other := &APIError{Status: 400, Message: "websocket transport session does not exist or has already disconnected"}
	if !isDeprecated(dep) || isDeprecated(other) || isDeprecated(&APIError{Status: 500, Message: "deprecated"}) {
		t.Fatal("only Twitch's 400 deprecated answer is lasting")
	}
}

func TestEveryKindHasACategory(t *testing.T) {
	for _, k := range EventKinds {
		if c := Category(k); !slices.Contains(EventCategories, c) {
			t.Errorf("%s has no category (%q)", k, c)
		}
	}
}

func TestChannelTracker(t *testing.T) {
	var c channelTracker
	up := func(title, cat string) bool {
		return c.filter(ChannelEvent{Kind: EvChannelUpdate, Title: title, Category: cat})
	}
	c.seed("Old", "Chess")
	if up("Old", "Chess") {
		t.Error("only the language or labels changed: must be dropped")
	}
	if !up("New", "Chess") || !up("New", "Art") {
		t.Error("a new title or category must be reported")
	}
	if !c.filter(ChannelEvent{Kind: EvFollow}) {
		t.Error("other events must pass")
	}
	var fresh channelTracker // not seeded (reading the channel failed)
	if !fresh.filter(ChannelEvent{Kind: EvChannelUpdate, Title: "x"}) {
		t.Error("without a known state the first update is reported")
	}
}

func TestHypeSeed(t *testing.T) {
	var h hypeTracker
	h.seed("train", 2)
	if ev, ok := h.filter(ChannelEvent{Kind: evHypeProgress, TrainID: "train", Amount: 3}); !ok || ev.Kind != EvHypeLevel {
		t.Fatalf("level-up after a restart lost: %+v %v", ev, ok)
	}
}

func TestChannelInfoAndHypeNow(t *testing.T) {
	train := true
	fakeTwitch(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/helix/channels":
			writeJSON(w, 200, map[string]any{"data": []map[string]any{{"broadcaster_id": "42", "title": "T", "game_name": "G", "game_id": "1"}}})
		case "/helix/hypetrain/status":
			var cur any
			if train {
				cur = map[string]any{"id": "h1", "level": 3, "total": 900}
			}
			writeJSON(w, 200, map[string]any{"data": []map[string]any{{"current": cur, "all_time_high": nil}}})
		default:
			http.NotFound(w, r)
		}
	})
	m, _ := NewTokenManager("cid", &memStore{tok: &Token{AccessToken: "A1", RefreshToken: "R1", UserID: "42"}}, nopLog{})
	h := NewHelix(m)
	if c, err := h.ChannelInfo(context.Background(), "42"); err != nil || c.Title != "T" || c.Category != "G" {
		t.Fatalf("channel: %+v %v", c, err)
	}
	if id, lvl, err := h.HypeTrainNow(context.Background(), "42"); err != nil || id != "h1" || lvl != 3 {
		t.Fatalf("train: %s %d %v", id, lvl, err)
	}
	train = false
	if id, _, err := h.HypeTrainNow(context.Background(), "42"); err != nil || id != "" {
		t.Fatalf("no train: %q %v", id, err)
	}
}

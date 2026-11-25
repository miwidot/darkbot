package twitch

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
)

// sessionWait bounds every wait in these tests, so a regression fails
// instead of hanging.
const sessionWait = 5 * time.Second

// fakeEventSub serves an EventSub WebSocket that sends a welcome and then
// the given messages, and a Helix whose subscription calls are answered by
// subscribe.
func fakeEventSub(t *testing.T, subscribe http.HandlerFunc, messages ...string) {
	t.Helper()
	fakeTwitch(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/helix/eventsub/subscriptions" {
			subscribe(w, r)
			return
		}
		http.NotFound(w, r) // channel info, hype train: logged, not needed here
	})
	ws := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		defer c.CloseNow()
		ctx := r.Context()
		welcome := `{"metadata":{"message_id":"w","message_type":"session_welcome"},
			"payload":{"session":{"id":"s1","keepalive_timeout_seconds":30}}}`
		for _, m := range append([]string{welcome}, messages...) {
			if c.Write(ctx, websocket.MessageText, []byte(m)) != nil {
				return
			}
		}
		c.Read(ctx) // hold the connection until the client leaves
	}))
	t.Cleanup(ws.Close)
	old := eventSubURL
	eventSubURL = "ws" + strings.TrimPrefix(ws.URL, "http")
	t.Cleanup(func() { eventSubURL = old })
}

func chatNotification(id, text string) string {
	b, _ := json.Marshal(map[string]any{
		"metadata": map[string]string{"message_id": id, "message_type": "notification", "subscription_type": chatMessageType},
		"payload": map[string]any{"event": map[string]any{
			"broadcaster_user_id": "42", "chatter_user_id": "7", "chatter_user_name": "Viewer",
			"message_id": id, "message": map[string]string{"text": text}, "badges": []any{},
		}},
	})
	return string(b)
}

func testTokens(t *testing.T) *TokenManager {
	t.Helper()
	m, err := NewTokenManager("cid", &memStore{tok: &Token{
		AccessToken: "A1", RefreshToken: "R1", UserID: "42", Login: "me",
		Scopes: []string{ScopeReadChat},
	}}, nopLog{})
	if err != nil {
		t.Fatal(err)
	}
	return m
}

// A chat message sent right after connecting is handled while the
// subscriptions are still being created, not after the last one.
func TestChatArrivesWhileSubscribing(t *testing.T) {
	release := make(chan struct{})
	fakeEventSub(t, func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-release:
		case <-r.Context().Done():
			return
		}
		writeJSON(w, http.StatusAccepted, map[string]any{"data": []any{}})
	}, chatNotification("c1", "!horn"))

	tok := testTokens(t)
	status := make(chan bool, 4)
	e := NewEventSub(NewHelix(tok), tok, nopLog{}, func(Redemption) {}, func(up bool) { status <- up })
	chat := make(chan ChatMessage, 1)
	e.OnChat(func(m ChatMessage) { chat <- m })

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { e.Run(ctx); close(done) }()
	defer func() { cancel(); <-done }()

	select {
	case m := <-chat:
		if m.Text != "!horn" {
			t.Fatalf("chat %+v", m)
		}
	case <-time.After(sessionWait):
		t.Fatal("chat message waited for the subscriptions")
	}
	select {
	case up := <-status:
		t.Fatalf("reported connected=%v before the subscriptions were made", up)
	default:
	}
	close(release)
	select {
	case up := <-status:
		if !up {
			t.Fatal("want connected after subscribing")
		}
	case <-time.After(sessionWait):
		t.Fatal("never reported connected")
	}
}

// A redemption subscription Twitch refuses for a passing reason ends the
// session (to subscribe anew), even though reading already runs.
func TestFailedRedemptionSubscriptionEndsSession(t *testing.T) {
	fakeEventSub(t, func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "Bad Request", "status": 400, "message": "session gone"})
	})
	tok := testTokens(t)
	e := NewEventSub(NewHelix(tok), tok, nopLog{}, func(Redemption) {}, func(bool) {})
	ctx, cancel := context.WithTimeout(context.Background(), sessionWait)
	defer cancel()
	err := e.session(ctx)
	if err == nil || !strings.Contains(err.Error(), "subscribe to redemptions") {
		t.Fatalf("session ended with %v, want the subscription error", err)
	}
	if ctx.Err() != nil {
		t.Fatal("the session did not end by itself")
	}
}

// A notification that arrived while subscribing is newer than the state
// read afterwards; the read must not overwrite it. A new session forgets it.
func TestSeedAfterNotification(t *testing.T) {
	var c channelTracker
	c.filter(ChannelEvent{Kind: EvChannelUpdate, Title: "New", Category: "Art"})
	c.seed("Old", "Art")
	if c.filter(ChannelEvent{Kind: EvChannelUpdate, Title: "New", Category: "Art"}) {
		t.Error("the older read replaced the newer update")
	}
	c.reset()
	if !c.filter(ChannelEvent{Kind: EvChannelUpdate, Title: "New", Category: "Art"}) {
		t.Error("after a reset the first update is reported")
	}

	var h hypeTracker
	h.filter(ChannelEvent{Kind: EvHypeBegin, TrainID: "t", Amount: 3})
	h.seed("t", 2)
	if _, ok := h.filter(ChannelEvent{Kind: evHypeProgress, TrainID: "t", Amount: 3}); ok {
		t.Error("the older read lowered the level, so level 3 was reported twice")
	}
	h.reset()
	h.seed("t2", 1)
	if ev, ok := h.filter(ChannelEvent{Kind: evHypeProgress, TrainID: "t2", Amount: 2}); !ok || ev.Kind != EvHypeLevel {
		t.Error("after a reset the seed must apply")
	}
}

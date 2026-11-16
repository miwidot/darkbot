package twitch

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/coder/websocket"
)

// EventSub WebSocket (verified 2026-11-16):
// https://dev.twitch.tv/docs/eventsub/handling-websocket-events/
//
//   - We connect, receive session_welcome and must subscribe within 10 s.
//   - If neither a notification nor a keepalive arrives within
//     keepalive_timeout_seconds, the connection is considered dead.
//   - session_reconnect: connect to reconnect_url as-is, keep the old socket
//     until the new welcome arrives; subscriptions move over automatically.
//   - After a real drop, subscriptions are gone and must be recreated.
//   - We must never send messages ourselves (only pongs, handled by the lib).
const (
	eventSubURL = "wss://eventsub.wss.twitch.tv/ws"
	// keepaliveSeconds asks Twitch for a keepalive every 30 s (allowed 10–600).
	keepaliveSeconds = 30
	// keepaliveGrace tolerates network jitter on top of the keepalive window.
	keepaliveGrace = 10 * time.Second
	welcomeTimeout = 15 * time.Second
	readLimitBytes = 1 << 20
	minBackoff     = time.Second
	maxBackoff     = time.Minute
	// stableSession resets the backoff once a session lived this long.
	stableSession = time.Minute
	// dedupeWindow: Twitch may resend a message; ignore duplicate message_ids.
	dedupeWindow = 10 * time.Minute

	redemptionAddType = "channel.channel_points_custom_reward_redemption.add"
)

// Redemption is one channel point redemption.
type Redemption struct {
	ID          string
	RewardID    string
	RewardTitle string
	UserID      string
	UserLogin   string
	UserName    string
	UserInput   string
	RedeemedAt  time.Time
}

type wsMessage struct {
	Metadata struct {
		MessageID        string `json:"message_id"`
		MessageType      string `json:"message_type"`
		SubscriptionType string `json:"subscription_type"`
	} `json:"metadata"`
	Payload json.RawMessage `json:"payload"`
}

type sessionPayload struct {
	Session struct {
		ID                      string  `json:"id"`
		KeepaliveTimeoutSeconds int     `json:"keepalive_timeout_seconds"`
		ReconnectURL            *string `json:"reconnect_url"`
	} `json:"session"`
}

type notificationPayload struct {
	Subscription struct {
		Type   string `json:"type"`
		Status string `json:"status"`
	} `json:"subscription"`
	Event struct {
		ID         string    `json:"id"`
		UserID     string    `json:"user_id"`
		UserLogin  string    `json:"user_login"`
		UserName   string    `json:"user_name"`
		UserInput  string    `json:"user_input"`
		RedeemedAt time.Time `json:"redeemed_at"`
		Reward     struct {
			ID    string `json:"id"`
			Title string `json:"title"`
		} `json:"reward"`
	} `json:"event"`
}

// EventSub listens for channel point redemptions.
type EventSub struct {
	helix    *Helix
	tokens   *TokenManager
	log      Logger
	onEvent  func(Redemption)
	onStatus func(connected bool)

	seenMu sync.Mutex
	seen   map[string]time.Time
}

// NewEventSub creates the listener. onEvent/onStatus may be called from any goroutine.
func NewEventSub(h *Helix, t *TokenManager, log Logger, onEvent func(Redemption), onStatus func(bool)) *EventSub {
	return &EventSub{helix: h, tokens: t, log: log, onEvent: onEvent, onStatus: onStatus, seen: map[string]time.Time{}}
}

// Run connects and reconnects until ctx is cancelled.
func (e *EventSub) Run(ctx context.Context) {
	backoff := minBackoff
	for ctx.Err() == nil {
		started := time.Now()
		err := e.session(ctx)
		e.onStatus(false)
		if ctx.Err() != nil {
			return
		}
		if errors.Is(err, ErrNotLoggedIn) || errors.Is(err, ErrTokenRejected) {
			e.log.Warnf("EventSub stopped: %v", err)
			return
		}
		if time.Since(started) > stableSession {
			backoff = minBackoff
		}
		e.log.Warnf("EventSub disconnected (%v), reconnecting in %s", err, backoff)
		select {
		case <-ctx.Done():
			return
		case <-time.After(backoff):
		}
		backoff = min(backoff*2, maxBackoff)
	}
}

// session runs one logical session including any server-requested reconnects.
func (e *EventSub) session(ctx context.Context) error {
	url := fmt.Sprintf("%s?keepalive_timeout_seconds=%d", eventSubURL, keepaliveSeconds)
	conn, welcome, err := e.dial(ctx, url)
	if err != nil {
		return err
	}
	defer func() { conn.CloseNow() }()

	tok := e.tokens.Current()
	if tok == nil {
		return ErrNotLoggedIn
	}
	if err := e.helix.SubscribeRedemptions(ctx, welcome.Session.ID, tok.UserID); err != nil {
		return fmt.Errorf("subscribe to redemptions: %w", err)
	}
	e.log.Infof("Connected to Twitch EventSub, listening for channel point redemptions of %s", tok.Login)
	e.onStatus(true)

	keepalive := time.Duration(welcome.Session.KeepaliveTimeoutSeconds) * time.Second
	for {
		msg, err := readMsg(ctx, conn, keepalive+keepaliveGrace)
		if err != nil {
			return err
		}
		switch msg.Metadata.MessageType {
		case "session_keepalive":
		case "notification":
			e.handleNotification(msg)
		case "revocation":
			e.log.Errorf("Twitch revoked the redemption subscription (%s). Please log in again.", string(msg.Payload))
			return ErrTokenRejected
		case "session_reconnect":
			var sp sessionPayload
			if err := json.Unmarshal(msg.Payload, &sp); err != nil || sp.Session.ReconnectURL == nil {
				return fmt.Errorf("bad session_reconnect message: %v", err)
			}
			newConn, newWelcome, err := e.reconnect(ctx, conn, *sp.Session.ReconnectURL)
			if err != nil {
				return err
			}
			conn = newConn
			keepalive = time.Duration(newWelcome.Session.KeepaliveTimeoutSeconds) * time.Second
			e.log.Infof("EventSub moved to a new server (requested by Twitch)")
		}
	}
}

// reconnect dials reconnectURL while still draining the old connection, so no
// notification sent on the old socket during the switch is lost.
func (e *EventSub) reconnect(ctx context.Context, old *websocket.Conn, reconnectURL string) (*websocket.Conn, *sessionPayload, error) {
	drainCtx, stopDrain := context.WithCancel(ctx)
	drained := make(chan struct{})
	go func() {
		defer close(drained)
		for {
			msg, err := readMsg(drainCtx, old, welcomeTimeout*4)
			if err != nil {
				return
			}
			if msg.Metadata.MessageType == "notification" {
				e.handleNotification(msg)
			}
		}
	}()

	newConn, welcome, err := e.dial(ctx, reconnectURL)
	stopDrain()
	old.Close(websocket.StatusNormalClosure, "")
	<-drained
	if err != nil {
		return nil, nil, fmt.Errorf("reconnect: %w", err)
	}
	return newConn, welcome, nil
}

func (e *EventSub) dial(ctx context.Context, url string) (*websocket.Conn, *sessionPayload, error) {
	dctx, cancel := context.WithTimeout(ctx, welcomeTimeout)
	defer cancel()
	conn, _, err := websocket.Dial(dctx, url, nil)
	if err != nil {
		return nil, nil, fmt.Errorf("connect to EventSub: %w", err)
	}
	conn.SetReadLimit(readLimitBytes)

	msg, err := readMsg(ctx, conn, welcomeTimeout)
	if err != nil {
		conn.CloseNow()
		return nil, nil, fmt.Errorf("wait for welcome: %w", err)
	}
	if msg.Metadata.MessageType != "session_welcome" {
		conn.CloseNow()
		return nil, nil, fmt.Errorf("expected session_welcome, got %q", msg.Metadata.MessageType)
	}
	var sp sessionPayload
	if err := json.Unmarshal(msg.Payload, &sp); err != nil {
		conn.CloseNow()
		return nil, nil, fmt.Errorf("parse welcome: %w", err)
	}
	if sp.Session.KeepaliveTimeoutSeconds <= 0 {
		sp.Session.KeepaliveTimeoutSeconds = keepaliveSeconds
	}
	return conn, &sp, nil
}

func readMsg(ctx context.Context, conn *websocket.Conn, timeout time.Duration) (*wsMessage, error) {
	rctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	_, data, err := conn.Read(rctx)
	if err != nil {
		if errors.Is(rctx.Err(), context.DeadlineExceeded) {
			return nil, errors.New("no keepalive from Twitch, connection considered lost")
		}
		return nil, err
	}
	var msg wsMessage
	if err := json.Unmarshal(data, &msg); err != nil {
		return nil, fmt.Errorf("parse EventSub message: %w", err)
	}
	return &msg, nil
}

func (e *EventSub) handleNotification(msg *wsMessage) {
	if msg.Metadata.SubscriptionType != redemptionAddType || e.isDuplicate(msg.Metadata.MessageID) {
		return
	}
	var np notificationPayload
	if err := json.Unmarshal(msg.Payload, &np); err != nil {
		e.log.Warnf("ignore malformed redemption: %v", err)
		return
	}
	ev := np.Event
	e.onEvent(Redemption{
		ID:          ev.ID,
		RewardID:    ev.Reward.ID,
		RewardTitle: ev.Reward.Title,
		UserID:      ev.UserID,
		UserLogin:   ev.UserLogin,
		UserName:    ev.UserName,
		UserInput:   ev.UserInput,
		RedeemedAt:  ev.RedeemedAt,
	})
}

func (e *EventSub) isDuplicate(id string) bool {
	now := time.Now()
	e.seenMu.Lock()
	defer e.seenMu.Unlock()
	for k, t := range e.seen {
		if now.Sub(t) > dedupeWindow {
			delete(e.seen, k)
		}
	}
	if _, ok := e.seen[id]; ok {
		return true
	}
	e.seen[id] = now
	return false
}

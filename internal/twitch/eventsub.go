package twitch

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
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
	// dedupePrune: how often expired ids are dropped from the dedupe set.
	dedupePrune = 30 * time.Second

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
	helix          *Helix
	tokens         *TokenManager
	log            Logger
	onEvent        func(Redemption)
	onStatus       func(connected bool)
	onChat         func(ChatMessage)  // nil: chat is not subscribed
	onChannelEvent func(ChannelEvent) // nil: channel events are not subscribed

	seenMu sync.Mutex
	hype   hypeTracker
	seen   map[string]time.Time
	pruned time.Time // last walk over seen
}

// NewEventSub creates the listener. onEvent/onStatus may be called from any goroutine.
func NewEventSub(h *Helix, t *TokenManager, log Logger, onEvent func(Redemption), onStatus func(bool)) *EventSub {
	return &EventSub{helix: h, tokens: t, log: log, onEvent: onEvent, onStatus: onStatus, seen: map[string]time.Time{}}
}

// OnChat also subscribes to chat messages (needs the user:read:chat scope;
// without it only redemptions are received). Call before Run.
func (e *EventSub) OnChat(fn func(ChatMessage)) { e.onChat = fn }

// OnChannelEvent also subscribes to channel events (follows, subs, bits,
// raids, ...); each type only if the login has its scope. Call before Run.
func (e *EventSub) OnChannelEvent(fn func(ChannelEvent)) { e.onChannelEvent = fn }

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
	wsURL := fmt.Sprintf("%s?keepalive_timeout_seconds=%d", eventSubURL, keepaliveSeconds)
	conn, welcome, err := e.dial(ctx, wsURL)
	if err != nil {
		return err
	}
	defer func() { conn.CloseNow() }()

	tok := e.tokens.Current()
	if tok == nil {
		return ErrNotLoggedIn
	}
	switch err := e.helix.SubscribeRedemptions(ctx, welcome.Session.ID, tok.UserID); {
	case errors.Is(err, ErrNotAffiliate):
		// Lasting: channel points need Affiliate/Partner. Chat commands,
		// song requests and events still work, so carry on without them.
		e.log.Warnf("Connected to Twitch EventSub without channel point redemptions: %v", err)
	case isDeprecated(err):
		// Lasting: retrying can't help, the rest still works.
		e.log.Errorf("Connected to Twitch EventSub without channel point redemptions, Twitch refused the subscription (update darkbot): %v", err)
	case err != nil:
		return fmt.Errorf("subscribe to redemptions: %w", err) // retried with backoff
	default:
		e.log.Infof("Connected to Twitch EventSub, listening for channel point redemptions of %s", tok.Login)
	}
	e.subscribeChat(ctx, welcome.Session.ID, tok)
	e.subscribeEvents(ctx, welcome.Session.ID, tok)
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
			if err := e.revoked(msg.Payload); err != nil {
				return err
			}
		case "session_reconnect":
			var sp sessionPayload
			if err := json.Unmarshal(msg.Payload, &sp); err != nil || sp.Session.ReconnectURL == nil {
				return fmt.Errorf("bad session_reconnect message: %v", err)
			}
			if !isTwitchWSURL(*sp.Session.ReconnectURL) {
				return fmt.Errorf("refusing reconnect to unexpected URL %q", *sp.Session.ReconnectURL)
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

// revocationPayload is the part of a revocation message darkbot reads
// (https://dev.twitch.tv/docs/eventsub/handling-websocket-events/#revocation-message).
type revocationPayload struct {
	Subscription struct {
		Type   string `json:"type"`
		Status string `json:"status"`
	} `json:"subscription"`
}

// Revocation statuses that mean the login itself is gone.
const (
	revokedAuthorization = "authorization_revoked"
	revokedUserRemoved   = "user_removed"
	// revokedVersionRemoved: "The subscribed to subscription type and
	// version is no longer supported" (WebSocket guide, checked 2026-11-19).
	revokedVersionRemoved = "version_removed"
)

// revoked handles a revocation: a withdrawn login stops EventSub (the user
// has to log in again); a lost redemption subscription reconnects to
// subscribe anew; any other type only costs that one event type.
func (e *EventSub) revoked(payload json.RawMessage) error {
	var p revocationPayload
	if err := json.Unmarshal(payload, &p); err != nil {
		return fmt.Errorf("bad revocation message: %w", err)
	}
	s := p.Subscription
	switch {
	case s.Status == revokedAuthorization || s.Status == revokedUserRemoved:
		e.log.Errorf("Twitch revoked darkbot's access (%s). Please log in again.", s.Status)
		return ErrTokenRejected
	case s.Type == redemptionAddType && s.Status == revokedVersionRemoved:
		// Lasting: subscribing again would fail every time.
		e.log.Errorf("Twitch no longer supports the redemption subscription darkbot uses; update darkbot. Chat, songs and events keep running")
		return nil
	case s.Type == redemptionAddType:
		return fmt.Errorf("twitch ended the redemption subscription (%s)", s.Status)
	}
	e.log.Warnf("Twitch ended the %s subscription (%s); the others keep running", s.Type, s.Status)
	return nil
}

// subscribeChat adds the chat subscription. Failing here must not cost the
// redemptions (the sound bot's main job), so errors are only logged.
func (e *EventSub) subscribeChat(ctx context.Context, sessionID string, tok *Token) {
	if e.onChat == nil {
		return
	}
	if !tok.HasScope(ScopeReadChat) {
		e.log.Warnf("Chat commands are off: reconnect Twitch once to allow reading chat")
		return
	}
	if err := e.helix.SubscribeChat(ctx, sessionID, tok.UserID); err != nil {
		e.log.Errorf("Could not subscribe to chat, chat commands are off: %v", err)
		return
	}
	e.log.Infof("Listening for chat commands")
}

// subscribeEvents adds the channel event subscriptions. Like chat, a failure
// only costs that one event type.
func (e *EventSub) subscribeEvents(ctx context.Context, sessionID string, tok *Token) {
	if e.onChannelEvent == nil {
		return
	}
	var ok, missing []string
	for _, s := range eventSpecs {
		if ctx.Err() != nil {
			return // stopped (logout, restart): no warning per remaining type
		}
		if s.Scope != "" && !tok.HasScope(s.Scope) {
			missing = append(missing, s.Type)
			continue
		}
		if err := e.helix.subscribe(ctx, sessionID, s.Type, s.Version, s.condition(tok.UserID)); err != nil {
			e.log.Warnf("Could not subscribe to %s: %v", s.Type, err)
			continue
		}
		ok = append(ok, s.Type)
	}
	e.log.Infof("Listening for %d channel event types", len(ok))
	if len(missing) > 0 {
		e.log.Warnf("Reconnect Twitch once to also receive: %s", strings.Join(missing, ", "))
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

func (e *EventSub) dial(ctx context.Context, wsURL string) (*websocket.Conn, *sessionPayload, error) {
	dctx, cancel := context.WithTimeout(ctx, welcomeTimeout)
	defer cancel()
	conn, _, err := websocket.Dial(dctx, wsURL, nil)
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

// isTwitchWSURL allows only secure WebSocket URLs on twitch.tv hosts.
func isTwitchWSURL(raw string) bool {
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "wss" {
		return false
	}
	h := strings.ToLower(u.Hostname())
	return h == "twitch.tv" || strings.HasSuffix(h, ".twitch.tv")
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
	switch msg.Metadata.SubscriptionType {
	case redemptionAddType:
	case chatMessageType:
		if e.onChat == nil || e.isDuplicate(msg.Metadata.MessageID) {
			return
		}
		var p struct {
			Event json.RawMessage `json:"event"`
		}
		if err := json.Unmarshal(msg.Payload, &p); err != nil {
			e.log.Warnf("ignore malformed chat message: %v", err)
			return
		}
		m, ok, err := parseChatEvent(p.Event)
		if err != nil {
			e.log.Warnf("ignore malformed chat message: %v", err)
			return
		}
		if ok {
			e.onChat(m)
		}
		return
	default:
		e.handleChannelEvent(msg)
		return
	}
	if e.isDuplicate(msg.Metadata.MessageID) {
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

func (e *EventSub) handleChannelEvent(msg *wsMessage) {
	if e.onChannelEvent == nil {
		return
	}
	if _, known := specByType(msg.Metadata.SubscriptionType); !known || e.isDuplicate(msg.Metadata.MessageID) {
		return
	}
	var p struct {
		Event json.RawMessage `json:"event"`
	}
	if err := json.Unmarshal(msg.Payload, &p); err != nil {
		e.log.Warnf("ignore malformed %s event: %v", msg.Metadata.SubscriptionType, err)
		return
	}
	ev, err := parseChannelEvent(msg.Metadata.SubscriptionType, p.Event)
	if err != nil {
		e.log.Warnf("ignore malformed %s event: %v", msg.Metadata.SubscriptionType, err)
		return
	}
	if ev.Kind == "" {
		return // a notification darkbot doesn't use (e.g. other chat notices)
	}
	ev.MessageID = msg.Metadata.MessageID
	ev, report := e.hype.filter(ev)
	if !report {
		return
	}
	e.onChannelEvent(ev)
}

func (e *EventSub) isDuplicate(id string) bool {
	now := time.Now()
	e.seenMu.Lock()
	defer e.seenMu.Unlock()
	// Pruning walks every id of the window; once per dedupePrune is
	// enough (a busy chat sends thousands of messages per window).
	if now.Sub(e.pruned) >= dedupePrune {
		e.pruned = now
		for k, t := range e.seen {
			if now.Sub(t) > dedupeWindow {
				delete(e.seen, k)
			}
		}
	}
	if _, ok := e.seen[id]; ok {
		return true
	}
	e.seen[id] = now
	return false
}

// isDeprecated: Create EventSub Subscription's 400 "The subscription type
// and version combination is deprecated" (checked 2026-11-19). Other 400s
// (e.g. a session that just ended) are retried as before.
func isDeprecated(err error) bool {
	e, ok := errors.AsType[*APIError](err)
	return ok && e.Status == http.StatusBadRequest && strings.Contains(strings.ToLower(e.Message), "deprecated")
}

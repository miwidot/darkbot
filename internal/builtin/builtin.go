// Package builtin runs darkbot's small built-in chat commands that act on
// the stream or answer questions about it: !marker (a stream marker for
// editing the VOD later), !snooze (push the next automatic ad back by 5
// minutes), !uptime (how long the stream runs) and !followage (how long
// someone follows). Song requests
// (internal/songs) and !clips (internal/clips) have their own packages.
//
// Twitch calls and their documented errors: internal/twitch/manage.go,
// channel.go (Get Streams), followers.go and users.go.
package builtin

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/miwidot/darkbot/internal/chatbot"
	"github.com/miwidot/darkbot/internal/config"
	"github.com/miwidot/darkbot/internal/i18n"
	"github.com/miwidot/darkbot/internal/twitch"
)

// Command keys (also used by the app to save their settings).
const (
	KeyMarker    = "marker"
	KeySnooze    = "snooze"
	KeyUptime    = "uptime"
	KeyFollowage = "followage"
)

// Keys lists the commands in display order.
var Keys = []string{KeyMarker, KeySnooze, KeyUptime, KeyFollowage}

// callTimeout bounds one Twitch call.
const callTimeout = 15 * time.Second

// API is the slice of Helix the commands use.
type API interface {
	CreateMarker(ctx context.Context, broadcasterID, description string) (twitch.Marker, error)
	SnoozeAd(ctx context.Context, broadcasterID string) (twitch.AdSchedule, error)
	LiveSince(ctx context.Context, broadcasterID string) (live bool, since time.Time, err error)
	FollowedAt(ctx context.Context, broadcasterID, userID string) (at time.Time, following bool, err error)
	UserByLogin(ctx context.Context, login string) (u twitch.User, found bool, err error)
}

// Logger is the logging this package needs.
type Logger interface {
	Infof(format string, args ...any)
	Warnf(format string, args ...any)
}

// Deps wires the handler to the app.
type Deps struct {
	// Settings returns a command's settings by key.
	Settings func(key string) config.BuiltinCommand
	API      API
	// HasScope reports whether the Twitch login has a scope.
	HasScope func(scope string) bool
	Reply    func(ctx context.Context, text, replyTo string)
	Lang     func() string
	// OnUse is called when a command ran (optional, dashboard count).
	OnUse func()
	Log   Logger
	Now   func() time.Time // nil = time.Now
}

// Handler runs the commands. Safe for concurrent use.
type Handler struct {
	d    Deps
	mu   sync.Mutex
	last map[string]time.Time // key -> last use (cooldown)
	busy map[string]bool      // key -> a call is running
}

// New creates a handler.
func New(d Deps) *Handler {
	if d.Now == nil {
		d.Now = time.Now
	}
	if d.OnUse == nil {
		d.OnUse = func() {}
	}
	return &Handler{d: d, last: map[string]time.Time{}, busy: map[string]bool{}}
}

// Match returns the key of the turned-on command text starts with.
func (h *Handler) Match(text string) (string, bool) {
	first, _, _ := strings.Cut(strings.TrimSpace(text), " ")
	name, ok := strings.CutPrefix(strings.ToLower(first), chatbot.Prefix)
	if !ok || name == "" {
		return "", false
	}
	for _, k := range Keys {
		if s := h.d.Settings(k); s.Enabled && s.Name == name {
			return k, true
		}
	}
	return "", false
}

// Handle runs the command with that key for one chat message.
func (h *Handler) Handle(ctx context.Context, key string, m twitch.ChatMessage) {
	s := h.d.Settings(key)
	if !s.Enabled || !chatbot.Allowed(s.Permission, m) || !h.start(key, s, m) {
		return
	}
	defer h.done(key)
	h.d.Log.Infof("%s used %s%s", m.ChatterName, chatbot.Prefix, s.Name)
	h.d.OnUse()
	ctx, cancel := context.WithTimeout(ctx, callTimeout)
	defer cancel()
	var reply string
	switch key {
	case KeyMarker:
		reply = h.marker(ctx, m)
	case KeySnooze:
		reply = h.snooze(ctx, m)
	case KeyUptime:
		reply = h.uptime(ctx, m)
	case KeyFollowage:
		reply = h.followage(ctx, m)
	}
	if reply != "" {
		h.d.Reply(ctx, reply, m.ID)
	}
}

// start takes the command's cooldown (the broadcaster and moderators skip
// it) and its one running call.
func (h *Handler) start(key string, s config.BuiltinCommand, m twitch.ChatMessage) bool {
	now := h.d.Now()
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.busy[key] {
		return false
	}
	privileged := m.IsBroadcaster || m.IsModerator
	if last, ok := h.last[key]; ok && !privileged && now.Sub(last) < time.Duration(s.CooldownSeconds)*time.Second {
		return false
	}
	h.busy[key], h.last[key] = true, now
	return true
}

func (h *Handler) done(key string) {
	h.mu.Lock()
	delete(h.busy, key)
	h.mu.Unlock()
}

func (h *Handler) t(key string, args ...any) string { return i18n.T(h.d.Lang(), key, args...) }

// needScope answers when the login lacks scope (reconnect needed).
func (h *Handler) needScope(scope string) (string, bool) {
	if h.d.HasScope(scope) {
		return "", false
	}
	h.d.Log.Warnf("Reconnect Twitch once (Settings, Twitch) so darkbot may use %s", scope)
	return h.t("chat.cmdUnavailable"), true
}

// marker sets a stream marker; the text after the command (or who set it)
// becomes its description, which only the streamer sees in the VOD tools.
func (h *Handler) marker(ctx context.Context, m twitch.ChatMessage) string {
	if r, missing := h.needScope(twitch.ScopeManageBroadcast); missing {
		return r
	}
	_, arg, _ := strings.Cut(strings.TrimSpace(m.Text), " ")
	desc := strings.Join(strings.Fields(arg), " ")
	if desc == "" {
		desc = h.t("marker.by", "user", m.ChatterName)
	}
	mk, err := h.d.API.CreateMarker(ctx, m.BroadcasterID, desc)
	if err != nil {
		h.d.Log.Warnf("Marker for %s failed: %v", m.ChatterName, err)
		if errors.Is(err, twitch.ErrMarkerLater) {
			return h.t("chat.markerLater")
		}
		return h.t("chat.cmdFailed")
	}
	at := Clock(mk.Position)
	h.d.Log.Infof("Stream marker at %s: %s", at, desc)
	return h.t("chat.markerDone", "time", at)
}

// snooze pushes the next automatic ad back by 5 minutes.
func (h *Handler) snooze(ctx context.Context, m twitch.ChatMessage) string {
	if r, missing := h.needScope(twitch.ScopeManageAds); missing {
		return r
	}
	s, err := h.d.API.SnoozeAd(ctx, m.BroadcasterID)
	switch {
	case errors.Is(err, twitch.ErrNoSnoozes):
		return h.t("chat.snoozeNone")
	case errors.Is(err, twitch.ErrNoAdPlanned):
		return h.t("chat.snoozeNoAd")
	case err != nil:
		h.d.Log.Warnf("Snooze for %s failed: %v", m.ChatterName, err)
		return h.t("chat.cmdFailed")
	}
	h.d.Log.Infof("Next ad snoozed by %s, %d snoozes left", m.ChatterName, s.Snoozes)
	return h.t("chat.snoozeDone", "n", s.Snoozes)
}

// Clock formats a position in the stream as h:mm:ss.
func Clock(d time.Duration) string {
	d = d.Round(time.Second)
	sec := int(d / time.Second)
	return fmt.Sprintf("%d:%02d:%02d", sec/3600, sec/60%60, sec%60)
}

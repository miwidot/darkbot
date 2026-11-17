// Package songs runs the chat song request commands on the streamer's
// Spotify:
//
//	!sr <Spotify link or search>   queue a song (alias !songrequest)
//	!song                          what plays now
//	!next                          what plays next
//	!queue                         the next few songs
//	!skip                          skip the current song (moderators, the
//	                               broadcaster, optionally the requester)
//
// Spotify queues on the device that is currently playing; Add to Queue and
// Skip need Spotify Premium (Web API reference).
package songs

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/miwidot/darkbot/internal/config"
	"github.com/miwidot/darkbot/internal/i18n"
	"github.com/miwidot/darkbot/internal/spotify"
	"github.com/miwidot/darkbot/internal/twitch"
)

const (
	cmdRequest     = "!sr"
	cmdRequestLong = "!songrequest"
	cmdSong        = "!song"
	cmdNext        = "!next"
	cmdQueue       = "!queue"
	cmdSkip        = "!skip"

	// requestTimeout bounds all Spotify calls of one command.
	requestTimeout = 15 * time.Second
	// infoCooldown: !song, !next and !queue answer at most once per this
	// time each, so a chat spamming them can't flood the channel.
	infoCooldown = 5 * time.Second
	// queueShown is how many upcoming songs !queue lists.
	queueShown = 5
	// rememberFor: who requested which song, used for "requested by" and
	// requester skips. Longer than any realistic queue.
	rememberFor = 6 * time.Hour
)

// Player is the part of the Spotify client the commands need.
type Player interface {
	GetTrack(ctx context.Context, id string) (*spotify.Track, error)
	SearchTrack(ctx context.Context, query string) (*spotify.Track, error)
	AddToQueue(ctx context.Context, uri string) error
	Next(ctx context.Context) error
	GetQueue(ctx context.Context) (*spotify.Queue, error)
}

// Logger is the minimal logging interface this package needs.
type Logger interface {
	Infof(format string, args ...any)
	Warnf(format string, args ...any)
}

// Activity kinds for the UI feed.
const (
	ActAdded    = "song_added"
	ActSkipped  = "song_skipped"
	ActRejected = "song_rejected"
)

// Activity is shown in the activity feed.
type Activity struct {
	Kind    string    `json:"kind"`
	User    string    `json:"user"`
	Title   string    `json:"title"`
	Message string    `json:"message,omitempty"`
	At      time.Time `json:"at"`
}

// Deps wires the handler to the rest of the app.
type Deps struct {
	Settings   func() config.SongRequests
	Player     Player
	Connected  func() bool   // Spotify is connected
	Lang       func() string // language for chat replies
	Reply      func(ctx context.Context, text, replyTo string)
	OnActivity func(Activity)
	Log        Logger
	Now        func() time.Time // nil = time.Now
}

type request struct {
	user string
	at   time.Time
}

// Handler answers chat commands. Safe for concurrent use; every message may
// be handled in its own goroutine.
type Handler struct {
	d Deps

	mu        sync.Mutex
	lastSR    map[string]time.Time // chatter ID -> last accepted request
	lastInfo  map[string]time.Time // command -> last answer
	requested map[string]request   // track URI -> who requested it
}

// New creates a handler.
func New(d Deps) *Handler {
	if d.Now == nil {
		d.Now = time.Now
	}
	if d.OnActivity == nil {
		d.OnActivity = func(Activity) {}
	}
	return &Handler{d: d, lastSR: map[string]time.Time{}, lastInfo: map[string]time.Time{}, requested: map[string]request{}}
}

// IsCommand reports whether text starts with one of our commands, so the
// caller only spawns work for relevant messages.
func IsCommand(text string) bool {
	cmd, _ := split(text)
	switch cmd {
	case cmdRequest, cmdRequestLong, cmdSong, cmdNext, cmdQueue, cmdSkip:
		return true
	}
	return false
}

func split(text string) (cmd, arg string) {
	text = strings.TrimSpace(text)
	cmd, arg, _ = strings.Cut(text, " ")
	return strings.ToLower(cmd), strings.TrimSpace(arg)
}

// Handle runs one chat message.
func (h *Handler) Handle(ctx context.Context, m twitch.ChatMessage) {
	s := h.d.Settings()
	if !s.Enabled {
		return
	}
	cmd, arg := split(m.Text)
	ctx, cancel := context.WithTimeout(ctx, requestTimeout)
	defer cancel()
	switch cmd {
	case cmdRequest, cmdRequestLong:
		h.request(ctx, s, m, arg)
	case cmdSkip:
		h.skip(ctx, s, m)
	case cmdSong, cmdNext, cmdQueue:
		if h.infoAllowed(cmd) {
			h.info(ctx, s, m, cmd)
		}
	}
}

func (h *Handler) t(key string, args ...any) string { return i18n.T(h.d.Lang(), key, args...) }

func (h *Handler) reply(ctx context.Context, s config.SongRequests, m twitch.ChatMessage, text string) {
	if s.ChatReplies {
		h.d.Reply(ctx, text, m.ID)
	}
}

// errText words an error for chat: our own errors translated, anything
// unexpected (network etc.) as a short generic sentence; details go to the log.
func (h *Handler) errText(err error) string {
	if _, ok := errors.AsType[*i18n.Error](err); ok {
		return i18n.Localize(err, h.d.Lang())
	}
	return h.t("chat.error")
}

func (h *Handler) reject(ctx context.Context, s config.SongRequests, m twitch.ChatMessage, title, why string) {
	h.reply(ctx, s, m, why)
	h.d.OnActivity(Activity{Kind: ActRejected, User: m.ChatterName, Title: title, Message: why, At: h.d.Now()})
}

func privileged(m twitch.ChatMessage) bool { return m.IsBroadcaster || m.IsModerator }

func isYouTube(arg string) bool {
	l := strings.ToLower(arg)
	return strings.Contains(l, "youtube.com/") || strings.Contains(l, "youtu.be/")
}

func (h *Handler) request(ctx context.Context, s config.SongRequests, m twitch.ChatMessage, arg string) {
	if arg == "" {
		h.reply(ctx, s, m, h.t("chat.usage"))
		return
	}
	if !h.d.Connected() {
		h.reply(ctx, s, m, h.t("chat.offline"))
		return
	}
	if isYouTube(arg) {
		h.reject(ctx, s, m, arg, h.t("chat.onlySpotify"))
		return
	}
	// Reserve the cooldown before the slow Spotify calls, so two quick
	// requests from the same viewer can't both pass.
	if wait := h.reserve(s, m); wait > 0 {
		h.reply(ctx, s, m, h.t("chat.cooldown", "seconds", int(wait.Round(time.Second).Seconds())))
		return
	}
	ok := false
	defer func() {
		if !ok {
			h.release(m)
		}
	}()

	track, err := h.find(ctx, arg)
	if err != nil {
		h.d.Log.Warnf("Song request %q by %s: %v", arg, m.ChatterLogin, err)
		h.reject(ctx, s, m, arg, h.errText(err))
		return
	}
	title := track.Title()
	if why := h.check(s, track); why != "" {
		h.reject(ctx, s, m, title, why)
		return
	}
	if err := h.d.Player.AddToQueue(ctx, track.URI); err != nil {
		h.d.Log.Warnf("Queue %q for %s: %v", title, m.ChatterLogin, err)
		h.reject(ctx, s, m, title, h.errText(err))
		return
	}
	ok = true
	h.remember(track.URI, m.ChatterName)
	h.d.Log.Infof("%s requested %q", m.ChatterName, title)
	h.d.OnActivity(Activity{Kind: ActAdded, User: m.ChatterName, Title: title, At: h.d.Now()})
	h.reply(ctx, s, m, h.t("chat.added", "title", title))
}

func (h *Handler) find(ctx context.Context, arg string) (*spotify.Track, error) {
	id, isLink, err := spotify.ParseLink(arg)
	switch {
	case err != nil:
		return nil, err
	case isLink:
		return h.d.Player.GetTrack(ctx, id)
	default:
		return h.d.Player.SearchTrack(ctx, arg)
	}
}

// check applies the streamer's rules; it returns the reason to refuse or "".
func (h *Handler) check(s config.SongRequests, t *spotify.Track) string {
	if t.IsPlayable != nil && !*t.IsPlayable {
		return h.t("chat.unavailable")
	}
	if s.BlockExplicit && t.Explicit {
		return h.t("chat.explicit")
	}
	if s.MaxMinutes > 0 && t.Duration() > time.Duration(s.MaxMinutes)*time.Minute {
		return h.t("chat.tooLong", "minutes", s.MaxMinutes)
	}
	return ""
}

func (h *Handler) reserve(s config.SongRequests, m twitch.ChatMessage) time.Duration {
	if privileged(m) || s.UserCooldownSeconds <= 0 {
		return 0
	}
	now := h.d.Now()
	h.mu.Lock()
	defer h.mu.Unlock()
	if last, ok := h.lastSR[m.ChatterID]; ok {
		if wait := last.Add(time.Duration(s.UserCooldownSeconds) * time.Second).Sub(now); wait > 0 {
			return wait
		}
	}
	h.lastSR[m.ChatterID] = now
	return 0
}

// release undoes a reservation when the request failed, so a typo doesn't
// cost the viewer their cooldown.
func (h *Handler) release(m twitch.ChatMessage) {
	h.mu.Lock()
	delete(h.lastSR, m.ChatterID)
	h.mu.Unlock()
}

func (h *Handler) remember(uri, user string) {
	now := h.d.Now()
	h.mu.Lock()
	defer h.mu.Unlock()
	for k, r := range h.requested {
		if now.Sub(r.at) > rememberFor {
			delete(h.requested, k)
		}
	}
	h.requested[uri] = request{user: user, at: now}
}

func (h *Handler) requester(uri string) string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.requested[uri].user
}

func (h *Handler) infoAllowed(cmd string) bool {
	now := h.d.Now()
	h.mu.Lock()
	defer h.mu.Unlock()
	if last, ok := h.lastInfo[cmd]; ok && now.Sub(last) < infoCooldown {
		return false
	}
	h.lastInfo[cmd] = now
	return true
}

// label is "Title (requested by user)" when a viewer asked for it.
func (h *Handler) label(t spotify.Track) string {
	if who := h.requester(t.URI); who != "" {
		return h.t("chat.byUser", "title", t.Title(), "user", who)
	}
	return t.Title()
}

func (h *Handler) info(ctx context.Context, s config.SongRequests, m twitch.ChatMessage, cmd string) {
	if !h.d.Connected() {
		h.reply(ctx, s, m, h.t("chat.offline"))
		return
	}
	q, err := h.d.Player.GetQueue(ctx)
	if err != nil {
		h.d.Log.Warnf("Read Spotify queue: %v", err)
		h.reply(ctx, s, m, h.errText(err))
		return
	}
	switch cmd {
	case cmdSong:
		if q.Current == nil {
			h.reply(ctx, s, m, h.t("chat.nothingPlaying"))
			return
		}
		h.reply(ctx, s, m, h.t("chat.nowPlaying", "title", h.label(*q.Current)))
	case cmdNext:
		if len(q.Next) == 0 {
			h.reply(ctx, s, m, h.t("chat.queueEmpty"))
			return
		}
		h.reply(ctx, s, m, h.t("chat.upNext", "title", h.label(q.Next[0])))
	case cmdQueue:
		if len(q.Next) == 0 {
			h.reply(ctx, s, m, h.t("chat.queueEmpty"))
			return
		}
		parts := make([]string, 0, queueShown)
		for i, t := range q.Next[:min(queueShown, len(q.Next))] {
			parts = append(parts, fmt.Sprintf("%d. %s", i+1, h.label(t)))
		}
		h.reply(ctx, s, m, h.t("chat.queue", "list", strings.Join(parts, " | ")))
	}
}

func (h *Handler) skip(ctx context.Context, s config.SongRequests, m twitch.ChatMessage) {
	if !h.d.Connected() {
		h.reply(ctx, s, m, h.t("chat.offline"))
		return
	}
	q, err := h.d.Player.GetQueue(ctx)
	if err != nil {
		h.d.Log.Warnf("Read Spotify queue: %v", err)
		h.reply(ctx, s, m, h.errText(err))
		return
	}
	if q.Current == nil {
		h.reply(ctx, s, m, h.t("chat.nothingPlaying"))
		return
	}
	own := s.RequesterCanSkip && h.requester(q.Current.URI) == m.ChatterName
	if !privileged(m) && !own {
		h.reply(ctx, s, m, h.t("chat.skipDenied"))
		return
	}
	if err := h.d.Player.Next(ctx); err != nil {
		h.d.Log.Warnf("Skip on Spotify: %v", err)
		h.reply(ctx, s, m, h.errText(err))
		return
	}
	title := q.Current.Title()
	h.d.Log.Infof("%s skipped %q", m.ChatterName, title)
	h.d.OnActivity(Activity{Kind: ActSkipped, User: m.ChatterName, Title: title, At: h.d.Now()})
	h.reply(ctx, s, m, h.t("chat.skipped", "title", title))
}

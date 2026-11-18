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
	"maps"
	"strings"
	"sync"
	"time"

	"github.com/miwidot/darkbot/internal/config"
	"github.com/miwidot/darkbot/internal/i18n"
	"github.com/miwidot/darkbot/internal/ratelimit"
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
	// failCooldown: after a refused request (typo, not found, too long) a
	// viewer may try again this soon, not after the full cooldown, but not
	// instantly either, so nobody can hammer Spotify's search.
	failCooldown = 10 * time.Second
	// maxChannelRequests per channelRequestWindow for the whole channel
	// (mods and the broadcaster excepted): far more than a real chat sends,
	// few enough that a bot wave can't flood the Spotify queue.
	maxChannelRequests   = 10
	channelRequestWindow = time.Minute
	// commandGap: a viewer (not mod/broadcaster) can trigger one command per
	// this time; anything faster is ignored without a reply.
	commandGap = 3 * time.Second
	// maxReplies per replyWindow caps darkbot's chat answers well below
	// Twitch's 100 messages per 30 s for the broadcaster
	// (https://dev.twitch.tv/docs/chat/), so spam can't use up the budget.
	maxReplies  = 20
	replyWindow = 30 * time.Second
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
	OnCommand  func() // a command passed the checks and is being handled (stats)
	Log        Logger
	Now        func() time.Time // nil = time.Now
}

type request struct {
	userID string
	user   string
	at     time.Time
}

// Handler answers chat commands. Safe for concurrent use; every message may
// be handled in its own goroutine.
type Handler struct {
	d Deps

	mu        sync.Mutex
	lastSR    map[string]time.Time // chatter ID -> when the request cooldown started
	channel   *ratelimit.Window    // requests of the whole channel
	lastCmd   map[string]time.Time // chatter ID -> last command (spam gate)
	lastInfo  map[string]time.Time // command -> last answer
	requested map[string]request   // track URI -> who requested it
	replies   []time.Time          // recent chat answers (rate cap)
}

// New creates a handler.
func New(d Deps) *Handler {
	if d.Now == nil {
		d.Now = time.Now
	}
	if d.OnActivity == nil {
		d.OnActivity = func(Activity) {}
	}
	if d.OnCommand == nil {
		d.OnCommand = func() {}
	}
	return &Handler{d: d, channel: ratelimit.New(maxChannelRequests, channelRequestWindow), lastSR: map[string]time.Time{}, lastCmd: map[string]time.Time{}, lastInfo: map[string]time.Time{}, requested: map[string]request{}}
}

// IsCommand reports whether text starts with one of our commands, so the
// caller only spawns work for relevant messages.
// CommandNames are the song commands without "!", so the streamer's own
// chat commands can't take them.
func CommandNames() []string {
	var out []string
	for _, c := range []string{cmdRequest, cmdRequestLong, cmdSong, cmdNext, cmdQueue, cmdSkip} {
		out = append(out, strings.TrimPrefix(c, "!"))
	}
	return out
}

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
	if !h.commandAllowed(m) {
		return
	}
	h.d.OnCommand()
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
	if s.ChatReplies && h.replyAllowed() {
		h.d.Reply(ctx, text, m.ID)
	}
}

// commandAllowed is the per-viewer spam gate.
func (h *Handler) commandAllowed(m twitch.ChatMessage) bool {
	if privileged(m) {
		return true
	}
	now := h.d.Now()
	h.mu.Lock()
	defer h.mu.Unlock()
	if last, ok := h.lastCmd[m.ChatterID]; ok && now.Sub(last) < commandGap {
		return false
	}
	h.lastCmd[m.ChatterID] = now
	for id, t := range h.lastCmd {
		if now.Sub(t) > commandGap {
			delete(h.lastCmd, id)
		}
	}
	return true
}

// replyAllowed caps chat answers per replyWindow (sliding window).
func (h *Handler) replyAllowed() bool {
	now := h.d.Now()
	h.mu.Lock()
	defer h.mu.Unlock()
	keep := h.replies[:0]
	for _, t := range h.replies {
		if now.Sub(t) < replyWindow {
			keep = append(keep, t)
		}
	}
	h.replies = keep
	if len(h.replies) >= maxReplies {
		h.d.Log.Warnf("Song requests: chat answer skipped, %d answers in the last %s", maxReplies, replyWindow)
		return false
	}
	h.replies = append(h.replies, now)
	return true
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
	// Per-viewer cooldowns don't stop many accounts at once (bots):
	// the whole channel gets maxChannelRequests per channelRequestWindow.
	if ok, first := h.channel.Allow(h.d.Now()); !ok && !privileged(m) {
		if first {
			h.d.Log.Warnf("Song requests paused: %d in the last %s", maxChannelRequests, channelRequestWindow)
		}
		h.reply(ctx, s, m, h.t("chat.busy"))
		return
	}
	ok := false
	defer func() {
		if !ok {
			h.shorten(s, m)
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
	// Refuse songs that already play or wait: no duplicates, and nobody can
	// "claim" the streamer's song to get the right to skip it.
	q, err := h.d.Player.GetQueue(ctx)
	if err != nil {
		h.d.Log.Warnf("Read Spotify queue for %q: %v", title, err)
		h.reject(ctx, s, m, title, h.errText(err))
		return
	}
	if inQueue(q, track.URI) {
		h.reject(ctx, s, m, title, h.t("chat.alreadyQueued"))
		return
	}
	if err := h.d.Player.AddToQueue(ctx, track.URI); err != nil {
		h.d.Log.Warnf("Queue %q for %s: %v", title, m.ChatterLogin, err)
		h.reject(ctx, s, m, title, h.errText(err))
		return
	}
	ok = true
	h.remember(track.URI, m)
	h.d.Log.Infof("%s requested %q", m.ChatterName, title)
	h.d.OnActivity(Activity{Kind: ActAdded, User: m.ChatterName, Title: title, At: h.d.Now()})
	if s.HideTitles {
		h.reply(ctx, s, m, h.t("chat.addedNoTitle"))
	} else {
		h.reply(ctx, s, m, h.t("chat.added", "title", title))
	}
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
	full := time.Duration(s.UserCooldownSeconds) * time.Second
	h.mu.Lock()
	defer h.mu.Unlock()
	// Forget viewers whose cooldown is over, so the map doesn't grow with
	// every viewer who ever requested a song.
	maps.DeleteFunc(h.lastSR, func(_ string, t time.Time) bool { return now.Sub(t) >= full })
	if last, ok := h.lastSR[m.ChatterID]; ok {
		if wait := last.Add(time.Duration(s.UserCooldownSeconds) * time.Second).Sub(now); wait > 0 {
			return wait
		}
	}
	h.lastSR[m.ChatterID] = now
	return 0
}

// shorten turns the full cooldown into failCooldown after a refused
// request, so a typo doesn't cost the viewer the whole wait.
func (h *Handler) shorten(s config.SongRequests, m twitch.ChatMessage) {
	full := time.Duration(s.UserCooldownSeconds) * time.Second
	if privileged(m) || full <= failCooldown {
		return
	}
	h.mu.Lock()
	h.lastSR[m.ChatterID] = h.d.Now().Add(failCooldown - full)
	h.mu.Unlock()
}

func inQueue(q *spotify.Queue, uri string) bool {
	if q.Current != nil && q.Current.URI == uri {
		return true
	}
	for _, t := range q.Next {
		if t.URI == uri {
			return true
		}
	}
	return false
}

// remember records the requester. An existing record is never replaced, so
// requesting a song again can't take over someone else's request.
func (h *Handler) remember(uri string, m twitch.ChatMessage) {
	now := h.d.Now()
	h.mu.Lock()
	defer h.mu.Unlock()
	for k, r := range h.requested {
		if now.Sub(r.at) > rememberFor {
			delete(h.requested, k)
		}
	}
	if _, taken := h.requested[uri]; !taken {
		h.requested[uri] = request{userID: m.ChatterID, user: m.ChatterName, at: now}
	}
}

func (h *Handler) requester(uri string) request {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.requested[uri]
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
	if r := h.requester(t.URI); r.user != "" {
		return h.t("chat.byUser", "title", t.Title(), "user", r.user)
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
	if s.HideTitles {
		// These commands only exist to name titles.
		h.reply(ctx, s, m, h.t("chat.titlesHidden"))
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
	// Compare user IDs: display names can change and aren't unique.
	own := s.RequesterCanSkip && m.ChatterID != "" && h.requester(q.Current.URI).userID == m.ChatterID
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
	if s.HideTitles {
		h.reply(ctx, s, m, h.t("chat.skippedNoTitle"))
	} else {
		h.reply(ctx, s, m, h.t("chat.skipped", "title", title))
	}
}

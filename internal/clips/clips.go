// Package clips runs the built-in "!clips" chat command: a viewer makes a
// clip of the last ClipSeconds of the live stream, darkbot waits until
// Twitch has it and posts its link in chat.
//
// Twitch (https://dev.twitch.tv/docs/api/reference/clips, checked
// 2026-11-19): Create Clip needs clips:edit and a live stream; creating is
// asynchronous and a clip Get Clips hasn't listed after 60 seconds counts
// as failed. See internal/twitch/clips.go for the error cases.
package clips

import (
	"context"
	"errors"
	"net/url"
	"regexp"
	"slices"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/miwidot/darkbot/internal/chatbot"
	"github.com/miwidot/darkbot/internal/config"
	"github.com/miwidot/darkbot/internal/i18n"
	"github.com/miwidot/darkbot/internal/twitch"
)

// ClipSeconds: every clip is the last minute of the stream, the longest
// clip Twitch makes.
const ClipSeconds = twitch.MaxClipSeconds

// Variables only so tests can run fast.
var (
	// confirmTimeout: Twitch's documented time after which a clip that
	// Get Clips doesn't list has failed.
	confirmTimeout = 60 * time.Second
	// pollInterval: how often Get Clips is asked meanwhile (at most 20
	// calls per clip).
	pollInterval = 3 * time.Second
)

const (
	// createTimeout bounds the Create Clip call itself.
	createTimeout = 15 * time.Second
	// ActClip is the activity feed kind of a new clip.
	ActClip = "clip_made"
	// clipHost is where clip links point (Get Clips' "url").
	clipHost = "clips.twitch.tv"
)

// idPattern: what a clip ID looks like (the docs' example
// "FiveWordsForClipSlug"; real ones also have "-" and "_"). Anything else
// is not put into a link.
var idPattern = regexp.MustCompile(`^[A-Za-z0-9_-]{1,100}$`)

// API is the slice of Helix this package uses.
type API interface {
	CreateClip(ctx context.Context, broadcasterID, title string, seconds int) (twitch.Clip, error)
	GetClip(ctx context.Context, id string) (twitch.Clip, bool, error)
}

// Logger is the logging this package needs.
type Logger interface {
	Infof(format string, args ...any)
	Warnf(format string, args ...any)
}

// Activity is a new clip in the activity feed.
type Activity struct {
	Kind  string    `json:"kind"`
	User  string    `json:"user"`
	Title string    `json:"title"`
	URL   string    `json:"url"`
	At    time.Time `json:"at"`
}

// Deps wires the handler to the app.
type Deps struct {
	Settings func() config.Clips
	API      API
	// CanClip reports whether the Twitch login allows clips (clips:edit).
	CanClip func() bool
	// Reply answers in chat (as a reply to replyTo).
	Reply      func(ctx context.Context, text, replyTo string)
	Lang       func() string
	OnActivity func(Activity) // optional
	Log        Logger
	Now        func() time.Time // nil = time.Now
}

// Handler runs the clip command. Safe for concurrent use.
type Handler struct {
	d Deps

	mu   sync.Mutex
	busy bool      // a clip is being made
	last time.Time // when the last clip was started (cooldown)
}

// New creates a handler.
func New(d Deps) *Handler {
	if d.Now == nil {
		d.Now = time.Now
	}
	if d.OnActivity == nil {
		d.OnActivity = func(Activity) {}
	}
	return &Handler{d: d}
}

// IsCommand reports whether text is the clip command (turned on or not),
// so the caller only starts work for it.
func IsCommand(s config.Clips, text string) bool {
	first, _, _ := strings.Cut(strings.TrimSpace(text), " ")
	name, ok := strings.CutPrefix(strings.ToLower(first), chatbot.Prefix)
	return ok && name != "" && name == s.Name
}

// Handle runs one chat message that IsCommand matched.
func (h *Handler) Handle(ctx context.Context, m twitch.ChatMessage) {
	s := h.d.Settings()
	if !s.Enabled || !IsCommand(s, m.Text) || !chatbot.Allowed(s.Permission, m) {
		return
	}
	if !h.start(s, m) {
		return
	}
	defer h.done()
	// The chat subscription's context ends when EventSub reconnects; a clip
	// already started is still confirmed and announced (every step below
	// has its own time limit).
	ctx = context.WithoutCancel(ctx)
	h.d.Log.Infof("%s used %s%s", m.ChatterName, chatbot.Prefix, s.Name)
	if !h.d.CanClip() {
		h.d.Log.Warnf("Clips: reconnect Twitch once (Settings, Twitch) to allow darkbot to make clips")
		h.reply(ctx, m, h.t("chat.clipUnavailable"))
		return
	}
	clip, err := h.make(ctx, m, s)
	if err != nil {
		h.d.Log.Warnf("Clip for %s failed: %v", m.ChatterName, err)
		h.reply(ctx, m, h.errText(err))
		return
	}
	h.d.Log.Infof("Clip by %s: %s", m.ChatterName, clip.URL)
	h.reply(ctx, m, h.t("chat.clipDone", "user", m.ChatterName, "url", clip.URL))
	h.d.OnActivity(Activity{Kind: ActClip, User: m.ChatterName, Title: clip.Title, URL: clip.URL, At: h.d.Now()})
}

// start takes the one clip slot and the cooldown. Only one clip is made at
// a time (a whole chat typing !clips gets one clip); the broadcaster and
// moderators skip the cooldown.
func (h *Handler) start(s config.Clips, m twitch.ChatMessage) bool {
	now := h.d.Now()
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.busy {
		return false
	}
	privileged := m.IsBroadcaster || m.IsModerator
	if !privileged && !h.last.IsZero() && now.Sub(h.last) < time.Duration(s.CooldownSeconds)*time.Second {
		return false
	}
	h.busy, h.last = true, now
	return true
}

func (h *Handler) done() {
	h.mu.Lock()
	h.busy = false
	h.mu.Unlock()
}

// make creates the clip and waits until Twitch lists it.
func (h *Handler) make(ctx context.Context, m twitch.ChatMessage, s config.Clips) (twitch.Clip, error) {
	title := Title(s.Title, m.ChatterName)
	clip, err := h.create(ctx, m.BroadcasterID, title)
	if errors.Is(err, twitch.ErrClipNotClippable) && title != "" {
		// A 400 is a category that can't be clipped or a title AutoMod
		// refused; without a title only the first is left.
		h.d.Log.Infof("Clip refused with the title %q, trying without it", title)
		clip, err = h.create(ctx, m.BroadcasterID, "")
	}
	if err != nil {
		return twitch.Clip{}, err
	}
	if !idPattern.MatchString(clip.ID) {
		return twitch.Clip{}, errors.New("unexpected clip ID from Twitch")
	}
	return h.confirm(ctx, clip.ID)
}

func (h *Handler) create(ctx context.Context, broadcasterID, title string) (twitch.Clip, error) {
	ctx, cancel := context.WithTimeout(ctx, createTimeout)
	defer cancel()
	return h.d.API.CreateClip(ctx, broadcasterID, title, ClipSeconds)
}

// errClipMissing: Twitch never listed the clip.
var errClipMissing = errors.New("Twitch did not list the clip within a minute")

// confirm asks Get Clips until the clip is there or confirmTimeout is over.
// A failing Get Clips call is retried; only the last error is reported.
func (h *Handler) confirm(ctx context.Context, id string) (twitch.Clip, error) {
	ctx, cancel := context.WithTimeout(ctx, confirmTimeout)
	defer cancel()
	tick := time.NewTicker(pollInterval)
	defer tick.Stop()
	var lastErr error
	for {
		select {
		case <-ctx.Done():
			if lastErr != nil {
				return twitch.Clip{}, errors.Join(errClipMissing, lastErr)
			}
			return twitch.Clip{}, errClipMissing
		case <-tick.C:
		}
		c, ok, err := h.d.API.GetClip(ctx, id)
		if err != nil {
			lastErr = err
			continue
		}
		if ok {
			c.URL = Link(c.URL, id)
			return c, nil
		}
	}
}

// Link is the clip's address: Twitch's own when it is a twitch.tv https
// link, else built from the ID.
func Link(raw, id string) string {
	if u, err := url.Parse(raw); err == nil && u.Scheme == "https" &&
		(u.Hostname() == "twitch.tv" || strings.HasSuffix(u.Hostname(), ".twitch.tv")) {
		return u.String()
	}
	return (&url.URL{Scheme: "https", Host: clipHost, Path: "/" + id}).String()
}

// Title fills {user} in the title template and cuts it to MaxClipTitle.
func Title(tmpl, user string) string {
	t := strings.Join(strings.Fields(strings.ReplaceAll(tmpl, "{user}", user)), " ")
	if utf8.RuneCountInString(t) > config.MaxClipTitle {
		t = string([]rune(t)[:config.MaxClipTitle])
	}
	return t
}

func (h *Handler) t(key string, args ...any) string { return i18n.T(h.d.Lang(), key, args...) }

func (h *Handler) reply(ctx context.Context, m twitch.ChatMessage, text string) {
	h.d.Reply(ctx, text, m.ID)
}

// errText words a failure for chat; details are in the log.
func (h *Handler) errText(err error) string {
	switch {
	case errors.Is(err, twitch.ErrClipOffline):
		return h.t("chat.clipOffline")
	case errors.Is(err, twitch.ErrClipNotClippable):
		return h.t("chat.clipNotClippable")
	case errors.Is(err, twitch.ErrClipNotAllowed):
		return h.t("chat.clipNotAllowed")
	}
	return h.t("chat.clipFailed")
}

// Normalize cleans up and checks the clip settings. The name may not be a
// song command (reserved) or one of the streamer's "!" commands.
func Normalize(c config.Clips, cmds []config.ChatCommand, reserved []string) (config.Clips, error) {
	name, ok := chatbot.CleanName(c.Name)
	c.Name = name
	c.Title = strings.Join(strings.Fields(c.Title), " ")
	shown := chatbot.Prefix + name
	switch {
	case !ok:
		return c, i18n.E("err.cmdName", "name", name)
	case slices.Contains(reserved, name):
		return c, i18n.E("err.cmdReserved", "name", shown)
	case utf8.RuneCountInString(c.Title) > config.MaxClipTitle:
		return c, i18n.E("err.clipTitle", "max", config.MaxClipTitle)
	case !slices.Contains(config.Permissions, c.Permission):
		return c, i18n.E("err.cmdPermission")
	case c.CooldownSeconds < 0 || c.CooldownSeconds > config.MaxCommandCooldown:
		return c, i18n.E("err.cmdCooldown", "max", config.MaxCommandCooldown)
	}
	if _, taken := chatbot.Find(cmds, config.TriggerCommand, name); taken {
		return c, i18n.E("err.cmdTaken", "name", shown)
	}
	return c, nil
}

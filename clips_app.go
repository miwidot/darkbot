package main

import (
	"context"
	"slices"

	"github.com/miwidot/darkbot/internal/builtin"
	"github.com/miwidot/darkbot/internal/chatbot"
	"github.com/miwidot/darkbot/internal/clips"
	"github.com/miwidot/darkbot/internal/config"
	"github.com/miwidot/darkbot/internal/i18n"
	"github.com/miwidot/darkbot/internal/songs"
	"github.com/miwidot/darkbot/internal/twitch"
)

// The built-in commands: !clips (internal/clips), !marker and !snooze
// (internal/builtin).

// newClips wires the clip command to the app. Called in NewApp.
func (a *App) newClips() *clips.Handler {
	return clips.New(clips.Deps{
		Settings: func() config.Clips { return a.cfg.Get().Clips },
		API:      a.hx,
		CanClip:  a.canClip,
		Reply:    a.sendChat,
		Lang:     a.lang,
		OnActivity: func(act clips.Activity) {
			a.st.Command()
			a.keepActivity(act)
		},
		Log: a.log,
	})
}

// newBuiltins wires !marker, !snooze, !uptime and !followage to the app. Called in NewApp.
func (a *App) newBuiltins() *builtin.Handler {
	return builtin.New(builtin.Deps{
		Settings: a.builtinSettings,
		API:      a.hx,
		HasScope: func(scope string) bool {
			t := a.tok.Current()
			return t != nil && t.HasScope(scope)
		},
		Reply: a.sendChat,
		Lang:  a.lang,
		OnUse: func() { a.st.Command() },
		Log:   a.log,
	})
}

// builtinSettings returns a built-in command's settings by key.
func (a *App) builtinSettings(key string) config.BuiltinCommand {
	c := a.cfg.Get()
	if p := builtinOf(&c, key); p != nil {
		return *p
	}
	return config.BuiltinCommand{}
}

// builtinOf points at the settings of the built-in command with that key
// (builtin.Keys); nil for an unknown key.
func builtinOf(c *config.Config, key string) *config.BuiltinCommand {
	switch key {
	case builtin.KeyMarker:
		return &c.Marker
	case builtin.KeySnooze:
		return &c.Snooze
	case builtin.KeyUptime:
		return &c.Uptime
	case builtin.KeyFollowage:
		return &c.Followage
	}
	return nil
}

// canClip: the Twitch login may make clips.
func (a *App) canClip() bool {
	t := a.tok.Current()
	return t != nil && t.HasScope(twitch.ScopeEditClips)
}

// isClipCommand: the message is for the clip command (and it is on), so
// the chat bot leaves it alone.
func (a *App) isClipCommand(m twitch.ChatMessage) bool {
	s := a.cfg.Get().Clips
	return s.Enabled && clips.IsCommand(s, m.Text)
}

// handleChat routes one chat message; EventSub delivers on its read loop,
// so Twitch, Spotify and chat calls must not block it.
func (a *App) handleChat(ctx context.Context, m twitch.ChatMessage) {
	a.countChat(m)
	switch {
	case songs.IsCommand(m.Text):
		go a.sr.Handle(ctx, m)
	case a.isClipCommand(m):
		go a.clip.Handle(ctx, m)
	default:
		if key, ok := a.builtins.Match(m.Text); ok {
			go a.builtins.Handle(ctx, key, m)
		}
	}
	// Every message counts for the timers; song and built-in commands are
	// reserved names, so the chat bot never answers them.
	go a.cb.Handle(ctx, m)
}

// SaveClips stores the clip command's settings.
func (a *App) SaveClips(s config.Clips) error {
	var refused error
	err := a.cfg.Update(func(c *config.Config) {
		norm, err := clips.Normalize(s, c.Chat.Commands, reservedFor(c, clipsKey))
		if refused = err; err == nil {
			c.Clips = norm
		}
	})
	if refused != nil {
		return a.uiErr(refused)
	}
	a.emitState()
	return a.uiErr(err)
}

// SaveBuiltin stores the settings of a built-in command (by key, see
// builtin.Keys).
func (a *App) SaveBuiltin(key string, s config.BuiltinCommand) error {
	if !slices.Contains(builtin.Keys, key) {
		return a.uiErr(i18n.E("err.cmdUnknown", "name", key))
	}
	var refused error
	err := a.cfg.Update(func(c *config.Config) {
		norm, err := chatbot.NormalizeBuiltin(s, c.Chat.Commands, reservedFor(c, key))
		if refused = err; err != nil {
			return
		}
		*builtinOf(c, key) = norm
	})
	if refused != nil {
		return a.uiErr(refused)
	}
	a.emitState()
	return a.uiErr(err)
}

// clipsKey names the clip command among the built-in ones.
const clipsKey = "clips"

// reservedFor: the names a built-in command (clipsKey or a builtin.Keys
// key) can't take: the song commands and the other built-in ones.
func reservedFor(c *config.Config, key string) []string {
	out := songs.CommandNames()
	if key != clipsKey {
		out = append(out, c.Clips.Name)
	}
	for _, k := range builtin.Keys {
		if k != key {
			out = append(out, builtinOf(c, k).Name)
		}
	}
	return out
}

// reservedCommands: names the streamer's own "!" commands can't take: the
// song commands, the clip command and the built-in commands that are on.
// One that is off keeps its name free, so a streamer's own "!uptime" from
// before darkbot had one stays editable; turning the built-in one on then
// reports the name as taken (chatbot.NormalizeBuiltin).
func (a *App) reservedCommands() []string {
	c := a.cfg.Get()
	out := append(songs.CommandNames(), c.Clips.Name)
	for _, k := range builtin.Keys {
		if b := builtinOf(&c, k); b.Enabled {
			out = append(out, b.Name)
		}
	}
	return out
}

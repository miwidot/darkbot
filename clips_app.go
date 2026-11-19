package main

import (
	"context"

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

// newBuiltins wires !marker and !snooze to the app. Called in NewApp.
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
	switch key {
	case builtin.KeyMarker:
		return c.Marker
	case builtin.KeySnooze:
		return c.Snooze
	}
	return config.BuiltinCommand{}
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
		norm, err := clips.Normalize(s, c.Chat.Commands, reservedFor(c, "clips"))
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

// SaveBuiltin stores the settings of !marker or !snooze (by key).
func (a *App) SaveBuiltin(key string, s config.BuiltinCommand) error {
	if key != builtin.KeyMarker && key != builtin.KeySnooze {
		return a.uiErr(i18n.E("err.cmdUnknown", "name", key))
	}
	var refused error
	err := a.cfg.Update(func(c *config.Config) {
		norm, err := chatbot.NormalizeBuiltin(s, c.Chat.Commands, reservedFor(c, key))
		if refused = err; err != nil {
			return
		}
		if key == builtin.KeyMarker {
			c.Marker = norm
		} else {
			c.Snooze = norm
		}
	})
	if refused != nil {
		return a.uiErr(refused)
	}
	a.emitState()
	return a.uiErr(err)
}

// reservedFor: the names a built-in command (key "clips", "marker" or
// "snooze") can't take: the song commands and the other built-in ones.
func reservedFor(c *config.Config, key string) []string {
	out := songs.CommandNames()
	for k, name := range map[string]string{"clips": c.Clips.Name, builtin.KeyMarker: c.Marker.Name, builtin.KeySnooze: c.Snooze.Name} {
		if k != key {
			out = append(out, name)
		}
	}
	return out
}

// reservedCommands: names the streamer's own "!" commands can't take.
func (a *App) reservedCommands() []string {
	c := a.cfg.Get()
	return append(songs.CommandNames(), c.Clips.Name, c.Marker.Name, c.Snooze.Name)
}

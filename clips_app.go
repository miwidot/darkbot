package main

import (
	"context"

	"github.com/miwidot/darkbot/internal/clips"
	"github.com/miwidot/darkbot/internal/config"
	"github.com/miwidot/darkbot/internal/songs"
	"github.com/miwidot/darkbot/internal/twitch"
)

// The built-in clip command (internal/clips).

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
	}
	// Every message counts for the timers; song and clip commands are
	// reserved names, so the chat bot never answers them.
	go a.cb.Handle(ctx, m)
}

// SaveClips stores the clip command's settings.
func (a *App) SaveClips(s config.Clips) error {
	var refused error
	err := a.cfg.Update(func(c *config.Config) {
		norm, err := clips.Normalize(s, c.Chat.Commands, songs.CommandNames())
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

// reservedCommands: names the streamer's own "!" commands can't take.
func (a *App) reservedCommands() []string {
	return append(songs.CommandNames(), a.cfg.Get().Clips.Name)
}

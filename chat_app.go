package main

import (
	"context"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/miwidot/darkbot/internal/audio"
	"github.com/miwidot/darkbot/internal/chatbot"
	"github.com/miwidot/darkbot/internal/config"
	"github.com/miwidot/darkbot/internal/i18n"
	"github.com/miwidot/darkbot/internal/songs"
)

// Chat commands and timers (internal/chatbot), and whether the stream is
// live: timers post only then, and a stream that started while darkbot
// was off starts a new dashboard session.

// newChatBot wires the chat bot to the app. Called in NewApp.
func (a *App) newChatBot() *chatbot.Bot {
	return chatbot.New(chatbot.Deps{
		Settings: func() config.Chat { return a.cfg.Get().Chat },
		Send:     func(ctx context.Context, text string) { a.sendChat(ctx, text, "") },
		Play: func(c config.ChatCommand, user string) {
			a.bot.PlayEvent(config.Sound{
				ID: "cmd-" + c.ID, RewardTitle: chatbot.Prefix + c.Name,
				File: c.Sound, Volume: c.Volume, Enabled: true,
			}, nil, user)
		},
		Used: func() { a.st.Command() },
		Live: a.live.Load,
		Log:  a.log,
	})
}

// setLive records the stream state. since is the stream's start (zero if
// unknown): a stream that began after the dashboard session did (darkbot
// was off when it started) starts a new session at that time.
func (a *App) setLive(live bool, since time.Time) {
	was := a.live.Swap(live)
	if live && !since.IsZero() && since.After(a.st.Snapshot().Since) {
		a.st.ResetSessionAt(since)
		a.log.Infof("The stream started at %s while darkbot was not running: new dashboard session", since.Local().Format(time.DateTime))
	}
	if was != live {
		a.emitState()
	}
}

// readLive asks Twitch whether the stream is live (needs only a login).
func (a *App) readLive(ctx context.Context, userID string) error {
	live, since, err := a.hx.LiveSince(ctx, userID)
	if err != nil {
		return err
	}
	a.setLive(live, since)
	return nil
}

// SetCommandsEnabled turns all chat commands on or off.
func (a *App) SetCommandsEnabled(on bool) error {
	return a.updateChat(func(c *config.Chat) error { c.CommandsEnabled = on; return nil })
}

// SetTimersEnabled turns all timers on or off.
func (a *App) SetTimersEnabled(on bool) error {
	return a.updateChat(func(c *config.Chat) error { c.TimersEnabled = on; return nil })
}

// SaveCommand adds a command (empty ID) or replaces the one with its ID.
func (a *App) SaveCommand(cmd config.ChatCommand) error {
	if cmd.Sound = strings.TrimSpace(cmd.Sound); cmd.Sound != "" {
		if !filepath.IsAbs(cmd.Sound) {
			return a.uiErr(i18n.E("err.pathAbsolute"))
		}
		if _, err := audio.Duration(cmd.Sound); err != nil {
			return a.uiErr(i18n.Wrap(err, "err.readFile"))
		}
	}
	return a.updateChat(func(c *config.Chat) error {
		norm, err := chatbot.NormalizeCommand(cmd, c.Commands, songs.CommandNames())
		if err != nil {
			return err
		}
		if i := slices.IndexFunc(c.Commands, func(o config.ChatCommand) bool { return o.ID == norm.ID && norm.ID != "" }); i >= 0 {
			c.Commands[i] = norm
			return nil
		}
		if len(c.Commands) >= config.MaxCommands {
			return i18n.E("err.tooManyCommands", "max", config.MaxCommands)
		}
		norm.ID = config.NewID()
		c.Commands = append(c.Commands, norm)
		return nil
	})
}

// DeleteCommand removes a command.
func (a *App) DeleteCommand(id string) error {
	return a.updateChat(func(c *config.Chat) error {
		c.Commands = slices.DeleteFunc(c.Commands, func(o config.ChatCommand) bool { return o.ID == id })
		return nil
	})
}

// SaveTimer adds a timer (empty ID) or replaces the one with its ID.
func (a *App) SaveTimer(t config.ChatTimer) error {
	return a.updateChat(func(c *config.Chat) error {
		norm, err := chatbot.NormalizeTimer(t)
		if err != nil {
			return err
		}
		if i := slices.IndexFunc(c.Timers, func(o config.ChatTimer) bool { return o.ID == norm.ID && norm.ID != "" }); i >= 0 {
			c.Timers[i] = norm
			return nil
		}
		if len(c.Timers) >= config.MaxTimers {
			return i18n.E("err.tooManyTimers", "max", config.MaxTimers)
		}
		norm.ID = config.NewID()
		c.Timers = append(c.Timers, norm)
		return nil
	})
}

// DeleteTimer removes a timer.
func (a *App) DeleteTimer(id string) error {
	return a.updateChat(func(c *config.Chat) error {
		c.Timers = slices.DeleteFunc(c.Timers, func(o config.ChatTimer) bool { return o.ID == id })
		return nil
	})
}

// updateChat changes the chat settings; a refused change leaves them as
// they were.
func (a *App) updateChat(fn func(c *config.Chat) error) error {
	var refused error
	err := a.cfg.Update(func(c *config.Config) {
		next := c.Clone().Chat
		if refused = fn(&next); refused == nil {
			c.Chat = next
		}
	})
	if refused != nil {
		return a.uiErr(refused)
	}
	a.emitState()
	return a.uiErr(err)
}

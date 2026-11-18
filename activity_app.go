package main

import (
	"encoding/json"

	"github.com/wailsapp/wails/v2/pkg/runtime"

	"github.com/miwidot/darkbot/internal/bot"
)

// The activity feed is kept on disk (internal/activity), so what happened
// between streams or before a restart is still there. Tests are shown but
// not kept; pad lights (playing/done) are not feed lines.

// keptBotKinds are the sound activities that are feed lines.
var keptBotKinds = map[string]bool{
	bot.ActQueued: true, bot.ActCooldown: true, bot.ActDisabled: true,
	bot.ActQueueFull: true, bot.ActError: true,
}

// keepActivity saves a feed line and shows it.
func (a *App) keepActivity(v any) {
	if err := a.act.Add(v); err != nil {
		a.log.Warnf("Activity not kept: %v", err)
	}
	if a.ctx != nil {
		runtime.EventsEmit(a.ctx, "activity", v)
	}
}

// GetActivity returns the kept feed, newest first.
func (a *App) GetActivity() []json.RawMessage {
	return a.act.List()
}

// ClearActivity empties the feed.
func (a *App) ClearActivity() error {
	if err := a.act.Clear(); err != nil {
		a.log.Warnf("Could not clear the activity feed: %v", err)
		return a.uiErr(err)
	}
	a.log.Infof("Activity feed cleared")
	return nil
}

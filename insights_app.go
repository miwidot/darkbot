package main

import (
	"github.com/miwidot/darkbot/internal/config"
	"github.com/miwidot/darkbot/internal/stats"
	"github.com/miwidot/darkbot/internal/twitch"
)

// The statistics view: top chatters, subs, Bits and raiders for this stream
// and the last seven days, and the chat activity per hour (internal/stats,
// people.go).

// GetInsights returns the statistics view's numbers.
func (a *App) GetInsights() stats.Insights { return a.st.Insights() }

// SaveIgnoredChatters stores the streamer's own list of accounts the
// statistics never count (on top of the known bots).
func (a *App) SaveIgnoredChatters(logins []string) error {
	norm, err := stats.NormalizeLogins(logins)
	if err != nil {
		return a.uiErr(err)
	}
	if err := a.cfg.Update(func(c *config.Config) { c.IgnoredChatters = norm }); err != nil {
		return a.uiErr(err)
	}
	a.st.SetIgnored(norm)
	a.emitState()
	return nil
}

// countChat counts a chat message for the statistics. The broadcaster's
// own messages don't count: darkbot's replies are sent as the broadcaster.
func (a *App) countChat(m twitch.ChatMessage) {
	if m.IsBroadcaster {
		return
	}
	a.st.ChatMessage(m.ChatterID, m.ChatterLogin, m.ChatterName)
}

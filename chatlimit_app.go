package main

import (
	"time"

	"github.com/miwidot/darkbot/internal/ratelimit"
)

// Everything darkbot posts goes out as the broadcaster, who may send 100
// messages per 30 s; more makes Twitch ignore the account for an hour
// (https://dev.twitch.tv/docs/chat/). darkbot stays well below that, so the
// streamer can still type, and caps event messages on their own: a
// follow-bot wave must not make the streamer's account spam chat.
const (
	chatBudgetWindow = 30 * time.Second
	// chatBudgetMax: all of darkbot's messages together (commands, songs,
	// events, timers).
	chatBudgetMax = 60
	// eventChatMax: event thank-you messages (follow, sub, raid, ...).
	eventChatMax = 15
)

// One app per process, so the budgets are process-wide.
var (
	chatBudget      = ratelimit.New(chatBudgetMax, chatBudgetWindow)
	eventChatBudget = ratelimit.New(eventChatMax, chatBudgetWindow)
)

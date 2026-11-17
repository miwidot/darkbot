package main

import (
	"context"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/wailsapp/wails/v2/pkg/runtime"

	"github.com/miwidot/darkbot/internal/audio"
	"github.com/miwidot/darkbot/internal/config"
	"github.com/miwidot/darkbot/internal/i18n"
	"github.com/miwidot/darkbot/internal/twitch"
)

// Channel events (follow, sub, bits, raid, ...): count them, show them in
// the activity feed and run the streamer's action (sound and/or chat).

// eventSoundPrefix marks event action sounds in the sound URL (/sound/ev-follow).
const eventSoundPrefix = "ev-"

// amountKinds have a number worth a threshold (bits, raid viewers, gifted subs).
var amountKinds = []string{twitch.EvCheer, twitch.EvRaid, twitch.EvGift}

// noChatKinds never post in chat: naming people who unfollowed would put
// them on the spot in front of the whole chat.
var noChatKinds = []string{twitch.EvUnfollow}

// EventActivity is a channel event for the activity feed.
type EventActivity struct {
	Kind  string              `json:"kind"` // always "event"
	Event twitch.ChannelEvent `json:"event"`
	Test  bool                `json:"test"`
	At    time.Time           `json:"at"`
}

// EventInfo tells the UI which options an event kind has.
type EventInfo struct {
	Kind      string `json:"kind"`
	HasAmount bool   `json:"hasAmount"`
	HasChat   bool   `json:"hasChat"`
}

// GetEventKinds lists the event kinds in display order.
func (a *App) GetEventKinds() []EventInfo {
	out := make([]EventInfo, 0, len(twitch.EventKinds))
	for _, k := range twitch.EventKinds {
		out = append(out, EventInfo{Kind: k, HasAmount: slices.Contains(amountKinds, k), HasChat: !slices.Contains(noChatKinds, k)})
	}
	return out
}

func (a *App) onChannelEvent(ev twitch.ChannelEvent) {
	a.handleEvent(ev, false)
}

func (a *App) handleEvent(ev twitch.ChannelEvent, test bool) {
	if !test {
		a.countEvent(ev)
		a.log.Infof("Event %s %s %d", ev.Kind, ev.User, ev.Amount)
	}
	if a.ctx != nil {
		runtime.EventsEmit(a.ctx, "activity", EventActivity{Kind: "event", Event: ev, Test: test, At: time.Now()})
	}
	a.runAction(ev)
}

func (a *App) countEvent(ev twitch.ChannelEvent) {
	switch ev.Kind {
	case twitch.EvOnline:
		// A new stream is a new session on the dashboard.
		a.st.ResetSession()
		a.log.Infof("Stream is live, dashboard session started")
	case twitch.EvFollow:
		a.st.Follow(ev.User)
	case twitch.EvUnfollow:
		a.st.Unfollow()
	case twitch.EvSub:
		if !ev.Gift { // gift recipients are counted by the gift event
			a.st.Sub(ev.User)
		}
	case twitch.EvResub:
		a.st.Sub(ev.User)
	case twitch.EvGift:
		a.st.Gift(ev.User, ev.Amount)
	case twitch.EvCheer:
		a.st.Cheer(ev.User, ev.Amount)
	case twitch.EvRaid:
		a.st.Raid(ev.Amount)
	}
}

// runAction plays the event sound and posts the chat message, if set.
func (a *App) runAction(ev twitch.ChannelEvent) {
	act, ok := a.cfg.Get().Events[ev.Kind]
	if !ok || !act.Enabled {
		return
	}
	if act.MinAmount > 0 && slices.Contains(amountKinds, ev.Kind) && ev.Amount < act.MinAmount {
		return
	}
	lang := a.lang()
	if act.Sound != "" {
		a.bot.PlayEvent(config.Sound{
			ID: eventSoundPrefix + ev.Kind, RewardTitle: i18n.T(lang, "event."+ev.Kind),
			File: act.Sound, Volume: act.Volume, Enabled: true,
		}, ev.User)
	}
	if act.Chat != "" && !slices.Contains(noChatKinds, ev.Kind) {
		go a.sendChat(a.ctx, renderTemplate(act.Chat, ev, lang), "")
	}
}

// renderTemplate fills {user}, {amount}, {tier} and {months}. Viewer text
// (cheer and resub messages) is deliberately not available: darkbot must
// never post what a viewer wrote under the streamer's name.
func renderTemplate(tpl string, ev twitch.ChannelEvent, lang string) string {
	who := ev.User
	if who == "" || ev.Anonymous {
		who = i18n.T(lang, "event.anonymous")
	}
	return strings.NewReplacer(
		"{user}", who,
		"{amount}", strconv.Itoa(ev.Amount),
		"{tier}", ev.Tier,
		"{months}", strconv.Itoa(ev.Months),
	).Replace(tpl)
}

// SaveEventAction stores the action of one event kind.
func (a *App) SaveEventAction(kind string, act config.EventAction) error {
	if !slices.Contains(twitch.EventKinds, kind) {
		return a.uiErr(i18n.E("err.eventKind", "kind", kind))
	}
	act.Sound = strings.TrimSpace(act.Sound)
	if act.Sound != "" {
		if !filepath.IsAbs(act.Sound) {
			return a.uiErr(i18n.E("err.pathAbsolute"))
		}
		if _, err := audio.Duration(act.Sound); err != nil {
			return a.uiErr(i18n.Wrap(err, "err.readFile"))
		}
	}
	if utf8.RuneCountInString(act.Chat) > config.MaxChatTemplate {
		return a.uiErr(i18n.E("err.chatTooLong", "max", config.MaxChatTemplate))
	}
	if slices.Contains(noChatKinds, kind) {
		act.Chat = ""
	}
	err := a.cfg.Update(func(c *config.Config) {
		if c.Events == nil {
			c.Events = map[string]config.EventAction{}
		}
		c.Events[kind] = act
	})
	a.emitState()
	return a.uiErr(err)
}

// sampleEvent is what "Test" sends through the action (not counted).
func (a *App) sampleEvent(kind string) twitch.ChannelEvent {
	ev := twitch.ChannelEvent{Kind: kind, User: "darkbot", Tier: "1"}
	const sampleBits, sampleViewers, sampleGifts, sampleMonths, sampleLevel, sampleAdSeconds = 100, 25, 5, 12, 2, 90
	switch kind {
	case twitch.EvCheer:
		ev.Amount = sampleBits
	case twitch.EvRaid, twitch.EvShoutout:
		ev.Amount = sampleViewers
	case twitch.EvGift:
		ev.Amount = sampleGifts
	case twitch.EvResub:
		ev.Months = sampleMonths
	case twitch.EvHypeBegin, twitch.EvHypeEnd:
		ev.Amount, ev.User = sampleLevel, ""
	case twitch.EvAdBreak:
		ev.Amount, ev.User = sampleAdSeconds, ""
	case twitch.EvOnline, twitch.EvOffline:
		ev.User = ""
	}
	return ev
}

// TestEvent runs the action of a kind with sample data. It is marked as a
// test in the feed and never counted. The chat message is really sent, so
// the streamer sees what viewers would see.
func (a *App) TestEvent(kind string) error {
	if !slices.Contains(twitch.EventKinds, kind) {
		return a.uiErr(i18n.E("err.eventKind", "kind", kind))
	}
	a.handleEvent(a.sampleEvent(kind), true)
	return nil
}

// SetUnfollowDetection switches the follower comparison on or off.
func (a *App) SetUnfollowDetection(on bool) error {
	if err := a.cfg.Update(func(c *config.Config) { c.UnfollowDetection = on }); err != nil {
		return a.uiErr(err)
	}
	a.stopFollowerWatcher()
	if on {
		a.startFollowerWatcher()
	}
	a.emitState()
	return nil
}

func (a *App) startFollowerWatcher() {
	if !a.cfg.Get().UnfollowDetection || a.tok.Current() == nil {
		return
	}
	a.stopFollowerWatcher()
	ctx, cancel := context.WithCancel(a.ctx)
	w := twitch.NewFollowerWatcher(a.hx, a.tok, a.log, a.onChannelEvent)
	a.mu.Lock()
	a.fwCancel, a.fw = cancel, w
	a.mu.Unlock()
	go w.Run(ctx)
	go a.emitFollowerStatus(ctx)
}

// emitFollowerStatus refreshes the UI after each check.
func (a *App) emitFollowerStatus(ctx context.Context) {
	t := time.NewTicker(time.Minute)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			a.emitState()
		}
	}
}

func (a *App) stopFollowerWatcher() {
	a.mu.Lock()
	cancel := a.fwCancel
	a.fwCancel, a.fw = nil, nil
	a.mu.Unlock()
	if cancel != nil {
		cancel()
	}
}

func (a *App) followerStatus() *twitch.FollowerStatus {
	a.mu.Lock()
	w := a.fw
	a.mu.Unlock()
	if w == nil {
		return nil
	}
	s := w.Status()
	return &s
}

// eventSoundFile resolves /sound/ev-<kind> for serveSound.
func (a *App) eventSoundFile(id string) (string, bool) {
	kind, ok := strings.CutPrefix(id, eventSoundPrefix)
	if !ok {
		return "", false
	}
	act, ok := a.cfg.Get().Events[kind]
	if !ok || act.Sound == "" {
		return "", false
	}
	return act.Sound, true
}

package main

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/wailsapp/wails/v2/pkg/runtime"

	"github.com/miwidot/darkbot/internal/audio"
	"github.com/miwidot/darkbot/internal/bot"
	"github.com/miwidot/darkbot/internal/config"
	"github.com/miwidot/darkbot/internal/i18n"
	"github.com/miwidot/darkbot/internal/obs"
	"github.com/miwidot/darkbot/internal/twitch"
)

// Channel events (follow, sub, bits, raid, ...): count them, show them in
// the activity feed and run the streamer's action (sound and/or chat).

// eventSoundPrefix marks event action sounds in the sound URL (/sound/ev-follow).
const eventSoundPrefix = "ev-"

// amountKinds have a number worth a threshold (bits, raid viewers, gifted
// subs, hype train level).
var amountKinds = []string{twitch.EvCheer, twitch.EvPowerUp, twitch.EvAutoReward, twitch.EvRaid, twitch.EvGift, twitch.EvHypeLevel, twitch.EvWatchStreak}

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
	// Category groups kinds into the Alerts tab's sub-tabs.
	Category string `json:"category"`
	// Extra names a setting only this kind has: "shoutout" (raid) or
	// "lead" (ad warning).
	Extra string `json:"extra,omitempty"`
}

// Kind-only settings (EventInfo.Extra).
const (
	extraShoutout = "shoutout"
	extraLead     = "lead"
)

var eventExtras = map[string]string{twitch.EvRaid: extraShoutout, twitch.EvAdSoon: extraLead}

// GetEventKinds lists the event kinds in display order.
func (a *App) GetEventKinds() []EventInfo {
	out := make([]EventInfo, 0, len(twitch.EventKinds))
	for _, k := range twitch.EventKinds {
		out = append(out, EventInfo{Kind: k, HasAmount: slices.Contains(amountKinds, k), HasChat: !slices.Contains(noChatKinds, k), Category: twitch.Category(k), Extra: eventExtras[k]})
	}
	return out
}

func (a *App) onChannelEvent(ev twitch.ChannelEvent) {
	a.handleEvent(ev, false)
}

func (a *App) handleEvent(ev twitch.ChannelEvent, test bool) {
	if ev.Goal != nil || ev.Vote != nil {
		a.overlayEvent(ev, test)
	}
	if twitch.IsOverlayOnly(ev.Kind) {
		return // progress: overlays only, no feed line, no alert
	}
	if ev.Kind == twitch.EvSharedChatBegin || ev.Kind == twitch.EvSharedChatEnd {
		a.setSharedChat(ev.Kind == twitch.EvSharedChatBegin)
		a.keepActivity(EventActivity{Kind: "event", Event: ev, At: time.Now()})
		return
	}
	if ev.Kind == twitch.EvSub && ev.Gift {
		// A recipient of a gifted sub. Twitch sends one per recipient right
		// after the gift event, so a sub bomb of 50 would mean 50 sounds, 50
		// chat messages and 50 feed lines. The gift event (one per bomb, with
		// the count) is what the streamer reacts to; recipients are counted
		// there too. Same split as Streamer.bot's Gift Bomb trigger.
		return
	}
	at := time.Now()
	if !ev.At.IsZero() {
		at = ev.At
	}
	act := EventActivity{Kind: "event", Event: ev, Test: test, At: at}
	if ev.Missed {
		// Found after darkbot was off (follower list): too late for a sound
		// or a chat message, and not part of this session's numbers.
		a.log.Infof("Event %s %s found, it happened while darkbot was off", ev.Kind, ev.User)
		a.keepActivity(act)
		return
	}
	if !test {
		a.countEvent(ev)
		if channelKinds[ev.Kind] {
			a.refreshChannelTotalsSoon()
		}
		a.log.Infof("Event %s %s %d", ev.Kind, ev.User, ev.Amount)
		a.keepActivity(act)
	} else if a.ctx != nil {
		runtime.EventsEmit(a.ctx, "activity", act) // tests show, but aren't kept
	}
	a.runAction(ev)
}

func (a *App) countEvent(ev twitch.ChannelEvent) {
	switch ev.Kind {
	case twitch.EvOnline:
		// A new stream is a new session on the dashboard.
		a.st.ResetSession()
		a.setLive(true, time.Time{})
		a.log.Infof("Stream is live, dashboard session started")
	case twitch.EvOffline:
		a.setLive(false, time.Time{})
	case twitch.EvFollow:
		a.st.Follow(ev.User)
		a.mu.Lock()
		fw := a.fw
		a.mu.Unlock()
		if fw != nil {
			fw.Observe(ev.UserID, ev.User)
		}
	case twitch.EvUnfollow:
		a.st.Unfollow()
	case twitch.EvSub:
		a.st.Sub(ev.UserID, ev.User) // gift recipients never get here, see handleEvent
	case twitch.EvResub:
		a.st.Sub(ev.UserID, ev.User)
	case twitch.EvGift:
		a.st.Gift(ev.UserID, ev.User, ev.Amount)
	case twitch.EvCheer, twitch.EvPowerUp:
		a.st.Cheer(ev.UserID, ev.User, ev.Amount) // Bits are Bits, cheered or spent on a Power-up
	case twitch.EvRaid:
		a.st.Raid(ev.UserID, ev.User, ev.Amount)
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
	var alert *bot.Alert
	if act.Visual != "" || act.OverlayText != "" {
		alert = &bot.Alert{File: act.Visual, Seconds: act.Seconds}
		if act.OverlayText != "" && !slices.Contains(noChatKinds, ev.Kind) {
			alert.Text = renderTemplate(act.OverlayText, ev, lang)
		}
	}
	if act.Sound != "" || alert != nil {
		a.bot.PlayEvent(config.Sound{
			ID: eventSoundPrefix + ev.Kind, RewardTitle: i18n.T(lang, "event."+ev.Kind),
			File: act.Sound, Volume: act.Volume, Enabled: true,
		}, alert, ev.User)
	}
	if act.Shoutout && ev.Kind == twitch.EvRaid {
		go a.shoutout(ev)
	}
	if act.Chat != "" && !slices.Contains(noChatKinds, ev.Kind) {
		if ok, first := eventChatBudget.Allow(time.Now()); ok {
			go a.sendChat(a.ctx, renderTemplate(act.Chat, ev, lang), "")
		} else if first {
			a.log.Warnf("Event chat messages skipped: more than %d events in %s (e.g. a follow-bot wave)", eventChatMax, chatBudgetWindow)
		}
	}
}

// renderTemplate fills {user}, {amount}, {tier}, {months}, {top} (biggest
// hype train contributor, "everyone" if Twitch names none), {powerup},
// {reward} (Twitch's own channel point reward) and, for a channel update,
// {title} and {category} (the streamer's own text). Viewer text
// (cheer and resub messages) is deliberately not available: darkbot must
// never post what a viewer wrote under the streamer's name.
func renderTemplate(tpl string, ev twitch.ChannelEvent, lang string) string {
	who := ev.User
	if who == "" || ev.Anonymous {
		who = i18n.T(lang, "event.anonymous")
	}
	top := ev.Top
	if top == "" {
		top = i18n.T(lang, "event.everyone")
	}
	return strings.NewReplacer(
		"{user}", who,
		"{top}", top,
		"{amount}", strconv.Itoa(ev.Amount),
		"{tier}", ev.Tier,
		"{months}", strconv.Itoa(ev.Months),
		"{points}", strconv.Itoa(ev.Points),
		"{powerup}", powerUpName(ev, lang),
		"{reward}", autoRewardName(ev, lang),
		"{title}", ev.Title,
		"{category}", ev.Category,
		"{winner}", ev.Winner,
	).Replace(tpl)
}

// autoRewardName names one of Twitch's own channel point rewards.
func autoRewardName(ev twitch.ChannelEvent, lang string) string {
	if key := "autoreward." + ev.Reward; ev.Reward != "" && i18n.Has(key) {
		return i18n.T(lang, key)
	}
	return i18n.T(lang, "autoreward.other")
}

// powerUpName names a Power-up: a custom one by the title the streamer gave
// it, Twitch's own by their name in the UI language.
func powerUpName(ev twitch.ChannelEvent, lang string) string {
	if ev.PowerUpType == twitch.PowerUpCustom && ev.PowerUpTitle != "" {
		return ev.PowerUpTitle
	}
	if key := "powerup." + ev.PowerUpType; ev.PowerUpType != "" && i18n.Has(key) {
		return i18n.T(lang, key)
	}
	return i18n.T(lang, "powerup.other")
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
		if _, err := audio.Check(act.Sound); err != nil {
			return a.uiErr(readErr(err))
		}
	}
	// One chat line: line breaks and runs of spaces become single spaces.
	act.Chat = strings.Join(strings.Fields(act.Chat), " ")
	act.OverlayText = strings.Join(strings.Fields(act.OverlayText), " ")
	act.Visual = strings.TrimSpace(act.Visual)
	if act.Visual != "" {
		if !filepath.IsAbs(act.Visual) {
			return a.uiErr(i18n.E("err.pathAbsolute"))
		}
		if !obs.IsVisual(act.Visual) {
			return a.uiErr(i18n.E("err.visualType", "types", strings.Join(append(append([]string{}, obs.ImageExtensions...), obs.VideoExtensions...), ", ")))
		}
		if _, err := os.Stat(act.Visual); err != nil {
			return a.uiErr(i18n.Wrap(err, "err.readFile"))
		}
	}
	if utf8.RuneCountInString(act.OverlayText) > config.MaxOverlayText {
		return a.uiErr(i18n.E("err.overlayTooLong", "max", config.MaxOverlayText))
	}
	if eventExtras[kind] != extraShoutout {
		act.Shoutout = false
	}
	if eventExtras[kind] != extraLead {
		act.LeadSeconds = 0
	} else if act.LeadSeconds != 0 && (act.LeadSeconds < config.MinAdLead || act.LeadSeconds > config.MaxAdLead) { // 0 = default
		return a.uiErr(i18n.E("err.adLead", "min", config.MinAdLead, "max", config.MaxAdLead))
	}
	if act.Seconds < 0 || act.Seconds > config.MaxAlertSeconds {
		return a.uiErr(i18n.E("err.alertSeconds", "max", config.MaxAlertSeconds))
	}
	if utf8.RuneCountInString(act.Chat) > config.MaxChatTemplate {
		return a.uiErr(i18n.E("err.chatTooLong", "max", config.MaxChatTemplate))
	}
	if slices.Contains(noChatKinds, kind) {
		// Never name people who unfollowed, neither in chat nor on screen.
		act.Chat, act.OverlayText = "", ""
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
	const sampleBits, sampleViewers, sampleGifts, sampleMonths, sampleLevel, sampleAdSeconds = 100, 25, 5, 12, 3, 90
	const sampleStreak, sampleStreakPoints = 5, 450
	const samplePowerUpBits, samplePowerUp = 50, "gigantify_an_emote"
	const sampleRewardPoints, sampleReward, sampleCategory = 300, "send_highlighted_message", "Just Chatting"
	switch kind {
	case twitch.EvCheer:
		ev.Amount = sampleBits
	case twitch.EvPowerUp:
		ev.Amount, ev.PowerUpType = samplePowerUpBits, samplePowerUp
	case twitch.EvAutoReward:
		ev.Amount, ev.Reward = sampleRewardPoints, sampleReward
	case twitch.EvAdSoon:
		ev.Amount, ev.User = config.DefaultAdLead, ""
	case twitch.EvGoalBegin, twitch.EvGoalAchieved:
		ev = sampleGoal(kind, a.lang())
	case twitch.EvPollBegin, twitch.EvPollEnd, twitch.EvPredictionBegin, twitch.EvPredictionLock, twitch.EvPredictionEnd:
		ev = sampleVote(kind, a.lang())
	case twitch.EvChannelUpdate:
		ev.User, ev.Title, ev.Category = "", i18n.T(a.lang(), "event.sampleTitle"), sampleCategory
	case twitch.EvRaid, twitch.EvShoutout:
		ev.Amount = sampleViewers
	case twitch.EvGift:
		ev.Amount = sampleGifts
	case twitch.EvResub:
		ev.Months = sampleMonths
	case twitch.EvHypeBegin, twitch.EvHypeLevel:
		ev.Amount, ev.User = sampleLevel, ""
	case twitch.EvHypeEnd:
		ev.Amount, ev.User, ev.Top = sampleLevel, "", "darkbot"
	case twitch.EvAdBreak:
		ev.Amount, ev.User = sampleAdSeconds, ""
	case twitch.EvWatchStreak:
		ev.Amount, ev.Points = sampleStreak, sampleStreakPoints
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
	a.life.Lock()
	a.stopFollowerWatcherLocked()
	if on {
		a.startFollowerWatcherLocked()
	}
	a.life.Unlock()
	a.emitState()
	return nil
}

// startFollowerWatcherLocked starts unfollow detection if it is on; call
// with a.life held.
func (a *App) startFollowerWatcherLocked() {
	if !a.cfg.Get().UnfollowDetection || a.tok.Current() == nil {
		return
	}
	a.stopFollowerWatcherLocked()
	ctx, cancel := context.WithCancel(a.ctx)
	w := twitch.NewFollowerWatcher(a.hx, a.tok, a.log, a.cfg.FollowersPath(), a.onChannelEvent)
	a.mu.Lock()
	a.fwCancel, a.fw = cancel, w
	a.mu.Unlock()
	w.OnChecked(a.emitState) // the status line shows the last check
	go w.Run(ctx)
}

// stopFollowerWatcherLocked stops unfollow detection; call with a.life held.
func (a *App) stopFollowerWatcherLocked() {
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

// sampleGoal is a goal for the "Test" button: half way, or reached. It
// also shows on the goal overlay for a moment.
func sampleGoal(kind, lang string) twitch.ChannelEvent {
	const sampleTarget = 100
	g := &twitch.Goal{ID: "sample-goal", Type: twitch.GoalFollow, Description: i18n.T(lang, "sample.goal"), Current: sampleTarget / 2, Target: sampleTarget}
	if kind == twitch.EvGoalAchieved {
		g.Current = sampleTarget
	}
	return twitch.ChannelEvent{Kind: kind, Title: g.Description, Amount: g.Target, Goal: g, GoalType: g.Type}
}

// sampleVote is a poll or prediction for the "Test" button (also shown on
// the poll overlay for a moment).
func sampleVote(kind, lang string) twitch.ChannelEvent {
	prediction := kind == twitch.EvPredictionBegin || kind == twitch.EvPredictionLock || kind == twitch.EvPredictionEnd
	v := &twitch.Vote{ID: "sample-vote", Prediction: prediction, State: twitch.VoteActive, Until: time.Now().Add(testLinger)}
	if prediction {
		v.Title = i18n.T(lang, "sample.prediction")
		v.Options = []twitch.VoteOption{
			{ID: "a", Title: i18n.T(lang, "sample.yes"), Votes: 12500, Users: 14, Color: "blue"},
			{ID: "b", Title: i18n.T(lang, "sample.no"), Votes: 4200, Users: 6, Color: "pink"},
		}
	} else {
		v.Title = i18n.T(lang, "sample.poll")
		v.Options = []twitch.VoteOption{
			{ID: "a", Title: i18n.T(lang, "sample.option1"), Votes: 21},
			{ID: "b", Title: i18n.T(lang, "sample.option2"), Votes: 13},
			{ID: "c", Title: i18n.T(lang, "sample.option3"), Votes: 5},
		}
	}
	ev := twitch.ChannelEvent{Kind: kind, Title: v.Title, Vote: v}
	switch kind {
	case twitch.EvPredictionLock:
		v.State = twitch.VoteLocked
	case twitch.EvPollEnd, twitch.EvPredictionEnd:
		lead, _ := v.Leader()
		v.State, v.Winner, ev.Winner, ev.Amount = twitch.VoteEnded, lead.ID, lead.Title, lead.Votes
	}
	return ev
}

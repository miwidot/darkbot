package twitch

import (
	"encoding/json"
	"fmt"
	"strconv"
	"sync"
	"time"
)

// Channel events (verified 2026-11-17, https://dev.twitch.tv/docs/eventsub/reference/):
// every type darkbot listens to besides redemptions and chat. Subscriptions
// authorized by the broadcaster's scopes cost 0; the ones without a scope
// (raid, stream online/offline) cost 1 each against max_total_cost 10.
//
// Twitch has no unfollow event; FollowerWatcher detects unfollows by
// comparing the follower list (opt-in).

// maxUnscopedCost: EventSub WebSocket allows a total subscription cost of
// 10; every subscription without an authorizing scope costs 1.
const maxUnscopedCost = 10

// Event kinds darkbot reports and can attach actions to.
const (
	EvFollow    = "follow"
	EvUnfollow  = "unfollow" // from FollowerWatcher, not EventSub
	EvSub       = "sub"
	EvResub     = "resub"
	EvGift      = "gift"
	EvCheer     = "cheer"
	EvRaid      = "raid"
	EvHypeBegin = "hype_begin"
	EvHypeLevel = "hype_level" // derived from channel.hype_train.progress, see hypeTracker
	EvHypeEnd   = "hype_end"
	EvOnline    = "online"
	EvOffline   = "offline"
	EvAdBreak   = "ad_break"
	EvShoutout  = "shoutout"
	EvSubEnd    = "sub_end"
	// EvWatchStreak: a viewer shared their watch streak in chat (from
	// channel.chat.notification, notice_type watch_streak).
	EvWatchStreak = "watch_streak"
)

// EventKinds lists every kind in the order the UI shows them.
var EventKinds = []string{EvFollow, EvUnfollow, EvSub, EvResub, EvGift, EvCheer, EvRaid, EvWatchStreak, EvHypeBegin, EvHypeLevel, EvHypeEnd, EvShoutout, EvAdBreak, EvSubEnd, EvOnline, EvOffline}

// Scopes for channel events.
const (
	ScopeReadFollowers = "moderator:read:followers"
	ScopeReadSubs      = "channel:read:subscriptions"
	ScopeReadBits      = "bits:read"
	ScopeReadHypeTrain = "channel:read:hype_train"
	ScopeReadAds       = "channel:read:ads"
	ScopeReadShoutouts = "moderator:read:shoutouts"
)

// ChannelEvent is one normalized channel event.
type ChannelEvent struct {
	Kind      string `json:"kind"`
	MessageID string `json:"-"`    // EventSub message id (dedupe)
	User      string `json:"user"` // display name; "" for anonymous/none
	UserID    string `json:"-"`
	Amount    int    `json:"amount"`  // bits, gifted subs, raid viewers, hype level, ad seconds, shoutout viewers
	Tier      string `json:"tier"`    // "1", "2", "3" for subs ("" otherwise)
	Months    int    `json:"months"`  // resub: cumulative months
	Message   string `json:"message"` // resub/cheer message
	Anonymous bool   `json:"anonymous"`
	Gift      bool   `json:"gift"`   // sub/sub_end was a gifted sub
	Top       string `json:"top"`    // hype_end: biggest contributor ("" if unknown)
	Points    int    `json:"points"` // watch_streak: channel points Twitch awarded
	// Missed: happened while darkbot was off and was found later (follower
	// list comparison). Shown in the feed only: no sound, chat or counting.
	Missed bool `json:"missed"`
	// At is when it happened, if known and not "now" (a missed follow).
	At time.Time `json:"-"`
	// TrainID identifies the hype train (begin/progress/end), for hypeTracker.
	TrainID string `json:"-"`
}

// evHypeProgress is a raw hype train progress notification. It never leaves
// this package: hypeTracker turns level changes into EvHypeLevel.
const evHypeProgress = "hype_progress"

// eventSpec is one EventSub subscription darkbot creates.
type eventSpec struct {
	Type    string
	Version string
	Scope   string // "" = no authorization needed
	// condition builds the subscription condition for the broadcaster.
	condition func(broadcasterID string) map[string]string
	parse     func(raw json.RawMessage) (ChannelEvent, error)
}

func byBroadcaster(id string) map[string]string { return map[string]string{"broadcaster_user_id": id} }

// readChatAs: chat types read the broadcaster's chat as the broadcaster.
func readChatAs(id string) map[string]string {
	return map[string]string{"broadcaster_user_id": id, "user_id": id}
}

const (
	chatNotificationType = "channel.chat.notification"
	noticeWatchStreak    = "watch_streak"
)

// asModerator: types that need a moderator_user_id; the broadcaster counts.
func asModerator(id string) map[string]string {
	return map[string]string{"broadcaster_user_id": id, "moderator_user_id": id}
}

// user is the user_* triple that most payloads share. Nullable for
// anonymous gifts/cheers.
type user struct {
	ID   *string `json:"user_id"`
	Name *string `json:"user_name"`
}

func (u user) name() string {
	if u.Name == nil {
		return ""
	}
	return *u.Name
}

func (u user) id() string {
	if u.ID == nil {
		return ""
	}
	return *u.ID
}

// tierNumber turns Twitch's "1000"/"2000"/"3000" into "1"/"2"/"3".
func tierNumber(t string) string {
	n, err := strconv.Atoi(t)
	if err != nil || n < 1000 {
		return t
	}
	return strconv.Itoa(n / 1000)
}

func decode[T any](raw json.RawMessage) (T, error) {
	var v T
	err := json.Unmarshal(raw, &v)
	return v, err
}

// hypeTrain is the part of the hype train v2 payloads darkbot uses.
type hypeTrain struct {
	ID    string `json:"id"`
	Level int    `json:"level"`
	// Top contributions per type (bits, subscription, other). total is in
	// hype train points: Bits 1:1, subs 500/1000/2500 for tier 1/2/3.
	Top []struct {
		UserName string `json:"user_name"`
		Total    int    `json:"total"`
	} `json:"top_contributions"`
}

// topContributor is whoever put the most points into the train.
func (h hypeTrain) topContributor() string {
	best, name := 0, ""
	for _, c := range h.Top {
		if c.Total > best {
			best, name = c.Total, c.UserName
		}
	}
	return name
}

// hypeTracker turns hype train progress into level-up events. It remembers
// the level of the current train; a progress with a higher level is a
// level-up (one event even if a big contribution skipped levels). A train
// first seen through progress (darkbot connected mid-train) only sets the
// baseline, so a reconnect never fakes a level-up.
type hypeTracker struct {
	mu    sync.Mutex
	train string
	level int
}

// filter returns the event to report, or false to drop it.
func (h *hypeTracker) filter(ev ChannelEvent) (ChannelEvent, bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	switch ev.Kind {
	case EvHypeBegin:
		h.train, h.level = ev.TrainID, ev.Amount
	case EvHypeEnd:
		h.train, h.level = "", 0
	case evHypeProgress:
		if ev.TrainID != h.train {
			h.train, h.level = ev.TrainID, ev.Amount
			return ev, false
		}
		if ev.Amount <= h.level {
			return ev, false
		}
		h.level = ev.Amount
		ev.Kind = EvHypeLevel
	}
	return ev, true
}

var eventSpecs = []eventSpec{
	{Type: "channel.follow", Version: "2", Scope: ScopeReadFollowers, condition: asModerator,
		parse: func(raw json.RawMessage) (ChannelEvent, error) {
			v, err := decode[user](raw)
			return ChannelEvent{Kind: EvFollow, User: v.name(), UserID: v.id()}, err
		}},
	{Type: "channel.subscribe", Version: "1", Scope: ScopeReadSubs, condition: byBroadcaster,
		parse: func(raw json.RawMessage) (ChannelEvent, error) {
			v, err := decode[struct {
				user
				Tier   string `json:"tier"`
				IsGift bool   `json:"is_gift"`
			}](raw)
			return ChannelEvent{Kind: EvSub, User: v.name(), UserID: v.id(), Tier: tierNumber(v.Tier), Gift: v.IsGift}, err
		}},
	{Type: "channel.subscription.message", Version: "1", Scope: ScopeReadSubs, condition: byBroadcaster,
		parse: func(raw json.RawMessage) (ChannelEvent, error) {
			v, err := decode[struct {
				user
				Tier    string `json:"tier"`
				Months  int    `json:"cumulative_months"`
				Message struct {
					Text string `json:"text"`
				} `json:"message"`
			}](raw)
			return ChannelEvent{Kind: EvResub, User: v.name(), UserID: v.id(), Tier: tierNumber(v.Tier), Months: v.Months, Message: v.Message.Text}, err
		}},
	{Type: "channel.subscription.gift", Version: "1", Scope: ScopeReadSubs, condition: byBroadcaster,
		parse: func(raw json.RawMessage) (ChannelEvent, error) {
			v, err := decode[struct {
				user
				Total     int    `json:"total"`
				Tier      string `json:"tier"`
				Anonymous bool   `json:"is_anonymous"`
			}](raw)
			return ChannelEvent{Kind: EvGift, User: v.name(), UserID: v.id(), Amount: v.Total, Tier: tierNumber(v.Tier), Anonymous: v.Anonymous}, err
		}},
	{Type: "channel.subscription.end", Version: "1", Scope: ScopeReadSubs, condition: byBroadcaster,
		parse: func(raw json.RawMessage) (ChannelEvent, error) {
			v, err := decode[struct {
				user
				Tier   string `json:"tier"`
				IsGift bool   `json:"is_gift"`
			}](raw)
			return ChannelEvent{Kind: EvSubEnd, User: v.name(), UserID: v.id(), Tier: tierNumber(v.Tier), Gift: v.IsGift}, err
		}},
	{Type: "channel.cheer", Version: "1", Scope: ScopeReadBits, condition: byBroadcaster,
		parse: func(raw json.RawMessage) (ChannelEvent, error) {
			v, err := decode[struct {
				user
				Anonymous bool   `json:"is_anonymous"`
				Message   string `json:"message"`
				Bits      int    `json:"bits"`
			}](raw)
			return ChannelEvent{Kind: EvCheer, User: v.name(), UserID: v.id(), Amount: v.Bits, Message: v.Message, Anonymous: v.Anonymous}, err
		}},
	{Type: "channel.raid", Version: "1", Scope: "",
		condition: func(id string) map[string]string { return map[string]string{"to_broadcaster_user_id": id} },
		parse: func(raw json.RawMessage) (ChannelEvent, error) {
			v, err := decode[struct {
				FromID   string `json:"from_broadcaster_user_id"`
				FromName string `json:"from_broadcaster_user_name"`
				Viewers  int    `json:"viewers"`
			}](raw)
			return ChannelEvent{Kind: EvRaid, User: v.FromName, UserID: v.FromID, Amount: v.Viewers}, err
		}},
	{Type: "channel.hype_train.begin", Version: "2", Scope: ScopeReadHypeTrain, condition: byBroadcaster,
		parse: func(raw json.RawMessage) (ChannelEvent, error) {
			v, err := decode[hypeTrain](raw)
			return ChannelEvent{Kind: EvHypeBegin, Amount: v.Level, TrainID: v.ID}, err
		}},
	// Twitch has no level-up event; progress is sent periodically while the
	// train gains points and carries the current level.
	{Type: "channel.hype_train.progress", Version: "2", Scope: ScopeReadHypeTrain, condition: byBroadcaster,
		parse: func(raw json.RawMessage) (ChannelEvent, error) {
			v, err := decode[hypeTrain](raw)
			return ChannelEvent{Kind: evHypeProgress, Amount: v.Level, TrainID: v.ID}, err
		}},
	{Type: "channel.hype_train.end", Version: "2", Scope: ScopeReadHypeTrain, condition: byBroadcaster,
		parse: func(raw json.RawMessage) (ChannelEvent, error) {
			v, err := decode[hypeTrain](raw)
			return ChannelEvent{Kind: EvHypeEnd, Amount: v.Level, TrainID: v.ID, Top: v.topContributor()}, err
		}},
	{Type: "channel.shoutout.receive", Version: "1", Scope: ScopeReadShoutouts, condition: asModerator,
		parse: func(raw json.RawMessage) (ChannelEvent, error) {
			v, err := decode[struct {
				FromID   string `json:"from_broadcaster_user_id"`
				FromName string `json:"from_broadcaster_user_name"`
				Viewers  int    `json:"viewer_count"`
			}](raw)
			return ChannelEvent{Kind: EvShoutout, User: v.FromName, UserID: v.FromID, Amount: v.Viewers}, err
		}},
	{Type: "channel.ad_break.begin", Version: "1", Scope: ScopeReadAds, condition: byBroadcaster,
		parse: func(raw json.RawMessage) (ChannelEvent, error) {
			v, err := decode[struct {
				Seconds int `json:"duration_seconds"`
			}](raw)
			return ChannelEvent{Kind: EvAdBreak, Amount: v.Seconds}, err
		}},
	// Chat notices (https://dev.twitch.tv/docs/eventsub/reference/chat,
	// checked 2026-11-18): Twitch added Watch Streaks on 2026-04-02;
	// watch_streak carries streak_count ("consecutive broadcasts") and
	// channel_points_awarded. The other notice types (subs, raids, ...) come
	// through their own subscriptions above, so only watch_streak is used.
	{Type: chatNotificationType, Version: "1", Scope: ScopeReadChat, condition: readChatAs,
		parse: func(raw json.RawMessage) (ChannelEvent, error) {
			v, err := decode[struct {
				BroadcasterID string  `json:"broadcaster_user_id"`
				ChatterID     string  `json:"chatter_user_id"`
				ChatterName   string  `json:"chatter_user_name"`
				Anonymous     bool    `json:"chatter_is_anonymous"`
				NoticeType    string  `json:"notice_type"`
				Source        *string `json:"source_broadcaster_user_id"`
				WatchStreak   *struct {
					Count  int `json:"streak_count"`
					Points int `json:"channel_points_awarded"`
				} `json:"watch_streak"`
			}](raw)
			if err != nil || v.NoticeType != noticeWatchStreak || v.WatchStreak == nil {
				return ChannelEvent{}, err // not a watch streak: ignored
			}
			if v.Source != nil && *v.Source != "" && *v.Source != v.BroadcasterID {
				return ChannelEvent{}, nil // another channel's chat (Shared Chat)
			}
			return ChannelEvent{Kind: EvWatchStreak, User: v.ChatterName, UserID: v.ChatterID,
				Amount: v.WatchStreak.Count, Points: v.WatchStreak.Points, Anonymous: v.Anonymous}, nil
		}},
	{Type: "stream.online", Version: "1", Scope: "", condition: byBroadcaster,
		parse: func(json.RawMessage) (ChannelEvent, error) { return ChannelEvent{Kind: EvOnline}, nil }},
	{Type: "stream.offline", Version: "1", Scope: "", condition: byBroadcaster,
		parse: func(json.RawMessage) (ChannelEvent, error) { return ChannelEvent{Kind: EvOffline}, nil }},
}

// specByType finds the spec of an EventSub subscription type.
func specByType(t string) (eventSpec, bool) {
	for _, s := range eventSpecs {
		if s.Type == t {
			return s, true
		}
	}
	return eventSpec{}, false
}

// parseChannelEvent turns a notification's event payload into a ChannelEvent.
func parseChannelEvent(subType string, raw json.RawMessage) (ChannelEvent, error) {
	s, ok := specByType(subType)
	if !ok {
		return ChannelEvent{}, fmt.Errorf("unknown event type %q", subType)
	}
	return s.parse(raw)
}

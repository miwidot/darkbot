package twitch

import (
	"encoding/json"
	"fmt"
	"strconv"
	"sync"
	"time"
)

// Channel events (verified 2026-11-17, Bits 2026-11-19, https://dev.twitch.tv/docs/eventsub/reference/):
// every type darkbot listens to besides redemptions and chat. A type that
// needs no scope but names a user (raid, stream online/offline) costs 1
// only when that user has not authorized the app; the broadcaster has, so
// all cost 0 ("there is no cost if that user has authorized your
// application", subscription limits, checked 2026-11-19).
// maxUnscopedCost stays as a safety limit in case that ever changes.
//
// Twitch has no unfollow event; FollowerWatcher detects unfollows by
// comparing the follower list (opt-in).

// maxUnscopedCost: EventSub WebSocket allows a total subscription cost of
// 10; at worst each subscription without a scope would cost 1.
const maxUnscopedCost = 10

// Event kinds darkbot reports and can attach actions to.
const (
	EvFollow    = "follow"
	EvUnfollow  = "unfollow" // from FollowerWatcher, not EventSub
	EvSub       = "sub"
	EvResub     = "resub"
	EvGift      = "gift"
	EvCheer     = "cheer"
	// EvPowerUp: a viewer spent Bits on a Power-up (built-in or the
	// channel's own), from channel.bits.use.
	EvPowerUp = "power_up"
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
	// EvSharedChatBegin and EvSharedChatEnd: the channel joined or left a
	// Shared Chat session. Not in EventKinds: they have no action, darkbot
	// itself stops and resumes writing in chat.
	// EvChannelUpdate: the stream title or category changed (from
	// channel.update; other changes such as labels are dropped).
	EvChannelUpdate = "channel_update"
	// EvAutoReward: a viewer redeemed one of Twitch's own channel point
	// rewards (highlighted message, emote unlock, ...).
	EvAutoReward      = "auto_reward"
	EvSharedChatBegin = "shared_chat_begin"
	EvSharedChatEnd   = "shared_chat_end"
)

// Event categories: the Alerts tab groups kinds by them, in this order.
const (
	CatSupport = "support" // viewers giving: follows, subs, Bits, raids, ...
	CatHype    = "hype"    // hype trains and goals
	CatVotes   = "votes"   // polls and predictions
	CatChannel = "channel" // the channel itself: live, ads, shoutouts, ...
)

// EventCategories lists the categories in display order.
var EventCategories = []string{CatSupport, CatHype, CatVotes, CatChannel}

// eventCategory maps each kind in EventKinds to its category.
var eventCategory = map[string]string{
	EvFollow: CatSupport, EvUnfollow: CatSupport, EvSub: CatSupport, EvResub: CatSupport,
	EvGift: CatSupport, EvCheer: CatSupport, EvPowerUp: CatSupport, EvRaid: CatSupport,
	EvWatchStreak: CatSupport, EvSubEnd: CatSupport, EvAutoReward: CatSupport,
	EvHypeBegin: CatHype, EvHypeLevel: CatHype, EvHypeEnd: CatHype,
	EvShoutout: CatChannel, EvAdBreak: CatChannel, EvChannelUpdate: CatChannel, EvOnline: CatChannel, EvOffline: CatChannel,
}

// Category returns the category of an event kind.
func Category(kind string) string { return eventCategory[kind] }

// EventKinds lists every kind in the order the UI shows them.
var EventKinds = []string{EvFollow, EvUnfollow, EvSub, EvResub, EvGift, EvCheer, EvPowerUp, EvRaid, EvWatchStreak, EvAutoReward, EvHypeBegin, EvHypeLevel, EvHypeEnd, EvShoutout, EvAdBreak, EvChannelUpdate, EvSubEnd, EvOnline, EvOffline}

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
	// PowerUpType: power_up only, PowerUpCustom for the channel's own
	// Power-ups, else Twitch's power_up.type (e.g. "gigantify_an_emote").
	PowerUpType string `json:"power_up_type,omitempty"`
	// PowerUpTitle: the title of a custom Power-up (set by the streamer).
	PowerUpTitle string `json:"power_up_title,omitempty"`
	// StreamTitle and Category: channel_update, the streamer's own title
	// and the category (game) name.
	StreamTitle string `json:"stream_title,omitempty"`
	Category    string `json:"category,omitempty"`
	// Reward: auto_reward, Twitch's reward type (e.g.
	// "send_highlighted_message"); Amount is its price in channel points.
	Reward string `json:"reward,omitempty"`
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

// streamLive is stream.online's type for a live stream.
const streamLive = "live"

// flexInt reads a number Twitch documents as Integer but shows as a string
// in its own example ("duration_seconds": "60" in channel.ad_break.begin,
// checked 2026-11-19), so either form decodes.
type flexInt int

func (n *flexInt) UnmarshalJSON(b []byte) error {
	if string(b) == "null" {
		*n = 0 // as for a missing field
		return nil
	}
	if s, err := strconv.Unquote(string(b)); err == nil {
		b = []byte(s)
	}
	v, err := strconv.Atoi(string(b))
	if err != nil {
		return fmt.Errorf("not a whole number: %s", b)
	}
	*n = flexInt(v)
	return nil
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

// channelTracker drops channel.update notifications in which neither the
// title nor the category changed (e.g. only the language). It is seeded
// with the channel as it is when darkbot connects.
type channelTracker struct {
	mu              sync.Mutex
	known           bool
	title, category string
}

func (c *channelTracker) seed(title, category string) {
	c.mu.Lock()
	c.known, c.title, c.category = true, title, category
	c.mu.Unlock()
}

// filter returns false for a channel update that changed nothing darkbot
// reports; other events pass.
func (c *channelTracker) filter(ev ChannelEvent) bool {
	if ev.Kind != EvChannelUpdate {
		return true
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	same := c.known && ev.StreamTitle == c.title && ev.Category == c.category
	c.known, c.title, c.category = true, ev.StreamTitle, ev.Category
	return !same
}

// seed starts a hype train already running when darkbot connects, so its
// next level-up is reported (not taken as the baseline).
func (h *hypeTracker) seed(train string, level int) {
	h.mu.Lock()
	h.train, h.level = train, level
	h.mu.Unlock()
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
	// channel.bits.use replaces channel.cheer: it reports cheers too, plus
	// Bits spent on Power-ups (subscribing to both would count cheers
	// twice). It has no is_anonymous field.
	{Type: "channel.bits.use", Version: "1", Scope: ScopeReadBits, condition: byBroadcaster, parse: parseBitsUse},
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
				Seconds flexInt `json:"duration_seconds"`
			}](raw)
			return ChannelEvent{Kind: EvAdBreak, Amount: int(v.Seconds)}, err
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
	// channel.update v2 (no authorization needed, checked 2026-11-19):
	// "when a broadcaster updates the category, title, content
	// classification labels, or broadcast language". channelTracker keeps
	// only title and category changes.
	{Type: "channel.update", Version: "2", Scope: "", condition: byBroadcaster,
		parse: func(raw json.RawMessage) (ChannelEvent, error) {
			v, err := decode[struct {
				Title    string `json:"title"`
				Category string `json:"category_name"`
			}](raw)
			return ChannelEvent{Kind: EvChannelUpdate, StreamTitle: v.Title, Category: v.Category}, err
		}},
	// Automatic rewards v2 (checked 2026-11-19): reward.type is
	// single_message_bypass_sub_mode, send_highlighted_message,
	// random_sub_emote_unlock, chosen_sub_emote_unlock or
	// chosen_modified_sub_emote_unlock; reward.channel_points the cost.
	// The viewer's message is not used (darkbot never repeats viewer text).
	{Type: "channel.channel_points_automatic_reward_redemption.add", Version: "2", Scope: ScopeReadRedemptions, condition: byBroadcaster,
		parse: func(raw json.RawMessage) (ChannelEvent, error) {
			v, err := decode[struct {
				user
				Reward struct {
					Type   string `json:"type"`
					Points int    `json:"channel_points"`
				} `json:"reward"`
			}](raw)
			return ChannelEvent{Kind: EvAutoReward, User: v.name(), UserID: v.id(), Amount: v.Reward.Points, Reward: v.Reward.Type}, err
		}},
	{Type: "channel.shared_chat.begin", Version: "1", Scope: "", condition: byBroadcaster,
		parse: func(json.RawMessage) (ChannelEvent, error) { return ChannelEvent{Kind: EvSharedChatBegin}, nil }},
	{Type: "channel.shared_chat.end", Version: "1", Scope: "", condition: byBroadcaster,
		parse: func(json.RawMessage) (ChannelEvent, error) { return ChannelEvent{Kind: EvSharedChatEnd}, nil }},
	// stream.online "type" is live, playlist, watch_party, premiere or
	// rerun (checked 2026-11-19); only a live stream counts, as in
	// LiveSince (Get Streams type=live).
	{Type: "stream.online", Version: "1", Scope: "", condition: byBroadcaster,
		parse: func(raw json.RawMessage) (ChannelEvent, error) {
			v, err := decode[struct {
				Type string `json:"type"`
			}](raw)
			if err != nil || v.Type != streamLive {
				return ChannelEvent{}, err
			}
			return ChannelEvent{Kind: EvOnline}, nil
		}},
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

// channel.bits.use (https://dev.twitch.tv/docs/eventsub/reference/channel/,
// checked 2026-11-19): "type" is cheer, power_up or custom_power_up;
// "message" is null when there is none; power_up.type is message_effect,
// celebration or gigantify_an_emote; custom_power_up has title and
// reward_id.
const (
	bitsCheer         = "cheer"
	bitsPowerUp       = "power_up"
	bitsCustomPowerUp = "custom_power_up"
	// PowerUpCustom is ChannelEvent.PowerUpType for the channel's own
	// Power-ups.
	PowerUpCustom = "custom"
)

func parseBitsUse(raw json.RawMessage) (ChannelEvent, error) {
	v, err := decode[struct {
		user
		Bits    int    `json:"bits"`
		Type    string `json:"type"`
		Message *struct {
			Text string `json:"text"`
		} `json:"message"`
		PowerUp *struct {
			Type string `json:"type"`
		} `json:"power_up"`
		CustomPowerUp *struct {
			Title string `json:"title"`
		} `json:"custom_power_up"`
	}](raw)
	if err != nil {
		return ChannelEvent{}, err
	}
	ev := ChannelEvent{Kind: EvCheer, User: v.name(), UserID: v.id(), Amount: v.Bits}
	if v.Message != nil {
		ev.Message = v.Message.Text
	}
	switch v.Type {
	case bitsCheer:
	case bitsCustomPowerUp:
		ev.Kind, ev.PowerUpType = EvPowerUp, PowerUpCustom
		if v.CustomPowerUp != nil {
			ev.PowerUpTitle = v.CustomPowerUp.Title
		}
	case bitsPowerUp:
		ev.Kind = EvPowerUp
		if v.PowerUp != nil {
			ev.PowerUpType = v.PowerUp.Type
		}
	default:
		// "an all-purpose event" that may grow: a new kind of Bits use is
		// still Bits the viewer spent, shown as a Power-up of unknown type.
		ev.Kind = EvPowerUp
	}
	return ev, nil
}

package twitch

import (
	"context"
	"net/http"
	"net/url"
	"time"
)

// Channel totals for the dashboard (verified 2026-11-18):
//   - Get Channel Followers, https://dev.twitch.tv/docs/api/reference/channels:
//     "total": the number of users that follow the broadcaster.
//   - Get Broadcaster Subscriptions, https://dev.twitch.tv/docs/api/reference/subscriptions:
//     "total" subscribers and "points" (tier 1 = 1, tier 2 = 2, tier 3 = 6;
//     they unlock emote slots). Needs channel:read:subscriptions.
//
// Both are read with first=1: only the totals are needed, not the lists.
// There is no channel-wide Bits total in the API (the Bits leaderboard is
// per user), so Bits stay counted by darkbot.

// LiveSince reports whether the broadcaster is streaming now and since
// when. Get Streams (https://dev.twitch.tv/docs/api/reference/streams,
// checked 2026-11-18) needs no scope and "Returns only the streams of
// those users that are broadcasting", so an offline channel is an empty
// list; started_at is "The UTC date and time ... of when the broadcast
// began".
func (h *Helix) LiveSince(ctx context.Context, broadcasterID string) (live bool, since time.Time, err error) {
	var out struct {
		Data []struct {
			StartedAt time.Time `json:"started_at"`
		} `json:"data"`
	}
	q := url.Values{"user_id": {broadcasterID}, "type": {"live"}}
	if err := h.do(ctx, http.MethodGet, "/streams", q, nil, &out); err != nil {
		return false, time.Time{}, err
	}
	if len(out.Data) == 0 {
		return false, time.Time{}, nil
	}
	return true, out.Data[0].StartedAt, nil
}

// GetFollowerCount returns how many users follow the broadcaster.
func (h *Helix) GetFollowerCount(ctx context.Context, broadcasterID string) (int, error) {
	var out struct {
		Total int `json:"total"`
	}
	q := url.Values{"broadcaster_id": {broadcasterID}, "first": {"1"}}
	if err := h.do(ctx, http.MethodGet, "/channels/followers", q, nil, &out); err != nil {
		return 0, err
	}
	return out.Total, nil
}

// GetSubscriptionTotals returns the number of subscribers and the
// subscriber points of the broadcaster.
func (h *Helix) GetSubscriptionTotals(ctx context.Context, broadcasterID string) (total, points int, err error) {
	var out struct {
		Total  int `json:"total"`
		Points int `json:"points"`
	}
	q := url.Values{"broadcaster_id": {broadcasterID}, "first": {"1"}}
	if err := h.do(ctx, http.MethodGet, "/subscriptions", q, nil, &out); err != nil {
		return 0, 0, err
	}
	return out.Total, out.Points, nil
}

// Shared Chat (checked 2026-11-19): while a channel is in a Shared Chat
// session, every message sent with a user access token "will be sent to
// all channels in the shared chat session ... This behavior cannot be
// changed with this token type" (Send Chat Message). Get Shared Chat
// Session "Returns an empty array if the broadcaster_id ... isn't in an
// active shared chat session"; channel.shared_chat.begin/end v1 need no
// authorization.

// InSharedChat reports whether the broadcaster is in a Shared Chat session.
func (h *Helix) InSharedChat(ctx context.Context, broadcasterID string) (bool, error) {
	var out struct {
		Data []struct {
			SessionID string `json:"session_id"`
		} `json:"data"`
	}
	q := url.Values{"broadcaster_id": {broadcasterID}}
	if err := h.do(ctx, http.MethodGet, "/shared_chat/session", q, nil, &out); err != nil {
		return false, err
	}
	return len(out.Data) > 0, nil
}

// ChannelInfo is the part of Get Channel Information darkbot uses.
type ChannelInfo struct {
	Title    string `json:"title"`
	Category string `json:"game_name"`
}

// ChannelInfo reads the stream title and category (Get Channel
// Information, any user token, checked 2026-11-19).
func (h *Helix) ChannelInfo(ctx context.Context, broadcasterID string) (ChannelInfo, error) {
	var out struct {
		Data []ChannelInfo `json:"data"`
	}
	if err := h.do(ctx, http.MethodGet, "/channels", url.Values{"broadcaster_id": {broadcasterID}}, nil, &out); err != nil {
		return ChannelInfo{}, err
	}
	if len(out.Data) == 0 {
		return ChannelInfo{}, ErrNotFound
	}
	return out.Data[0], nil
}

// HypeTrainNow returns the running hype train's ID and level ("" when none
// runs). Get Hype Train Status (channel:read:hype_train, checked
// 2026-11-19): data[].current is "Null if a Hype Train is not active".
func (h *Helix) HypeTrainNow(ctx context.Context, broadcasterID string) (train string, level int, err error) {
	var out struct {
		Data []struct {
			Current *struct {
				ID    string `json:"id"`
				Level int    `json:"level"`
			} `json:"current"`
		} `json:"data"`
	}
	if err := h.do(ctx, http.MethodGet, "/hypetrain/status", url.Values{"broadcaster_id": {broadcasterID}}, nil, &out); err != nil {
		return "", 0, err
	}
	if len(out.Data) == 0 || out.Data[0].Current == nil {
		return "", 0, nil
	}
	return out.Data[0].Current.ID, out.Data[0].Current.Level, nil
}

package twitch

import (
	"context"
	"net/http"
	"net/url"
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

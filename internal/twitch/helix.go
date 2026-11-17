package twitch

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// Helix reference (verified 2026-11-16):
//   - Get Custom Reward: https://dev.twitch.tv/docs/api/reference/channel-points
//   - Create EventSub Subscription: https://dev.twitch.tv/docs/api/reference/eventsub
//
// helixBase is a variable only so tests can use a fake server.
var helixBase = "https://api.twitch.tv/helix"

const helixTimeout = 15 * time.Second

// ErrNotAffiliate means the channel has no channel points (Helix 403).
var ErrNotAffiliate = errors.New("channel points need a Twitch Affiliate or Partner account")

// ErrNotFound is a Helix 404 (e.g. the reward was deleted on Twitch).
var ErrNotFound = errors.New("not found on Twitch")

// ErrNotOwner means the reward was created by another app (Helix 403 with a
// Client-Id mismatch), so darkbot may not change it.
var ErrNotOwner = errors.New("this reward was created outside darkbot, so darkbot can't change it")

// Helix is a minimal Helix client that refreshes the token on 401.
type Helix struct {
	tokens *TokenManager
	http   *http.Client
}

// NewHelix creates a Helix client.
func NewHelix(tokens *TokenManager) *Helix {
	return &Helix{tokens: tokens, http: &http.Client{Timeout: helixTimeout}}
}

// do sends a request; on 401 it refreshes the token once and retries.
func (h *Helix) do(ctx context.Context, method, path string, query url.Values, body any, out any) error {
	var payload []byte
	if body != nil {
		var err error
		if payload, err = json.Marshal(body); err != nil {
			return err
		}
	}

	for attempt := 0; attempt < 2; attempt++ {
		access, err := h.tokens.AccessToken()
		if err != nil {
			return err
		}
		u := helixBase + path
		if len(query) > 0 {
			u += "?" + query.Encode()
		}
		req, err := http.NewRequestWithContext(ctx, method, u, bytes.NewReader(payload))
		if err != nil {
			return err
		}
		req.Header.Set("Authorization", "Bearer "+access)
		req.Header.Set("Client-Id", h.tokens.ClientID())
		if body != nil {
			req.Header.Set("Content-Type", "application/json")
		}

		resp, err := h.http.Do(req)
		if err != nil {
			return fmt.Errorf("%s %s: %w", method, path, err)
		}
		if resp.StatusCode == http.StatusUnauthorized && attempt == 0 {
			resp.Body.Close()
			if err := h.tokens.RefreshIfStale(ctx, access); err != nil {
				return err
			}
			continue
		}
		defer resp.Body.Close()

		if resp.StatusCode < 200 || resp.StatusCode > 299 {
			te := readTwitchError(resp)
			switch {
			case resp.StatusCode == http.StatusNotFound:
				return fmt.Errorf("%w: %s", ErrNotFound, te.Message)
			case resp.StatusCode == http.StatusForbidden && strings.Contains(strings.ToLower(te.Message), "client-id"):
				return ErrNotOwner
			case resp.StatusCode == http.StatusForbidden && strings.Contains(strings.ToLower(te.Message), "partner"):
				return ErrNotAffiliate
			}
			return fmt.Errorf("%s %s: status %d: %s", method, path, te.Status, te.Message)
		}
		if out == nil {
			_, _ = io.Copy(io.Discard, resp.Body)
			return nil
		}
		if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
			return fmt.Errorf("parse %s response: %w", path, err)
		}
		return nil
	}
	return fmt.Errorf("%s %s: unauthorized after token refresh", method, path)
}

// Reward is a channel point custom reward (only the fields darkbot shows).
type Reward struct {
	ID        string `json:"id"`
	Title     string `json:"title"`
	Cost      int    `json:"cost"`
	IsEnabled bool   `json:"is_enabled"`
}

// GetCustomRewards lists all custom rewards of the broadcaster.
// Needs channel:read:redemptions; the token user must be the broadcaster.
func (h *Helix) GetCustomRewards(ctx context.Context, broadcasterID string) ([]Reward, error) {
	var out struct {
		Data []Reward `json:"data"`
	}
	q := url.Values{"broadcaster_id": {broadcasterID}}
	if err := h.do(ctx, http.MethodGet, "/channel_points/custom_rewards", q, nil, &out); err != nil {
		return nil, err
	}
	return out.Data, nil
}

// SubscribeRedemptions creates the
// channel.channel_points_custom_reward_redemption.add subscription (version 1)
// on the given EventSub WebSocket session. reward_id is omitted so we receive
// redemptions of ALL rewards and filter locally.
func (h *Helix) SubscribeRedemptions(ctx context.Context, sessionID, broadcasterID string) error {
	body := map[string]any{
		"type":    "channel.channel_points_custom_reward_redemption.add",
		"version": "1",
		"condition": map[string]string{
			"broadcaster_user_id": broadcasterID,
		},
		"transport": map[string]string{
			"method":     "websocket",
			"session_id": sessionID,
		},
	}
	return h.do(ctx, http.MethodPost, "/eventsub/subscriptions", nil, body, nil)
}

// RewardSettings are the reward fields darkbot manages for rewards it created.
// Docs: https://dev.twitch.tv/docs/api/reference/channel-points (verified 2026-11-17)
type RewardSettings struct {
	Title   string
	Cost    int
	Enabled bool
	// CooldownSeconds > 0 enables Twitch's global cooldown (shown to viewers
	// on Twitch when >= 60 s).
	CooldownSeconds int
}

// Reward limits from the Twitch reference.
const (
	MaxRewardTitle = 45
	MinRewardCost  = 1
	MaxRewardCost  = 1_000_000_000
	MaxRewards     = 50
)

func (r RewardSettings) body() map[string]any {
	// Limit fields are sent as pairs; omitting one resets it (docs).
	return map[string]any{
		"title":                                 r.Title,
		"cost":                                  r.Cost,
		"is_enabled":                            r.Enabled,
		"is_global_cooldown_enabled":            r.CooldownSeconds > 0,
		"global_cooldown_seconds":               max(r.CooldownSeconds, 0),
		"should_redemptions_skip_request_queue": false, // keep UNFULFILLED so we can fulfill or reject
	}
}

// CreateReward creates a custom reward owned by darkbot's Client ID.
func (h *Helix) CreateReward(ctx context.Context, broadcasterID string, r RewardSettings) (*Reward, error) {
	var out struct {
		Data []Reward `json:"data"`
	}
	q := url.Values{"broadcaster_id": {broadcasterID}}
	if err := h.do(ctx, http.MethodPost, "/channel_points/custom_rewards", q, r.body(), &out); err != nil {
		return nil, err
	}
	if len(out.Data) == 0 {
		return nil, errors.New("create reward: empty response")
	}
	return &out.Data[0], nil
}

// UpdateReward updates a reward darkbot created.
func (h *Helix) UpdateReward(ctx context.Context, broadcasterID, rewardID string, r RewardSettings) error {
	q := url.Values{"broadcaster_id": {broadcasterID}, "id": {rewardID}}
	return h.do(ctx, http.MethodPatch, "/channel_points/custom_rewards", q, r.body(), nil)
}

// DeleteReward deletes a reward darkbot created. A reward that is already
// gone (404) counts as deleted.
func (h *Helix) DeleteReward(ctx context.Context, broadcasterID, rewardID string) error {
	q := url.Values{"broadcaster_id": {broadcasterID}, "id": {rewardID}}
	err := h.do(ctx, http.MethodDelete, "/channel_points/custom_rewards", q, nil, nil)
	if errors.Is(err, ErrNotFound) {
		return nil
	}
	return err
}

// Redemption statuses for UpdateRedemptionStatus.
const (
	StatusFulfilled = "FULFILLED"
	StatusCanceled  = "CANCELED"
)

// UpdateRedemptionStatus fulfills or rejects one UNFULFILLED redemption of a
// reward darkbot created.
func (h *Helix) UpdateRedemptionStatus(ctx context.Context, broadcasterID, rewardID, redemptionID, status string) error {
	q := url.Values{"broadcaster_id": {broadcasterID}, "reward_id": {rewardID}, "id": {redemptionID}}
	return h.do(ctx, http.MethodPatch, "/channel_points/custom_rewards/redemptions", q, map[string]string{"status": status}, nil)
}

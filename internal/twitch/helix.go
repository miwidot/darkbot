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
			if resp.StatusCode == http.StatusForbidden && path == "/channel_points/custom_rewards" {
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

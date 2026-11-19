package twitch

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"time"
	"unicode/utf8"
)

// Calls that act on the channel (checked 2026-11-19 against
// https://dev.twitch.tv/docs/api/reference/):
//   - Get Ad Schedule: GET /channels/ads, channel:read:ads. next_ad_at
//     "Is 0 if the channel has no ad scheduled or is not live".
//   - Snooze Next Ad: POST /channels/ads/schedule/snooze, channel:manage:ads,
//     "pushes back ... the upcoming automatic mid-roll ad by 5 minutes".
//     400: not live or no ad scheduled; 429: "Channel has no snoozes left"
//     or another ad/snooze request is in progress.
//   - Send a Shoutout: POST /chat/shoutouts, moderator:manage:shoutouts.
//     "The broadcaster may send a Shoutout once every 2 minutes. They may
//     send the same broadcaster a Shoutout once every 60 minutes." 400 when
//     not live.
//   - Create Stream Marker: POST /streams/markers, channel:manage:broadcast,
//     description max 140 characters. 404 when not live, VOD off, or "in
//     the first few seconds of a stream"; 400 for reruns and premieres.
//   - Get Creator Goals: GET /goals, channel:read:goals.
const (
	ScopeManageAds       = "channel:manage:ads"
	ScopeManageShoutouts = "moderator:manage:shoutouts"
	ScopeManageBroadcast = "channel:manage:broadcast"
	ScopeReadGoals       = "channel:read:goals"
	ScopeReadPolls       = "channel:read:polls"
	ScopeReadPredictions = "channel:read:predictions"
	// MaxMarkerDescription: Create Stream Marker's description limit.
	MaxMarkerDescription = 140
)

// Errors of the calls above, by the cases the reference lists.
var (
	ErrNotLive     = errors.New("the stream is not live")
	ErrNoSnoozes   = errors.New("no ad snoozes left (or another ad request is running)")
	ErrNoAdPlanned = errors.New("no ad is scheduled")
	ErrMarkerLater = errors.New("markers need a live stream with VODs on, and not in its first seconds")
	ErrRateLimited = errors.New("Twitch's limit for this was reached, try later")
)

// AdSchedule is the channel's next ad, from Get Ad Schedule.
type AdSchedule struct {
	// NextAd is zero when no ad is scheduled or the channel is not live.
	NextAd   time.Time
	Duration int // seconds
	Snoozes  int
	// SnoozeRefresh: when the next snooze is added (zero = all there).
	SnoozeRefresh time.Time
}

// unixTime turns Twitch's Unix seconds (0 = none) into a time.
func unixTime(s flexInt) time.Time {
	if s <= 0 {
		return time.Time{}
	}
	return time.Unix(int64(s), 0)
}

type adScheduleJSON struct {
	NextAdAt        flexInt `json:"next_ad_at"`
	Duration        flexInt `json:"duration"`
	SnoozeCount     flexInt `json:"snooze_count"`
	SnoozeRefreshAt flexInt `json:"snooze_refresh_at"`
}

func (a adScheduleJSON) schedule() AdSchedule {
	return AdSchedule{NextAd: unixTime(a.NextAdAt), Duration: int(a.Duration), Snoozes: int(a.SnoozeCount), SnoozeRefresh: unixTime(a.SnoozeRefreshAt)}
}

// AdScheduleNow reads the next scheduled ad.
func (h *Helix) AdScheduleNow(ctx context.Context, broadcasterID string) (AdSchedule, error) {
	var out struct {
		Data []adScheduleJSON `json:"data"`
	}
	if err := h.do(ctx, http.MethodGet, "/channels/ads", url.Values{"broadcaster_id": {broadcasterID}}, nil, &out); err != nil {
		return AdSchedule{}, err
	}
	if len(out.Data) == 0 {
		return AdSchedule{}, nil
	}
	return out.Data[0].schedule(), nil
}

// SnoozeAd pushes the next automatic ad back by 5 minutes.
func (h *Helix) SnoozeAd(ctx context.Context, broadcasterID string) (AdSchedule, error) {
	var out struct {
		Data []adScheduleJSON `json:"data"`
	}
	err := h.do(ctx, http.MethodPost, "/channels/ads/schedule/snooze", url.Values{"broadcaster_id": {broadcasterID}}, nil, &out)
	if e, ok := errors.AsType[*APIError](err); ok {
		switch e.Status {
		case http.StatusTooManyRequests:
			return AdSchedule{}, errors.Join(ErrNoSnoozes, err)
		case http.StatusBadRequest:
			return AdSchedule{}, errors.Join(ErrNoAdPlanned, err)
		}
	}
	if err != nil {
		return AdSchedule{}, err
	}
	if len(out.Data) == 0 {
		return AdSchedule{}, nil
	}
	return out.Data[0].schedule(), nil
}

// Shoutout gives another broadcaster Twitch's shoutout, as the broadcaster.
func (h *Helix) Shoutout(ctx context.Context, broadcasterID, toID string) error {
	q := url.Values{"from_broadcaster_id": {broadcasterID}, "to_broadcaster_id": {toID}, "moderator_id": {broadcasterID}}
	err := h.do(ctx, http.MethodPost, "/chat/shoutouts", q, nil, nil)
	if e, ok := errors.AsType[*APIError](err); ok {
		switch e.Status {
		case http.StatusTooManyRequests:
			return errors.Join(ErrRateLimited, err)
		case http.StatusBadRequest:
			return errors.Join(ErrNotLive, err) // or a shoutout to oneself, which darkbot never sends
		}
	}
	return err
}

// Marker is a new stream marker.
type Marker struct {
	// Position is the offset from the start of the stream.
	Position time.Duration
}

// CreateMarker marks the current moment of the live stream (description is
// cut to MaxMarkerDescription).
func (h *Helix) CreateMarker(ctx context.Context, broadcasterID, description string) (Marker, error) {
	if utf8.RuneCountInString(description) > MaxMarkerDescription {
		description = string([]rune(description)[:MaxMarkerDescription])
	}
	body := map[string]string{"user_id": broadcasterID}
	if description != "" {
		body["description"] = description
	}
	var out struct {
		Data []struct {
			Position flexInt `json:"position_seconds"`
		} `json:"data"`
	}
	err := h.do(ctx, http.MethodPost, "/streams/markers", nil, body, &out)
	if errors.Is(err, ErrNotFound) {
		return Marker{}, errors.Join(ErrMarkerLater, err)
	}
	if e, ok := errors.AsType[*APIError](err); ok && e.Status == http.StatusBadRequest {
		return Marker{}, errors.Join(ErrMarkerLater, err) // rerun or premiere
	}
	if err != nil {
		return Marker{}, err
	}
	if len(out.Data) == 0 {
		return Marker{}, errors.New("create marker: empty response")
	}
	return Marker{Position: time.Duration(out.Data[0].Position) * time.Second}, nil
}

// Goal is a creator goal (Get Creator Goals and the channel.goal events).
type Goal struct {
	ID          string `json:"id"`
	Type        string `json:"type"` // GoalFollow, GoalSubs, ... (normalized)
	Description string `json:"description"`
	Current     int    `json:"current"`
	Target      int    `json:"target"`
}

// Goal types. Get Creator Goals says "follower" where the channel.goal
// events say "follow" (both checked 2026-11-19); both become GoalFollow.
const (
	GoalFollow        = "follow"
	goalFollowerHelix = "follower"
)

func normalizeGoalType(t string) string {
	if t == goalFollowerHelix {
		return GoalFollow
	}
	return t
}

// Goals reads the broadcaster's active goals.
func (h *Helix) Goals(ctx context.Context, broadcasterID string) ([]Goal, error) {
	var out struct {
		Data []struct {
			ID          string `json:"id"`
			Type        string `json:"type"`
			Description string `json:"description"`
			Current     int    `json:"current_amount"`
			Target      int    `json:"target_amount"`
		} `json:"data"`
	}
	if err := h.do(ctx, http.MethodGet, "/goals", url.Values{"broadcaster_id": {broadcasterID}}, nil, &out); err != nil {
		return nil, err
	}
	goals := make([]Goal, 0, len(out.Data))
	for _, g := range out.Data {
		goals = append(goals, Goal{ID: g.ID, Type: normalizeGoalType(g.Type), Description: g.Description, Current: g.Current, Target: g.Target})
	}
	return goals, nil
}

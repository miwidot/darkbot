package twitch

import (
	"context"
	"errors"
	"net/http"
	"testing"
	"time"
)

func helixFor(t *testing.T, h http.HandlerFunc) *Helix {
	t.Helper()
	fakeTwitch(t, h)
	m, _ := NewTokenManager("cid", &memStore{tok: &Token{AccessToken: "A1", RefreshToken: "R1", UserID: "42"}}, nopLog{})
	return NewHelix(m)
}

func TestAdSchedule(t *testing.T) {
	asStrings := false
	h := helixFor(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/helix/channels/ads" || r.URL.Query().Get("broadcaster_id") != "42" {
			http.NotFound(w, r)
			return
		}
		// Shaped like the reference's example; numbers as strings are read
		// too (flexInt), as other Twitch examples send them that way.
		next := any(1690931298)
		if asStrings {
			next = "1690931298"
		}
		writeJSON(w, 200, map[string]any{"data": []map[string]any{{"next_ad_at": next, "last_ad_at": 0, "duration": 60, "preroll_free_time": 90, "snooze_count": 1, "snooze_refresh_at": 0}}})
	})
	for _, str := range []bool{false, true} {
		asStrings = str
		s, err := h.AdScheduleNow(context.Background(), "42")
		if err != nil || !s.NextAd.Equal(time.Unix(1690931298, 0)) || s.Duration != 60 || s.Snoozes != 1 || !s.SnoozeRefresh.IsZero() {
			t.Fatalf("strings=%v: %+v %v", str, s, err)
		}
	}
}

func TestSnoozeErrors(t *testing.T) {
	status := 429
	h := helixFor(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/helix/channels/ads/schedule/snooze" {
			http.NotFound(w, r)
			return
		}
		if status != 200 {
			writeJSON(w, status, map[string]any{"status": status, "message": "x"})
			return
		}
		writeJSON(w, 200, map[string]any{"data": []map[string]any{{"snooze_count": 2, "snooze_refresh_at": 1800000600, "next_ad_at": 1800000300}}})
	})
	if _, err := h.SnoozeAd(context.Background(), "42"); !errors.Is(err, ErrNoSnoozes) {
		t.Errorf("429: %v", err)
	}
	status = 400
	if _, err := h.SnoozeAd(context.Background(), "42"); !errors.Is(err, ErrNoAdPlanned) {
		t.Errorf("400: %v", err)
	}
	status = 200
	if s, err := h.SnoozeAd(context.Background(), "42"); err != nil || s.Snoozes != 2 || !s.NextAd.Equal(time.Unix(1800000300, 0)) {
		t.Errorf("ok: %+v %v", s, err)
	}
}

func TestShoutoutAndMarker(t *testing.T) {
	var got string
	h := helixFor(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/helix/chat/shoutouts":
			q := r.URL.Query()
			got = q.Get("from_broadcaster_id") + ">" + q.Get("to_broadcaster_id") + "/" + q.Get("moderator_id")
			w.WriteHeader(http.StatusNoContent)
		case "/helix/streams/markers":
			writeJSON(w, 200, map[string]any{"data": []map[string]any{{"id": "m1", "created_at": "2026-11-19T10:00:00Z", "description": "x", "position_seconds": 244}}})
		default:
			http.NotFound(w, r)
		}
	})
	if err := h.Shoutout(context.Background(), "42", "7"); err != nil || got != "42>7/42" {
		t.Fatalf("shoutout %q %v", got, err)
	}
	if m, err := h.CreateMarker(context.Background(), "42", "gg"); err != nil || m.Position != 244*time.Second {
		t.Fatalf("marker %+v %v", m, err)
	}
}

func TestMarkerNotLive(t *testing.T) {
	h := helixFor(t, func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, 404, map[string]any{"status": 404, "message": "user is not streaming live"})
	})
	if _, err := h.CreateMarker(context.Background(), "42", ""); !errors.Is(err, ErrMarkerLater) {
		t.Fatal(err)
	}
}

func TestGoalsNormalizesFollower(t *testing.T) {
	h := helixFor(t, func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, 200, map[string]any{"data": []map[string]any{{"id": "g1", "type": "follower", "description": "Follow goal for Helix testing", "current_amount": 27062, "target_amount": 30000, "created_at": "2021-08-16T17:22:23Z"}}})
	})
	g, err := h.Goals(context.Background(), "42")
	if err != nil || len(g) != 1 || g[0].Type != GoalFollow || g[0].Current != 27062 || g[0].Target != 30000 {
		t.Fatalf("%+v %v", g, err)
	}
}

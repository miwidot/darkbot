package twitch

import (
	"context"
	"errors"
	"net/http"
	"testing"
)

func TestCreateClip(t *testing.T) {
	status := http.StatusAccepted
	fakeTwitch(t, func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		if r.Method != http.MethodPost || r.URL.Path != "/helix/clips" || q.Get("broadcaster_id") != "42" || q.Get("duration") != "60" {
			writeJSON(w, 400, map[string]any{"status": 400, "message": "unexpected request " + r.URL.String()})
			return
		}
		if status != http.StatusAccepted {
			writeJSON(w, status, map[string]any{"status": status, "message": "nope"})
			return
		}
		// The docs' example response.
		writeJSON(w, status, map[string]any{"data": []map[string]any{{"id": "FiveWordsForClipSlug", "edit_url": "https://www.twitch.tv/twitchdev/clip/FiveWordsForClipSlug"}}})
	})
	m, _ := NewTokenManager("cid", &memStore{tok: &Token{AccessToken: "A1", RefreshToken: "R1", UserID: "42"}}, nopLog{})
	h := NewHelix(m)
	c, err := h.CreateClip(context.Background(), "42", "", MaxClipSeconds)
	if err != nil || c.ID != "FiveWordsForClipSlug" {
		t.Fatalf("create: %+v %v", c, err)
	}
	for _, tt := range []struct {
		status int
		want   error
	}{
		{http.StatusNotFound, ErrClipOffline},
		{http.StatusBadRequest, ErrClipNotClippable},
		{http.StatusForbidden, ErrClipNotAllowed},
	} {
		status = tt.status
		if _, err := h.CreateClip(context.Background(), "42", "", MaxClipSeconds); !errors.Is(err, tt.want) {
			t.Errorf("status %d: %v, want %v", tt.status, err, tt.want)
		}
	}
	if _, err := h.CreateClip(context.Background(), "42", "", MaxClipSeconds+1); err == nil {
		t.Error("a 61 s clip was accepted")
	}
}

func TestCreateClipTitle(t *testing.T) {
	var got string
	fakeTwitch(t, func(w http.ResponseWriter, r *http.Request) {
		got = r.URL.Query().Get("title")
		writeJSON(w, 202, map[string]any{"data": []map[string]any{{"id": "c1"}}})
	})
	m, _ := NewTokenManager("cid", &memStore{tok: &Token{AccessToken: "A1", RefreshToken: "R1", UserID: "42"}}, nopLog{})
	if _, err := NewHelix(m).CreateClip(context.Background(), "42", "Clip by Anna & co", 30); err != nil || got != "Clip by Anna & co" {
		t.Fatalf("title %q, %v", got, err)
	}
}

func TestGetClip(t *testing.T) {
	fakeTwitch(t, func(w http.ResponseWriter, r *http.Request) {
		data := []map[string]any{}
		if r.URL.Path == "/helix/clips" && r.URL.Query().Get("id") == "ready" {
			data = append(data, map[string]any{"id": "ready", "url": "https://clips.twitch.tv/ready", "title": "t"})
		}
		writeJSON(w, 200, map[string]any{"data": data, "pagination": map[string]any{}})
	})
	m, _ := NewTokenManager("cid", &memStore{tok: &Token{AccessToken: "A1", RefreshToken: "R1", UserID: "42"}}, nopLog{})
	h := NewHelix(m)
	if c, ok, err := h.GetClip(context.Background(), "ready"); err != nil || !ok || c.URL != "https://clips.twitch.tv/ready" {
		t.Fatalf("ready: %+v %v %v", c, ok, err)
	}
	// Not created yet: Get Clips ignores unknown IDs.
	if _, ok, err := h.GetClip(context.Background(), "pending"); err != nil || ok {
		t.Fatalf("pending: %v %v", ok, err)
	}
}

func TestScopesIncludeClips(t *testing.T) {
	if !(&Token{Scopes: Scopes}).HasScope(ScopeEditClips) {
		t.Fatal("clips:edit is not requested at login")
	}
}

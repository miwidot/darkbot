package main

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/miwi/darkbot/internal/config"
)

func TestServeSoundOnlyServesConfiguredFiles(t *testing.T) {
	dir := t.TempDir()
	snd := filepath.Join(dir, "airhorn.mp3")
	secret := filepath.Join(dir, "secret.txt")
	if err := os.WriteFile(snd, []byte("ID3fake"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(secret, []byte("token"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := cfg.Update(func(c *config.Config) {
		c.Sounds = []config.Sound{{ID: "abc", RewardTitle: "Airhorn", File: snd}}
	}); err != nil {
		t.Fatal(err)
	}
	a := &App{cfg: cfg}

	tests := []struct {
		method, path string
		want         int
		body         string
	}{
		{http.MethodGet, "/sound/abc", http.StatusOK, "ID3fake"},
		{http.MethodGet, "/sound/nope", http.StatusNotFound, ""},
		{http.MethodGet, "/sound/../secret.txt", http.StatusNotFound, ""},
		{http.MethodGet, "/sound/" + secret, http.StatusNotFound, ""},
		{http.MethodGet, "/other", http.StatusNotFound, ""},
		{http.MethodPost, "/sound/abc", http.StatusNotFound, ""},
	}
	for _, tt := range tests {
		rec := httptest.NewRecorder()
		a.serveSound(rec, httptest.NewRequest(tt.method, tt.path, nil))
		if rec.Code != tt.want {
			t.Errorf("%s %s: status %d, want %d", tt.method, tt.path, rec.Code, tt.want)
		}
		if tt.body != "" && rec.Body.String() != tt.body {
			t.Errorf("%s %s: body %q", tt.method, tt.path, rec.Body.String())
		}
		if rec.Code == http.StatusOK && rec.Header().Get("Content-Type") != "audio/mpeg" {
			t.Errorf("content type = %q", rec.Header().Get("Content-Type"))
		}
	}
}

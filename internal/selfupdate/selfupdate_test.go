package selfupdate

import (
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

func TestIsNewer(t *testing.T) {
	tests := []struct {
		cur, latest string
		want        bool
	}{
		{"0.1.0", "v0.2.0", true},
		{"0.2.0", "v0.2.0", false},
		{"0.2.0", "0.1.9", false},
		{"0.2.0-beta", "0.2.0", true},
		{"0.2.0", "0.2.1-alpha", true},
		{"0.2.0", "garbage", false},
		{"0.2.0", "0.10.0", true},
	}
	for _, tt := range tests {
		if got := IsNewer(tt.cur, tt.latest); got != tt.want {
			t.Errorf("IsNewer(%q,%q) = %v", tt.cur, tt.latest, got)
		}
	}
}

func TestAssetName(t *testing.T) {
	cases := map[[2]string]string{
		{"windows", "amd64"}: "darkbot-windows-amd64.exe",
		{"darwin", "arm64"}:  "darkbot-macos-arm64",
		{"linux", "amd64"}:   "darkbot-linux-amd64",
	}
	for in, want := range cases {
		if got := assetName(in[0], in[1]); got != want {
			t.Errorf("assetName(%v) = %q, want %q", in, got, want)
		}
	}
}

func setup(t *testing.T, payload []byte) (*Asset, string) {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write(payload)
	}))
	t.Cleanup(srv.Close)
	old := downloadPrefix
	downloadPrefix = srv.URL
	t.Cleanup(func() { downloadPrefix = old })

	sum := sha256.Sum256(payload)
	a := &Asset{
		Name:        "darkbot-test",
		DownloadURL: srv.URL + "/o/r/releases/download/v9.9.9/darkbot-test",
		Size:        int64(len(payload)),
		Digest:      "sha256:" + hex.EncodeToString(sum[:]),
	}
	exe := filepath.Join(t.TempDir(), "darkbot")
	if err := os.WriteFile(exe, []byte("old binary"), 0o755); err != nil {
		t.Fatal(err)
	}
	return a, exe
}

func TestApplySwapsBinary(t *testing.T) {
	a, exe := setup(t, []byte("new binary"))
	if err := applyTo(exe, "o", "r", a); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(exe); string(b) != "new binary" {
		t.Fatalf("exe = %q", b)
	}
	if b, _ := os.ReadFile(exe + ".old"); string(b) != "old binary" {
		t.Fatalf("old = %q", b)
	}
}

func TestApplyRejectsBadChecksum(t *testing.T) {
	a, exe := setup(t, []byte("new binary"))
	a.Digest = "sha256:" + hex.EncodeToString(make([]byte, sha256.Size))
	if err := applyTo(exe, "o", "r", a); err == nil {
		t.Fatal("expected checksum error")
	}
	if b, _ := os.ReadFile(exe); string(b) != "old binary" {
		t.Fatal("binary must stay untouched on failure")
	}
	if _, err := os.Stat(exe + ".new"); !os.IsNotExist(err) {
		t.Fatal("partial download must be removed")
	}
}

func TestApplyRejectsForeignURLAndMissingDigest(t *testing.T) {
	a, exe := setup(t, []byte("x"))
	foreign := *a
	foreign.DownloadURL = "https://evil.example/o/r/releases/download/v1/x"
	if err := applyTo(exe, "o", "r", &foreign); err == nil {
		t.Fatal("foreign URL must be refused")
	}
	noDigest := *a
	noDigest.Digest = ""
	if err := applyTo(exe, "o", "r", &noDigest); err == nil {
		t.Fatal("missing digest must be refused")
	}
	otherRepo := *a
	if err := applyTo(exe, "o", "other", &otherRepo); err == nil {
		t.Fatal("asset of another repo must be refused")
	}
}

// Package selfupdate updates darkbot from GitHub releases on Windows, macOS
// and Linux (ported from streamerchat, extended beyond Windows).
//
// Strategy (works for a running program on all three OSes):
//   - download the platform asset to <exe>.new and verify size + SHA-256
//   - rename the running <exe> to <exe>.old (allowed on Windows, and on
//     Unix the running process keeps its inode)
//   - rename <exe>.new to <exe>, start it, exit; the next start removes .old
//
// On macOS <exe> is the binary inside darkbot.app/Contents/MacOS, so the
// bundle stays in place. (Unsigned builds; replacing the binary of a signed
// bundle would break the signature.)
//
// Security model: the asset always comes from the GitHub API response for
// this repo (never from the UI), must live under this repo's
// release-download path and must match the SHA-256 digest GitHub computed
// on upload. Unverifiable assets are refused.
package selfupdate

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"
)

// apiBase/downloadPrefix are variables only so tests can use a fake server.
var (
	apiBase        = "https://api.github.com"
	downloadPrefix = "https://github.com"
)

var (
	apiClient = &http.Client{Timeout: 30 * time.Second}
	// downloadClient: only time-to-first-byte is tight; the whole body may
	// take long on slow connections.
	downloadClient = &http.Client{
		Timeout: 15 * time.Minute,
		Transport: &http.Transport{
			Proxy:                 http.ProxyFromEnvironment,
			ResponseHeaderTimeout: 30 * time.Second,
			TLSHandshakeTimeout:   15 * time.Second,
		},
	}
)

// Release is the subset of GitHub's release JSON we need.
type Release struct {
	TagName string  `json:"tag_name"`
	Name    string  `json:"name"`
	Body    string  `json:"body"`
	HTMLURL string  `json:"html_url"`
	Assets  []Asset `json:"assets"`
}

// Asset is one downloadable file of a release.
type Asset struct {
	Name        string `json:"name"`
	DownloadURL string `json:"browser_download_url"`
	Size        int64  `json:"size"`
	// Digest is "sha256:<hex>", computed by GitHub on upload.
	Digest string `json:"digest"`
}

// AssetName is the release file for the current platform, e.g.
// darkbot-windows-amd64.exe, darkbot-macos-arm64, darkbot-linux-amd64.
func AssetName() string { return assetName(runtime.GOOS, runtime.GOARCH) }

func assetName(goos, goarch string) string {
	osName, ext := goos, ""
	switch goos {
	case "darwin":
		osName = "macos"
	case "windows":
		ext = ".exe"
	}
	return fmt.Sprintf("darkbot-%s-%s%s", osName, goarch, ext)
}

// Latest returns the latest stable (non-prerelease) release.
func Latest(owner, repo string) (*Release, error) {
	url := fmt.Sprintf("%s/repos/%s/%s/releases/latest", apiBase, owner, repo)
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	resp, err := apiClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("check for update: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		return nil, errors.New("no releases published yet")
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("check for update: GitHub answered status %d", resp.StatusCode)
	}
	var rel Release
	if err := json.NewDecoder(resp.Body).Decode(&rel); err != nil {
		return nil, fmt.Errorf("parse release: %w", err)
	}
	return &rel, nil
}

// FindAsset returns the asset named name, or nil.
func FindAsset(rel *Release, name string) *Asset {
	if rel == nil {
		return nil
	}
	for i := range rel.Assets {
		if rel.Assets[i].Name == name {
			return &rel.Assets[i]
		}
	}
	return nil
}

// IsNewer reports whether latest > current (semver precedence, leading "v"
// allowed, "+build" ignored). Unparseable input is never newer.
func IsNewer(current, latest string) bool {
	a, okA := parseVersion(current)
	b, okB := parseVersion(latest)
	if !okA || !okB {
		return false
	}
	return compareVersions(b, a) > 0
}

type semver struct {
	core [3]int
	pre  []string
}

func parseVersion(s string) (semver, bool) {
	var v semver
	s = strings.TrimPrefix(strings.TrimSpace(s), "v")
	if i := strings.IndexByte(s, '+'); i >= 0 {
		s = s[:i]
	}
	core := s
	if i := strings.IndexByte(s, '-'); i >= 0 {
		core = s[:i]
		if s[i+1:] == "" {
			return v, false
		}
		v.pre = strings.Split(s[i+1:], ".")
	}
	parts := strings.Split(core, ".")
	if len(parts) != 3 {
		return v, false
	}
	for i, p := range parts {
		n, err := strconv.Atoi(p)
		if err != nil || n < 0 {
			return v, false
		}
		v.core[i] = n
	}
	return v, true
}

func compareVersions(a, b semver) int {
	for i := 0; i < 3; i++ {
		if a.core[i] != b.core[i] {
			if a.core[i] > b.core[i] {
				return 1
			}
			return -1
		}
	}
	switch {
	case a.pre == nil && b.pre == nil:
		return 0
	case a.pre == nil:
		return 1
	case b.pre == nil:
		return -1
	}
	for i := 0; i < len(a.pre) && i < len(b.pre); i++ {
		if c := compareIdent(a.pre[i], b.pre[i]); c != 0 {
			return c
		}
	}
	switch {
	case len(a.pre) > len(b.pre):
		return 1
	case len(a.pre) < len(b.pre):
		return -1
	}
	return 0
}

func compareIdent(x, y string) int {
	nx, errX := strconv.Atoi(x)
	ny, errY := strconv.Atoi(y)
	switch {
	case errX == nil && errY == nil:
		switch {
		case nx > ny:
			return 1
		case nx < ny:
			return -1
		}
		return 0
	case errX == nil:
		return -1
	case errY == nil:
		return 1
	}
	return strings.Compare(x, y)
}

// validateAsset checks an asset before any byte is downloaded.
func validateAsset(owner, repo string, a *Asset) (wantHash string, err error) {
	if a == nil {
		return "", errors.New("no update file for this platform")
	}
	prefix := fmt.Sprintf("%s/%s/%s/releases/download/", downloadPrefix, owner, repo)
	if !strings.HasPrefix(a.DownloadURL, prefix) {
		return "", fmt.Errorf("refusing update from unexpected URL %q", a.DownloadURL)
	}
	const algo = "sha256:"
	if !strings.HasPrefix(a.Digest, algo) {
		return "", fmt.Errorf("release file %s has no sha256 digest, refusing unverifiable update", a.Name)
	}
	h := strings.ToLower(strings.TrimPrefix(a.Digest, algo))
	if b, err := hex.DecodeString(h); err != nil || len(b) != sha256.Size {
		return "", fmt.Errorf("release file %s has a malformed digest %q", a.Name, a.Digest)
	}
	if a.Size <= 0 {
		return "", fmt.Errorf("release file %s has no size", a.Name)
	}
	return h, nil
}

func executable() (string, error) {
	exe, err := os.Executable()
	if err != nil {
		return "", err
	}
	if resolved, err := filepath.EvalSymlinks(exe); err == nil {
		exe = resolved
	}
	return exe, nil
}

// Apply downloads, verifies and swaps in the asset for the running binary.
// Call Restart afterwards.
func Apply(owner, repo string, a *Asset) error {
	exe, err := executable()
	if err != nil {
		return err
	}
	return applyTo(exe, owner, repo, a)
}

func applyTo(exe, owner, repo string, a *Asset) error {
	wantHash, err := validateAsset(owner, repo, a)
	if err != nil {
		return err
	}
	tmp := exe + ".new"
	_ = os.Remove(tmp) // stale partial download
	if err := download(a.DownloadURL, tmp, a.Size, wantHash); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	old := exe + ".old"
	_ = os.Remove(old)
	if err := os.Rename(exe, old); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("move current program aside: %w (is the folder writable?)", err)
	}
	if err := os.Rename(tmp, exe); err != nil {
		_ = os.Rename(old, exe) // roll back
		_ = os.Remove(tmp)
		return fmt.Errorf("install update: %w", err)
	}
	return nil
}

// download streams url into dst, hashing it; checks size and SHA-256.
func download(url, dst string, wantSize int64, wantHash string) error {
	resp, err := downloadClient.Get(url)
	if err != nil {
		return fmt.Errorf("download: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("download: status %d", resp.StatusCode)
	}
	const execPerm = 0o755
	f, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, execPerm)
	if err != nil {
		return err
	}
	h := sha256.New()
	// One byte past the expected size detects oversized bodies early.
	n, err := io.Copy(io.MultiWriter(f, h), io.LimitReader(resp.Body, wantSize+1))
	if err != nil {
		f.Close()
		return fmt.Errorf("download: %w", err)
	}
	if n != wantSize {
		f.Close()
		return fmt.Errorf("download size mismatch: got %d bytes, want %d", n, wantSize)
	}
	if got := hex.EncodeToString(h.Sum(nil)); got != wantHash {
		f.Close()
		return fmt.Errorf("download checksum mismatch: got sha256 %s, want %s", got, wantHash)
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return err
	}
	return f.Close()
}

// WaitArg tells the new process which process it replaces:
// "--wait-pid=<pid>". The new process waits for it to exit (WaitForExit)
// before it starts its window, so the single-instance lock the old process
// still holds doesn't make the new one quit right away.
const WaitArg = "--wait-pid="

// RestartWait is how long a restarted darkbot waits for the old process.
const RestartWait = 15 * time.Second

// Restart starts the (updated) binary detached with args and exits this
// process.
func Restart(args ...string) error {
	exe, err := executable()
	if err != nil {
		return err
	}
	args = append(args, WaitArg+strconv.Itoa(os.Getpid()))
	cmd := exec.Command(exe, args...)
	if err := cmd.Start(); err != nil {
		return err
	}
	_ = cmd.Process.Release()
	os.Exit(0)
	return nil
}

// CleanupPrevious removes <exe>.old from a previous update (best effort).
func CleanupPrevious() {
	if exe, err := executable(); err == nil {
		_ = os.Remove(exe + ".old")
	}
}

// Info describes the update state for the UI.
type Info struct {
	Current   string `json:"current"`
	Latest    string `json:"latest"`
	Available bool   `json:"available"`
	Notes     string `json:"notes"`
	URL       string `json:"url"`
}

// Check compares the running version with the latest release.
func Check(owner, repo, current string) (*Info, error) {
	rel, err := Latest(owner, repo)
	if err != nil {
		return nil, err
	}
	info := &Info{
		Current: current,
		Latest:  strings.TrimPrefix(rel.TagName, "v"),
		Notes:   rel.Body,
		URL:     rel.HTMLURL,
	}
	info.Available = IsNewer(current, rel.TagName) && FindAsset(rel, AssetName()) != nil
	return info, nil
}

// Update re-resolves the latest release and installs it if newer. It takes
// no URL, so nothing the UI sends can choose what gets executed.
func Update(owner, repo, current string) (string, error) {
	rel, err := Latest(owner, repo)
	if err != nil {
		return "", err
	}
	if !IsNewer(current, rel.TagName) {
		return "", fmt.Errorf("already up to date (v%s, latest %s)", current, rel.TagName)
	}
	asset := FindAsset(rel, AssetName())
	if asset == nil {
		return "", fmt.Errorf("release %s has no %s", rel.TagName, AssetName())
	}
	return rel.TagName, Apply(owner, repo, asset)
}

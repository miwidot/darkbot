package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/wailsapp/wails/v2/pkg/runtime"

	"github.com/miwidot/darkbot/internal/config"
	"github.com/miwidot/darkbot/internal/i18n"
	"github.com/miwidot/darkbot/internal/obs"
	"github.com/miwidot/darkbot/internal/secret"
	"github.com/miwidot/darkbot/internal/songs"
)

// "Now Playing": the song Spotify or YouTube Music plays, shown by a
// browser source in OBS and previewed in the Now Playing tab. Both run the
// same page (frontend/dist/overlay/nowplaying.html, six designs).

const (
	// nowPlayingPoll: how often darkbot asks the services; the page counts
	// the progress on by itself in between.
	nowPlayingPoll = 3 * time.Second
	// nowPlayingTimeout bounds one round of asking the services.
	nowPlayingTimeout = 5 * time.Second
	// coverMaxBytes caps a downloaded cover (they are ~20–100 KB).
	coverMaxBytes = 2 << 20
	coverTimeout  = 10 * time.Second
	// coverURLPath serves the cached cover to the preview frames.
	coverURLPath = "/np/cover"
	// Files darkbot writes for OBS (the page loads the font next to it).
	overlayDir   = "overlay"
	overlayPage  = "overlay/nowplaying.html"
	goalsPage    = "overlay/goals.html"
	votePage     = "overlay/vote.html"
	overlayFont  = "fonts/archivo-latin-wdth-normal.woff2"
	assetsPrefix = "frontend/dist/"
)

// npSong is the song as the page gets it.
type npSong struct {
	Service     string `json:"service"`
	ServiceName string `json:"serviceName"`
	ID          string `json:"id"`
	Title       string `json:"title"`
	Artist      string `json:"artist"`
	Cover       string `json:"cover"`
	DurationMS  int64  `json:"durationMs"`
	ProgressMS  int64  `json:"progressMs"`
	Playing     bool   `json:"playing"`
	RequestedBy string `json:"requestedBy"`
}

// npPayload is everything the page needs: design, switches, words, song.
type npPayload struct {
	Theme          string            `json:"theme"`
	ShowCover      bool              `json:"showCover"`
	ShowProgress   bool              `json:"showProgress"`
	ShowRequester  bool              `json:"showRequester"`
	HideWhenPaused bool              `json:"hideWhenPaused"`
	Labels         map[string]string `json:"labels"`
	Song           *npSong           `json:"song"`
}

// NowPlayingState is what the Now Playing tab shows.
type NowPlayingState struct {
	Payload npPayload `json:"payload"`
	// Problem: why there is no song (a service failed), "" otherwise.
	Problem string `json:"problem"`
	// OBSReady: the browser source is in the darkbot scene.
	OBSReady bool `json:"obsReady"`
	// OBSProblem: why the overlay isn't in OBS (not connected, no darkbot
	// scene, ...), "" when it is.
	OBSProblem string `json:"obsProblem"`
}

// nowPlaying holds the overlay's runtime state (guarded by mu).
type nowPlaying struct {
	mu        sync.Mutex
	previews  int // open Now Playing tabs (normally 0 or 1)
	song      *songs.Now
	problem   error
	page      string // absolute path of the page for OBS, "" = not written
	refreshed bool   // OBS reloaded the page after darkbot wrote it
	pushErr   string // last error sending to OBS (logged once)

	coverMu  sync.Mutex
	coverURL string // the service's URL of the cached cover
	cover    []byte
	coverCT  string
}

// overlayFiles are the OBS overlay pages and the font they share.
var overlayFiles = []string{overlayPage, goalsPage, votePage, overlayFont}

// writeOverlayFiles copies the overlay pages and their font into darkbot's
// settings folder, where OBS can open them; unchanged files are left
// alone. It returns the Now Playing page's path (the others sit next to
// it, see overlayPath) and whether anything was rewritten.
func writeOverlayFiles(dir string, files fs.FS) (page string, changed bool, err error) {
	for _, name := range overlayFiles {
		data, err := fs.ReadFile(files, assetsPrefix+name)
		if err != nil {
			return "", false, fmt.Errorf("read the embedded %s: %w", name, err)
		}
		dst := filepath.Join(dir, filepath.FromSlash(name))
		if old, err := os.ReadFile(dst); err == nil && bytes.Equal(old, data) {
			continue
		}
		if err := os.MkdirAll(filepath.Dir(dst), 0o700); err != nil {
			return "", false, err
		}
		if err := secret.WriteFileAtomic(dst, data); err != nil {
			return "", false, fmt.Errorf("write %s: %w", dst, err)
		}
		changed = true
	}
	return overlayPath(dir, overlayPage), changed, nil
}

// overlayPath is where writeOverlayFiles put an overlay file.
func overlayPath(dir, name string) string { return filepath.Join(dir, filepath.FromSlash(name)) }

// startNowPlaying writes the page for OBS and starts asking the services
// while the overlay is on or its tab is open.
func (a *App) startNowPlaying(ctx context.Context) {
	dir, err := config.Dir()
	if err == nil {
		var page string
		var changed bool
		page, changed, err = writeOverlayFiles(dir, assets)
		a.np.mu.Lock()
		a.np.page = page
		a.np.refreshed = !changed // a new page must be reloaded in OBS once
		a.np.mu.Unlock()
		a.ov.setPages(dir, changed)
	}
	if err != nil {
		a.log.Errorf("Now Playing: could not write the overlay page: %v", err)
	}
	go a.nowPlayingLoop(ctx)
}

func (a *App) nowPlayingWanted() bool {
	a.np.mu.Lock()
	previews := a.np.previews
	a.np.mu.Unlock()
	return previews > 0 || a.cfg.Get().NowPlaying.Enabled
}

func (a *App) nowPlayingLoop(ctx context.Context) {
	t := time.NewTicker(nowPlayingPoll)
	defer t.Stop()
	for {
		if a.nowPlayingWanted() {
			a.pollNowPlaying(ctx)
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// pollNowPlaying asks the services once and passes the result on.
func (a *App) pollNowPlaying(ctx context.Context) {
	cctx, cancel := context.WithTimeout(ctx, nowPlayingTimeout)
	n, err := a.sr.NowPlaying(cctx)
	cancel()
	if ctx.Err() != nil {
		return
	}
	a.np.mu.Lock()
	before := a.np.problem
	a.np.song, a.np.problem = n, err
	a.np.mu.Unlock()
	if err != nil && (before == nil || before.Error() != err.Error()) {
		a.log.Warnf("Now Playing: %v", err)
	}
	if n != nil && n.Cover != "" {
		a.cacheCover(ctx, n.Cover)
	}
	a.pushNowPlaying()
}

// payload builds what the page gets; local swaps the cover for darkbot's
// cached copy (the app's pages load nothing from the internet).
func (a *App) payload(local bool) npPayload {
	c := a.cfg.Get().NowPlaying
	lang := a.lang()
	p := npPayload{
		Theme: c.Theme, ShowCover: c.ShowCover, ShowProgress: c.ShowProgress,
		ShowRequester: c.ShowRequester, HideWhenPaused: c.HideWhenPaused,
		Labels: map[string]string{
			"nowPlaying":     i18n.T(lang, "np.overlayNowPlaying"),
			"requestedBy":    i18n.T(lang, "np.overlayRequestedBy"),
			"requestedShort": i18n.T(lang, "np.overlayRequestedShort"),
			"playingOn":      i18n.T(lang, "np.overlayPlayingOn"),
		},
	}
	a.np.mu.Lock()
	n := a.np.song
	a.np.mu.Unlock()
	if n == nil {
		return p
	}
	s := &npSong{
		Service: n.Service, ServiceName: n.ServiceName, ID: n.ID, Title: n.Title, Artist: n.Artist,
		Cover: n.Cover, DurationMS: n.Duration.Milliseconds(), ProgressMS: n.Progress.Milliseconds(),
		Playing: n.Playing, RequestedBy: n.RequestedBy,
	}
	if local {
		s.Cover = a.localCover(n.Cover)
	}
	p.Song = s
	return p
}

// pushNowPlaying sends the current state to the open tab and to OBS.
func (a *App) pushNowPlaying() {
	if a.ctx != nil {
		runtime.EventsEmit(a.ctx, "nowplaying", a.nowPlayingState(false))
	}
	if !a.cfg.Get().NowPlaying.Enabled || !a.obs.Connected() {
		return
	}
	a.np.mu.Lock()
	refresh := !a.np.refreshed
	a.np.refreshed = true
	a.np.mu.Unlock()
	if refresh {
		if err := a.obs.RefreshNowPlaying(); err != nil {
			a.log.Warnf("Now Playing: could not reload the page in OBS: %v", err)
		}
	}
	data, err := toMap(a.payload(false))
	if err == nil {
		err = a.obs.EmitNowPlaying(data)
	}
	// Sent every few seconds: log a problem once, not every time.
	msg := ""
	if err != nil {
		msg = err.Error()
	}
	a.np.mu.Lock()
	changed := msg != a.np.pushErr
	a.np.pushErr = msg
	a.np.mu.Unlock()
	if changed && err != nil {
		a.log.Warnf("Now Playing: %v", err)
	}
}

func toMap(v any) (map[string]any, error) {
	b, err := json.Marshal(v)
	if err != nil {
		return nil, err
	}
	var m map[string]any
	return m, json.Unmarshal(b, &m)
}

// cacheCover downloads a new cover for the preview (https only, size
// capped); a failure just leaves the preview without a picture.
func (a *App) cacheCover(ctx context.Context, src string) {
	a.np.coverMu.Lock()
	same := a.np.coverURL == src
	a.np.coverMu.Unlock()
	if same {
		return
	}
	data, ct, err := fetchCover(ctx, src)
	if err != nil {
		a.log.Warnf("Now Playing: cover: %v", err)
	}
	a.np.coverMu.Lock()
	a.np.coverURL, a.np.cover, a.np.coverCT = src, data, ct
	a.np.coverMu.Unlock()
}

func fetchCover(ctx context.Context, src string) ([]byte, string, error) {
	u, err := url.Parse(src)
	if err != nil || u.Scheme != "https" {
		return nil, "", fmt.Errorf("not an https address: %q", src)
	}
	ctx, cancel := context.WithTimeout(ctx, coverTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), http.NoBody)
	if err != nil {
		return nil, "", err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, "", err
	}
	defer resp.Body.Close()
	ct := resp.Header.Get("Content-Type")
	if resp.StatusCode != http.StatusOK || !strings.HasPrefix(ct, "image/") {
		return nil, "", fmt.Errorf("status %d, type %q", resp.StatusCode, ct)
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, coverMaxBytes+1))
	if err != nil {
		return nil, "", err
	}
	if len(data) > coverMaxBytes {
		return nil, "", errors.New("cover larger than 2 MB")
	}
	return data, ct, nil
}

// localCover is the app's address of the cached cover for src ("" when it
// isn't cached); the hash makes the frames load a new cover.
func (a *App) localCover(src string) string {
	a.np.coverMu.Lock()
	defer a.np.coverMu.Unlock()
	if src == "" || a.np.coverURL != src || a.np.cover == nil {
		return ""
	}
	sum := sha256.Sum256([]byte(src))
	return coverURLPath + "?v=" + hex.EncodeToString(sum[:8])
}

// serveCover serves the cached cover to the preview frames.
func (a *App) serveCover(w http.ResponseWriter) {
	a.np.coverMu.Lock()
	data, ct := a.np.cover, a.np.coverCT
	a.np.coverMu.Unlock()
	if data == nil {
		http.Error(w, "no cover", http.StatusNotFound)
		return
	}
	w.Header().Set("Content-Type", ct)
	w.Header().Set("Cache-Control", "no-store")
	_, _ = w.Write(data)
}

// nowPlayingState is the tab's view; withOBS also asks OBS where the
// overlay is (only when the tab opens or a setting changes).
func (a *App) nowPlayingState(withOBS bool) NowPlayingState {
	s := NowPlayingState{Payload: a.payload(true)}
	a.np.mu.Lock()
	if a.np.problem != nil && a.np.song == nil {
		s.Problem = i18n.Localize(a.np.problem, a.lang())
	}
	a.np.mu.Unlock()
	if withOBS {
		s.OBSReady, s.OBSProblem = a.nowPlayingOBS()
	}
	return s
}

func (a *App) nowPlayingOBS() (bool, string) {
	if !a.obs.Connected() {
		return false, i18n.T(a.lang(), "np.obsOffline")
	}
	ready, err := a.obs.NowPlayingReady()
	if err != nil {
		return false, i18n.Localize(err, a.lang())
	}
	if !ready {
		return false, i18n.T(a.lang(), "np.obsMissing")
	}
	return true, ""
}

// GetNowPlaying returns the tab's state (and checks OBS).
func (a *App) GetNowPlaying() NowPlayingState { return a.nowPlayingState(true) }

// SetNowPlayingPreview: the Now Playing tab opened (true) or closed. While
// it is open darkbot asks the services even with the overlay off.
func (a *App) SetNowPlayingPreview(open bool) {
	a.np.mu.Lock()
	if open {
		a.np.previews++
	} else if a.np.previews > 0 {
		a.np.previews--
	}
	first := open && a.np.previews == 1
	a.np.mu.Unlock()
	if first && a.ctx != nil {
		go a.pollNowPlaying(a.ctx) // don't wait for the next tick
	}
}

// SaveNowPlaying stores the overlay settings and applies them in OBS.
func (a *App) SaveNowPlaying(c config.NowPlaying) (NowPlayingState, error) {
	if !slices.Contains(config.NowPlayingThemes, c.Theme) {
		return NowPlayingState{}, a.uiErr(i18n.E("err.npTheme", "theme", c.Theme))
	}
	was := a.cfg.Get().NowPlaying.Enabled
	if err := a.cfg.Update(func(cfg *config.Config) { cfg.NowPlaying = c }); err != nil {
		return NowPlayingState{}, a.uiErr(err)
	}
	var err error
	switch {
	case c.Enabled:
		err = a.setupNowPlaying()
	case was:
		if a.obs.Connected() {
			err = a.obs.ShowNowPlaying(false)
		}
	}
	if c.Enabled != was {
		a.log.Infof("Now Playing overlay %s", map[bool]string{true: "on", false: "off"}[c.Enabled])
	}
	a.pushNowPlaying()
	return a.nowPlayingState(true), a.uiErr(err)
}

// setupNowPlaying puts the overlay into OBS (needs OBS and the darkbot
// scene; without them it is set up on the next connect).
func (a *App) setupNowPlaying() error {
	if !a.obs.Connected() {
		return nil // done by onOBSConnected
	}
	a.np.mu.Lock()
	page := a.np.page
	a.np.mu.Unlock()
	if page == "" {
		return i18n.E("err.npPage")
	}
	created, err := a.obs.SetupNowPlaying(page, true)
	switch {
	case errors.Is(err, obs.ErrNotSetUp):
		return i18n.E("err.npNoScene")
	case errors.Is(err, obs.ErrNoBrowserSource):
		return i18n.E("err.npNoBrowser")
	case err != nil:
		return err
	}
	if created {
		a.log.Infof("Now Playing: added the browser source %q to the darkbot scene", obs.NowPlayingInput)
	}
	return nil
}

// nowPlayingOnOBSConnect re-applies the overlay after OBS (re)connected.
func (a *App) nowPlayingOnOBSConnect() {
	if !a.cfg.Get().NowPlaying.Enabled {
		return
	}
	if err := a.setupNowPlaying(); err != nil {
		a.log.Warnf("Now Playing: %v", i18n.Localize(err, "en"))
		return
	}
	a.pushNowPlaying()
}

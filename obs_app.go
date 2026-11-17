package main

import (
	"context"
	"errors"
	"net"
	"strconv"
	"strings"
	"time"

	"github.com/wailsapp/wails/v2/pkg/runtime"

	"github.com/miwidot/darkbot/internal/bot"
	"github.com/miwidot/darkbot/internal/i18n"
	"github.com/miwidot/darkbot/internal/obs"
)

// OBS stays connected while it is wanted: after "Connect" or, with the
// auto-connect setting, from darkbot's start. If OBS isn't running yet or
// closes, darkbot retries quietly until it is back. A rejected password
// stops the retries until the user connects again.

// obsRetryEvery: how often darkbot looks for OBS while it is wanted but not
// connected. Connecting to localhost is cheap; OBS shows nothing for it.
const obsRetryEvery = 5 * time.Second

func (a *App) startOBS(ctx context.Context) {
	if a.cfg.Get().OBS.AutoConnect {
		a.mu.Lock()
		a.obsWant = true
		a.mu.Unlock()
		go a.tryOBS()
	}
	go a.obsLoop(ctx)
}

func (a *App) obsLoop(ctx context.Context) {
	t := time.NewTicker(obsRetryEvery)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			a.mu.Lock()
			retry := a.obsWant && !a.obsAuthFailed
			a.mu.Unlock()
			if retry && !a.obs.Connected() {
				a.tryOBS()
			}
		}
	}
}

// tryOBS is one quiet connection attempt of the keep-alive. Only changes
// are logged: the first miss, a rejected password, the connect.
func (a *App) tryOBS() {
	_, err := a.connectOBS()
	if err == nil || errors.Is(err, obs.ErrAuth) {
		return // logged by connectOBS
	}
	a.mu.Lock()
	first := !a.obsWaitLogged
	a.obsWaitLogged = true
	a.mu.Unlock()
	if first {
		c := a.cfg.Get().OBS
		a.log.Infof("Waiting for OBS at %s, trying every %s (%v)", net.JoinHostPort(c.Host, strconv.Itoa(c.Port)), obsRetryEvery, err)
		a.emitState()
	}
}

// connectOBS connects with the stored settings and records the outcome.
func (a *App) connectOBS() (string, error) {
	c := a.cfg.Get().OBS
	v, err := a.obs.Connect(c.Host, c.Port, c.Password)
	switch {
	case err == nil:
		a.mu.Lock()
		a.obsWaitLogged, a.obsAuthFailed = false, false
		a.mu.Unlock()
		a.log.Infof("Connected to %s", v)
		a.emitState()
		go a.refreshOBSScene()
	case errors.Is(err, obs.ErrAuth):
		a.mu.Lock()
		a.obsAuthFailed = true
		a.mu.Unlock()
		a.log.Errorf("OBS rejected the password. Copy it from OBS: Tools, WebSocket Server Settings, Show Connect Info")
		a.emitState()
	}
	return v, err
}

// ConnectOBS connects now (Settings → OBS) and keeps the connection up
// from then on.
func (a *App) ConnectOBS() error {
	a.mu.Lock()
	a.obsWant, a.obsAuthFailed, a.obsWaitLogged = true, false, false
	a.mu.Unlock()
	_, err := a.connectOBS()
	if err != nil && !errors.Is(err, obs.ErrAuth) {
		a.log.Errorf("%v", err)
		a.mu.Lock()
		a.obsWaitLogged = true // the loop keeps trying; this was the message
		a.mu.Unlock()
		a.emitState()
	}
	return a.uiErr(err)
}

// DisconnectOBS closes the OBS connection and stops reconnecting.
func (a *App) DisconnectOBS() {
	a.mu.Lock()
	a.obsWant = false
	a.mu.Unlock()
	a.obs.Disconnect()
	a.log.Infof("Disconnected from OBS")
	a.emitState()
}

// obsStatusLocked: (waiting for OBS, password rejected). Needs a.mu.
func (a *App) obsStatusLocked(connected bool) (waiting, authFailed bool) {
	return a.obsWant && !connected && !a.obsAuthFailed, a.obsAuthFailed
}

// obsStatus is obsStatusLocked for callers without a.mu.
func (a *App) obsStatus() (waiting, authFailed bool) {
	connected := a.obs.Connected()
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.obsStatusLocked(connected)
}

// onOBSDrop: OBS went away. The keep-alive reconnects when it is back.
func (a *App) onOBSDrop(err error) {
	a.log.Warnf("OBS connection lost (%v), reconnecting when OBS is back", err)
	a.mu.Lock()
	a.obsWaitLogged = true
	a.obsScene = nil
	a.mu.Unlock()
	a.emitState()
}

// ---------- alerts in OBS (see internal/obs/overlay.go) ----------

// refreshOBSScene reads where darkbot's scene is nested, for the settings.
func (a *App) refreshOBSScene() {
	if !a.obs.Connected() {
		return
	}
	st, err := a.obs.Status()
	if err != nil {
		a.log.Warnf("Could not read the OBS scenes: %v", err)
		return
	}
	a.mu.Lock()
	a.obsScene = &st
	a.mu.Unlock()
	a.emitState()
}

// SetupOBSScene creates darkbot's scene in OBS and adds it on top of every
// scene of the streamer (Settings → OBS, one click).
func (a *App) SetupOBSScene() (int, error) {
	added, err := a.obs.Setup(a.cfg.Get().OBS.InputName)
	if err != nil {
		a.log.Errorf("OBS setup: %v", err)
		return len(added), a.uiErr(err)
	}
	if len(added) > 0 {
		a.log.Infof("Added the darkbot scene to the OBS scenes %s", strings.Join(added, ", "))
	} else {
		a.log.Infof("The darkbot scene is ready in OBS")
	}
	a.refreshOBSScene()
	return len(added), nil
}

// overlay is where event alerts appear: OBS, while connected.
func (a *App) overlay() bot.Overlay {
	if !a.obs.Connected() {
		return nil
	}
	return obsOverlay{a}
}

type obsOverlay struct{ a *App }

func (o obsOverlay) ShowAlert(ctx context.Context, al bot.Alert) (func(), time.Duration, error) {
	return o.a.obs.Show(ctx, obs.Alert{File: al.File, Text: al.Text}, o.a.cfg.Get().OBS.InputName)
}

// PickVisualFile asks for an alert picture, GIF or video.
func (a *App) PickVisualFile() (string, error) {
	lang := a.lang()
	exts := append(append([]string{}, obs.ImageExtensions...), obs.VideoExtensions...)
	path, err := runtime.OpenFileDialog(a.ctx, runtime.OpenDialogOptions{
		Title: i18n.T(lang, "dialog.chooseVisual"),
		Filters: []runtime.FileFilter{{
			DisplayName: i18n.T(lang, "dialog.visualFiles", "ext", strings.Join(exts, ", ")),
			Pattern:     "*" + strings.Join(exts, ";*"),
		}},
	})
	return path, a.uiErr(err)
}

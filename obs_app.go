package main

import (
	"context"
	"errors"
	"net"
	"strconv"
	"time"

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
	a.mu.Unlock()
	a.emitState()
}

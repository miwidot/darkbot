package main

import (
	"context"
	"errors"
	"time"

	"github.com/miwidot/darkbot/internal/i18n"
)

const (
	// ytmusicConnectTimeout: Pear Desktop asks the streamer in a dialog,
	// which may be behind other windows; give them time to find it.
	ytmusicConnectTimeout = 2 * time.Minute
	// ytmusicCheckTimeout bounds the dashboard's "check now".
	ytmusicCheckTimeout = 10 * time.Second
)

// YTMusicConnect asks Pear Desktop for access and waits until the streamer
// clicks Allow or Deny there (or cancels in darkbot). connected is false
// when they cancelled.
func (a *App) YTMusicConnect() (connected bool, err error) {
	a.mu.Lock()
	if a.ytConnect != nil {
		a.mu.Unlock()
		return false, a.uiErr(i18n.E("err.ytmBusy"))
	}
	ctx, cancel := context.WithTimeout(a.ctx, ytmusicConnectTimeout)
	a.ytConnect = cancel
	a.mu.Unlock()
	a.emitState()
	defer func() {
		cancel()
		a.mu.Lock()
		a.ytConnect = nil
		a.mu.Unlock()
		a.emitState()
	}()
	a.log.Infof("YouTube Music: asking Pear Desktop for access")
	err = a.yt.Connect(ctx)
	switch {
	case err == nil:
		a.log.Infof("Connected to YouTube Music (Pear Desktop)")
		return true, nil
	case errors.Is(err, context.Canceled):
		a.log.Infof("YouTube Music: connecting cancelled")
		return false, nil
	case errors.Is(err, context.DeadlineExceeded):
		a.log.Warnf("YouTube Music: no answer from Pear Desktop within %s", ytmusicConnectTimeout)
		return false, a.uiErr(i18n.E("err.ytmTimeout"))
	}
	a.log.Warnf("YouTube Music: connecting failed: %v", err)
	return false, a.uiErr(err)
}

// YTMusicCancelConnect stops waiting for Pear Desktop.
func (a *App) YTMusicCancelConnect() {
	a.mu.Lock()
	cancel := a.ytConnect
	a.mu.Unlock()
	if cancel != nil {
		cancel()
	}
}

// YTMusicDisconnect forgets the access to Pear Desktop.
func (a *App) YTMusicDisconnect() error {
	err := a.yt.Disconnect()
	a.log.Infof("Disconnected from YouTube Music")
	return a.uiErr(err)
}

// YTMusicCheck asks Pear Desktop once whether it answers (dashboard).
func (a *App) YTMusicCheck() error {
	ctx, cancel := context.WithTimeout(a.ctx, ytmusicCheckTimeout)
	defer cancel()
	_, err := a.yt.Playing(ctx)
	if err != nil {
		a.log.Warnf("YouTube Music check: %v", err)
	}
	return a.uiErr(err)
}

package main

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/miwidot/darkbot/internal/applog"
	"github.com/miwidot/darkbot/internal/config"
	"github.com/miwidot/darkbot/internal/i18n"
)

// testSound is 1.5 s of AAC.
var testSound = config.Sound{ID: "s1", RewardTitle: "Horn", File: filepath.Join("internal", "audio", "testdata", "aac_ffmpeg.m4a"), Volume: 1}

const testSoundLength = 1500 * time.Millisecond

// fakeWindow answers play requests like the app window would, after the
// given delays (a negative delay: that report never comes).
func fakeWindow(t *testing.T, startAfter, endAfter time.Duration, errMsg string) (*webOutput, *applog.Log) {
	t.Helper()
	log := applog.New()
	var o *webOutput
	o = newWebOutput(log, func(req playRequest) {
		go func() {
			if startAfter >= 0 {
				time.Sleep(startAfter)
				o.started(req.Token)
			}
			if endAfter >= 0 {
				time.Sleep(endAfter)
				o.done(req.Token, errMsg)
			}
		}()
	})
	o.startTimeout, o.grace, o.lateStart = time.Second, 200*time.Millisecond, 500*time.Millisecond
	return o, log
}

// A sound that starts late (longer than its length plus the grace) still
// counts as played, and the late start is logged.
func TestPlayLateStart(t *testing.T) {
	o, log := fakeWindow(t, 800*time.Millisecond, testSoundLength, "")
	if err := o.Play(context.Background(), testSound, 1); err != nil {
		t.Fatalf("late start failed: %v", err)
	}
	if !strings.Contains(strings.Join(log.Lines(), "\n"), "started") {
		t.Errorf("late start not logged: %q", log.Lines())
	}
}

func TestPlayFailures(t *testing.T) {
	wantKey := func(name string, err error, key string) {
		t.Helper()
		if e, ok := errors.AsType[*i18n.Error](err); !ok || e.Key != key {
			t.Errorf("%s: err = %v, want %s", name, err, key)
		}
	}
	o, _ := fakeWindow(t, -1, -1, "")
	wantKey("never starts", o.Play(context.Background(), testSound, 1), "err.noPlaybackStart")

	o, _ = fakeWindow(t, 0, -1, "")
	wantKey("never ends", o.Play(context.Background(), testSound, 1), "err.noPlaybackConfirm")

	o, _ = fakeWindow(t, -1, 0, "cannot play")
	if err := o.Play(context.Background(), testSound, 1); err == nil || err.Error() != "cannot play" {
		t.Errorf("error before the start: %v", err)
	}

	o, log := fakeWindow(t, 0, 100*time.Millisecond, "")
	if err := o.Play(context.Background(), testSound, 1); err != nil || len(log.Lines()) != 0 {
		t.Errorf("prompt sound: %v, log %q", err, log.Lines())
	}
	if len(o.pending) != 0 {
		t.Errorf("%d playbacks left pending", len(o.pending))
	}
}

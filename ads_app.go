package main

import (
	"context"
	"math"
	"time"

	"github.com/miwidot/darkbot/internal/config"
	"github.com/miwidot/darkbot/internal/twitch"
)

// Ad warning and raid shoutouts (calls in internal/twitch/manage.go).
//
// Twitch has no "ad soon" event; Get Ad Schedule says when the next
// automatic ad starts ("0 if the channel has no ad scheduled or is not
// live"). While live and the warning is on, darkbot reads it every
// adPollInterval and raises EvAdSoon LeadSeconds before the ad.

const (
	// adPollInterval: how often the schedule is read while live and the
	// warning is on. A snooze or a manual ad changes it, so it is read
	// again rather than trusted for long.
	adPollInterval = 15 * time.Second
	// adCallTimeout bounds one Get Ad Schedule or shoutout call.
	adCallTimeout = 15 * time.Second
)

// adWarnDue decides whether the ad at next deserves a warning now: it is
// in the future, within lead, and not the one already warned about. left
// is the whole seconds until it starts (rounded up).
func adWarnDue(next time.Time, lead time.Duration, now, warned time.Time) (due bool, left int) {
	if next.IsZero() || !next.After(now) || next.Equal(warned) || now.Before(next.Add(-lead)) {
		return false, 0
	}
	return true, int(math.Ceil(next.Sub(now).Seconds()))
}

// adWait is how long to sleep before looking again: the next poll, or the
// moment the warning is due if that comes first.
func adWait(next time.Time, lead time.Duration, now time.Time) time.Duration {
	if next.IsZero() {
		return adPollInterval
	}
	if until := next.Add(-lead).Sub(now); until > 0 && until < adPollInterval {
		return until
	}
	return adPollInterval
}

// adWarning returns the warning's settings and whether it should run now.
func (a *App) adWarning() (lead time.Duration, ok bool) {
	act, on := a.cfg.Get().Events[twitch.EvAdSoon]
	t := a.tok.Current()
	if !on || !act.Enabled || !a.live.Load() || t == nil || !t.HasScope(twitch.ScopeReadAds) {
		return 0, false
	}
	secs := act.LeadSeconds
	if secs == 0 {
		secs = config.DefaultAdLead
	}
	return time.Duration(secs) * time.Second, true
}

// startAdWatch runs the ad warning until ctx ends. Called in startup.
func (a *App) startAdWatch(ctx context.Context) {
	go func() {
		var next, warned time.Time
		failing := false
		for {
			wait := adPollInterval
			if lead, ok := a.adWarning(); ok {
				if s, err := a.readAdSchedule(ctx); err != nil {
					if !failing {
						a.log.Warnf("Could not read the ad schedule: %v", err)
					}
					failing = true
				} else {
					if failing {
						a.log.Infof("Ad schedule readable again")
					}
					failing, next = false, s.NextAd
				}
				now := time.Now()
				if due, left := adWarnDue(next, lead, now, warned); due {
					warned = next
					a.log.Infof("Ad in %d seconds", left)
					a.onChannelEvent(twitch.ChannelEvent{Kind: twitch.EvAdSoon, Amount: left})
				}
				wait = adWait(next, lead, now)
			}
			select {
			case <-ctx.Done():
				return
			case <-time.After(wait):
			}
		}
	}()
}

func (a *App) readAdSchedule(ctx context.Context) (twitch.AdSchedule, error) {
	t := a.tok.Current()
	if t == nil {
		return twitch.AdSchedule{}, twitch.ErrNotLoggedIn
	}
	ctx, cancel := context.WithTimeout(ctx, adCallTimeout)
	defer cancel()
	return a.hx.AdScheduleNow(ctx, t.UserID)
}

// shoutout gives a raider Twitch's shoutout (Send a Shoutout: once every
// 2 minutes, the same channel once an hour, only while live; Twitch says
// no otherwise, which is logged).
func (a *App) shoutout(ev twitch.ChannelEvent) {
	t := a.tok.Current()
	switch {
	case ev.UserID == "":
		a.log.Infof("Shoutout skipped: no channel to shout out (test)")
		return
	case t == nil || !t.HasScope(twitch.ScopeManageShoutouts):
		a.log.Warnf("Shoutout for %s skipped: reconnect Twitch once (Settings, Twitch) to allow shoutouts", ev.User)
		return
	}
	ctx, cancel := context.WithTimeout(a.ctx, adCallTimeout)
	defer cancel()
	if err := a.hx.Shoutout(ctx, t.UserID, ev.UserID); err != nil {
		a.log.Warnf("Shoutout for %s failed: %v", ev.User, err)
		return
	}
	a.log.Infof("Shoutout given to %s", ev.User)
}

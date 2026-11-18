package main

import (
	"context"
	"sync"
	"time"

	"github.com/miwidot/darkbot/internal/twitch"
)

// The dashboard's channel row shows the channel as it is now (followers,
// active subs) from Twitch, next to what darkbot counted this session.
// The same loop reads whether the stream is live (see chat_app.go).

const (
	// channelTotalsEvery: regular refresh, two cheap requests.
	channelTotalsEvery = 5 * time.Minute
	// channelTotalsDelay: after a follow or sub event Twitch needs a moment
	// until its totals include it; events arriving meanwhile share one read.
	channelTotalsDelay   = 5 * time.Second
	channelTotalsTimeout = 15 * time.Second
)

// ChannelTotals is the channel's current state on Twitch. nil fields are
// unknown (not logged in, permission missing, or the read failed).
type ChannelTotals struct {
	Followers *int      `json:"followers"`
	Subs      *int      `json:"subs"`
	SubPoints *int      `json:"subPoints"`
	UpdatedAt time.Time `json:"updatedAt"`
}

type channelState struct {
	mu      sync.Mutex
	totals  ChannelTotals
	failing bool          // a read failed; logged once until one works again
	kick    chan struct{} // made in NewApp, before any goroutine runs
}

// channelKinds change the totals.
var channelKinds = map[string]bool{twitch.EvFollow: true, twitch.EvSub: true, twitch.EvResub: true, twitch.EvGift: true, twitch.EvSubEnd: true}

func (a *App) startChannelTotals(ctx context.Context) {
	go a.channelTotalsLoop(ctx)
}

// refreshChannelTotalsSoon asks for a fresh read after channelTotalsDelay.
func (a *App) refreshChannelTotalsSoon() {
	if a.ch.kick == nil {
		return
	}
	select {
	case a.ch.kick <- struct{}{}:
	default: // one is pending already
	}
}

func (a *App) channelTotalsLoop(ctx context.Context) {
	tick := time.NewTicker(channelTotalsEvery)
	defer tick.Stop()
	a.readChannelTotals(ctx)
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		case <-a.ch.kick:
			select {
			case <-ctx.Done():
				return
			case <-time.After(channelTotalsDelay):
			}
		}
		a.readChannelTotals(ctx)
	}
}

func (a *App) readChannelTotals(ctx context.Context) {
	tok := a.tok.Current()
	if tok == nil {
		a.setLive(false, time.Time{})
		a.setChannelTotals(ChannelTotals{}, nil)
		return
	}
	ctx, cancel := context.WithTimeout(ctx, channelTotalsTimeout)
	defer cancel()
	var t ChannelTotals
	firstErr := a.readLive(ctx, tok.UserID)
	if tok.HasScope(twitch.ScopeReadFollowers) {
		if n, err := a.hx.GetFollowerCount(ctx, tok.UserID); err == nil {
			t.Followers = &n
		} else if firstErr == nil {
			firstErr = err
		}
	}
	if tok.HasScope(twitch.ScopeReadSubs) {
		if n, p, err := a.hx.GetSubscriptionTotals(ctx, tok.UserID); err == nil {
			t.Subs, t.SubPoints = &n, &p
		} else if firstErr == nil {
			firstErr = err
		}
	}
	t.UpdatedAt = time.Now()
	a.setChannelTotals(t, firstErr)
}

func (a *App) setChannelTotals(t ChannelTotals, err error) {
	a.ch.mu.Lock()
	logFail := err != nil && !a.ch.failing
	a.ch.failing = err != nil
	changed := !sameTotals(a.ch.totals, t)
	a.ch.totals = t
	a.ch.mu.Unlock()
	if logFail {
		a.log.Warnf("Could not read the stream state or follower/sub totals from Twitch: %v", err)
	}
	if changed {
		a.emitState() // the UI re-renders on every state event
	}
}

// sameTotals compares the numbers (not when they were read).
func sameTotals(x, y ChannelTotals) bool {
	eq := func(p, q *int) bool { return (p == nil) == (q == nil) && (p == nil || *p == *q) }
	return eq(x.Followers, y.Followers) && eq(x.Subs, y.Subs) && eq(x.SubPoints, y.SubPoints)
}

func (a *App) channelTotals() ChannelTotals {
	a.ch.mu.Lock()
	defer a.ch.mu.Unlock()
	return a.ch.totals
}

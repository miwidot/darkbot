package main

import (
	"context"
	"errors"
	"slices"
	"sync"
	"time"

	"github.com/miwidot/darkbot/internal/config"
	"github.com/miwidot/darkbot/internal/i18n"
	"github.com/miwidot/darkbot/internal/obs"
	"github.com/miwidot/darkbot/internal/twitch"
)

// The goal overlay and the poll/prediction overlay: browser sources in the
// darkbot scene (like Now Playing), fed by the channel.goal.*, channel.poll.*
// and channel.prediction.* events through obs-browser's emit_event.

const (
	// goalEndLinger / voteEndLinger: how long a finished goal or vote stays
	// on screen with its result.
	goalEndLinger = 20 * time.Second
	voteEndLinger = 30 * time.Second
	// testLinger: how long the sample from an alert's "Test" stays.
	testLinger = 15 * time.Second
	// pruneSlack: the prune runs this long after something is due to
	// leave, so it is surely due by then.
	pruneSlack = 100 * time.Millisecond
	// goalsTimeout bounds reading the goals from Twitch.
	goalsTimeout = 10 * time.Second
)

// goalView is one goal on the overlay.
type goalView struct {
	twitch.Goal
	Achieved bool `json:"achieved"`
	Ended    bool `json:"ended"`
	// gone: when it leaves the screen (zero = while it runs).
	gone time.Time
}

// voteView is the poll or prediction on the overlay.
type voteView struct {
	twitch.Vote
	gone time.Time
}

// overlays holds both overlays' state (guarded by mu).
type overlays struct {
	mu        sync.Mutex
	goalsPage string // absolute paths, "" = not written
	votePage  string
	refreshed map[string]bool // OBS input -> page reloaded since darkbot wrote it
	goals     []goalView
	vote      *voteView
	pushErr   map[string]string // OBS input -> last error (logged once)
}

func (o *overlays) setPages(dir string, changed bool) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.goalsPage, o.votePage = overlayPath(dir, goalsPage), overlayPath(dir, votePage)
	o.refreshed = map[string]bool{obs.GoalInput: !changed, obs.VoteInput: !changed}
}

// apply takes a goal or vote event into the overlay state; test events
// leave after testLinger. It reports which overlay changed and when the
// thing it shows leaves the screen (zero: not planned).
func (o *overlays) apply(ev twitch.ChannelEvent, test bool, now time.Time) (goals, vote bool, gone time.Time) {
	o.mu.Lock()
	defer o.mu.Unlock()
	if ev.Goal != nil {
		g := goalView{Goal: *ev.Goal}
		switch ev.Kind {
		case twitch.EvGoalAchieved, twitch.EvGoalEnd:
			g.Ended, g.Achieved, g.gone = true, ev.Kind == twitch.EvGoalAchieved, now.Add(goalEndLinger)
		}
		if test {
			g.gone = now.Add(testLinger)
		}
		if i := slices.IndexFunc(o.goals, func(x goalView) bool { return x.ID == g.ID }); i >= 0 {
			o.goals[i] = g
		} else {
			o.goals = append(o.goals, g)
		}
		goals, gone = true, g.gone
	}
	if ev.Vote != nil {
		v := &voteView{Vote: *ev.Vote}
		if v.State == twitch.VoteEnded || v.State == twitch.VoteCanceled {
			v.gone = now.Add(voteEndLinger)
		}
		if test {
			v.gone = now.Add(testLinger)
		}
		o.vote, vote, gone = v, true, v.gone
	}
	return goals, vote, gone
}

// seedGoals replaces the running goals with Twitch's list (finished and
// sample goals stay until they leave).
func (o *overlays) seedGoals(list []twitch.Goal) bool {
	o.mu.Lock()
	defer o.mu.Unlock()
	keep := slices.DeleteFunc(slices.Clone(o.goals), func(g goalView) bool { return g.gone.IsZero() })
	for _, g := range list {
		if !slices.ContainsFunc(keep, func(k goalView) bool { return k.ID == g.ID }) {
			keep = append(keep, goalView{Goal: g})
		}
	}
	changed := !slices.EqualFunc(keep, o.goals, func(a, b goalView) bool { return a.Goal == b.Goal && a.Ended == b.Ended })
	o.goals = keep
	return changed
}

// prune drops what has left the screen; it reports what changed.
func (o *overlays) prune(now time.Time) (goals, vote bool) {
	o.mu.Lock()
	defer o.mu.Unlock()
	n := len(o.goals)
	o.goals = slices.DeleteFunc(o.goals, func(g goalView) bool { return !g.gone.IsZero() && !now.Before(g.gone) })
	if o.vote != nil && !o.vote.gone.IsZero() && !now.Before(o.vote.gone) {
		o.vote, vote = nil, true
	}
	return len(o.goals) != n, vote
}

func (o *overlays) snapshot() ([]goalView, *voteView) {
	o.mu.Lock()
	defer o.mu.Unlock()
	goals := slices.Clone(o.goals)
	var v *voteView
	if o.vote != nil {
		c := *o.vote
		v = &c
	}
	return goals, v
}

// overlayEvent hands a goal or vote event to the overlays and plans its
// leaving. Called for every event that carries a Goal or a Vote.
func (a *App) overlayEvent(ev twitch.ChannelEvent, test bool) {
	goals, vote, gone := a.ov.apply(ev, test, time.Now())
	a.pushOverlays(goals, vote)
	if !gone.IsZero() {
		time.AfterFunc(time.Until(gone)+pruneSlack, func() { a.pushOverlays(a.ov.prune(time.Now())) })
	}
}

// refreshGoals reads the running goals from Twitch (at start and with the
// channel numbers), so goals set before darkbot started show too.
func (a *App) refreshGoals(ctx context.Context, tok *twitch.Token) error {
	if !a.cfg.Get().Overlays.Goals || !tok.HasScope(twitch.ScopeReadGoals) {
		return nil
	}
	ctx, cancel := context.WithTimeout(ctx, goalsTimeout)
	defer cancel()
	list, err := a.hx.Goals(ctx, tok.UserID)
	if err != nil {
		return err
	}
	if a.ov.seedGoals(list) {
		a.pushOverlays(true, false)
	}
	return nil
}

// goalLabels and voteLabels are the overlay pages' words in the UI language.
func (a *App) goalLabels() map[string]string {
	lang := a.lang()
	out := map[string]string{"achieved": i18n.T(lang, "overlay.achieved"), "ended": i18n.T(lang, "overlay.goalEnded"), "other": i18n.T(lang, "overlay.goal.other")}
	for _, t := range []string{twitch.GoalFollow, "subscription", "subscription_count", "new_subscription", "new_subscription_count", "new_bit", "new_cheerer"} {
		out[t] = i18n.T(lang, "overlay.goal."+t)
	}
	return out
}

func (a *App) voteLabels() map[string]string {
	lang := a.lang()
	out := map[string]string{}
	for _, k := range []string{"poll", "prediction", "locked", "winner", "canceled", "votes", "points", "users", "ended"} {
		out[k] = i18n.T(lang, "overlay."+k)
	}
	return out
}

// pushOverlays sends the changed overlays' state to OBS.
func (a *App) pushOverlays(goals, vote bool) {
	if !a.obs.Connected() {
		return
	}
	g, v := a.ov.snapshot()
	set := a.cfg.Get().Overlays
	if goals && set.Goals {
		if g == nil {
			g = []goalView{}
		}
		a.emitOverlay(obs.GoalInput, obs.GoalEvent, map[string]any{"goals": g, "labels": a.goalLabels()})
	}
	if vote && set.Votes {
		a.emitOverlay(obs.VoteInput, obs.VoteEvent, map[string]any{"vote": v, "labels": a.voteLabels()})
	}
}

// emitOverlay reloads the page once after darkbot rewrote it, then sends
// data; a problem is logged once, not on every push.
func (a *App) emitOverlay(input, event string, payload any) {
	a.ov.mu.Lock()
	refresh := !a.ov.refreshed[input]
	if a.ov.refreshed == nil {
		a.ov.refreshed = map[string]bool{}
	}
	a.ov.refreshed[input] = true
	a.ov.mu.Unlock()
	if refresh {
		if err := a.obs.RefreshOverlay(input); err != nil {
			a.log.Warnf("Overlay %s: could not reload the page in OBS: %v", input, err)
		}
	}
	data, err := toMap(payload)
	if err == nil {
		err = a.obs.Emit(event, data)
	}
	msg := ""
	if err != nil {
		msg = err.Error()
	}
	a.ov.mu.Lock()
	if a.ov.pushErr == nil {
		a.ov.pushErr = map[string]string{}
	}
	changed := msg != a.ov.pushErr[input]
	a.ov.pushErr[input] = msg
	a.ov.mu.Unlock()
	if changed && err != nil {
		a.log.Warnf("Overlay %s: %v", input, err)
	}
}

// SaveOverlays turns the goal and poll overlays on or off in OBS.
func (a *App) SaveOverlays(s config.Overlays) error {
	was := a.cfg.Get().Overlays
	if err := a.cfg.Update(func(c *config.Config) { c.Overlays = s }); err != nil {
		return a.uiErr(err)
	}
	a.emitState()
	err := a.applyOverlays(was)
	if t := a.tok.Current(); s.Goals && !was.Goals && t != nil {
		if gerr := a.refreshGoals(a.ctx, t); gerr != nil {
			a.log.Warnf("Could not read the goals: %v", gerr)
		}
	}
	a.pushOverlays(true, true)
	return a.uiErr(err)
}

// applyOverlays sets up the overlays that are on and hides the ones
// switched off since was (needs OBS; on connect it runs again).
func (a *App) applyOverlays(was config.Overlays) error {
	if !a.obs.Connected() {
		return nil
	}
	s := a.cfg.Get().Overlays
	a.ov.mu.Lock()
	goalsFile, voteFile := a.ov.goalsPage, a.ov.votePage
	a.ov.mu.Unlock()
	var errs []error
	for _, o := range []struct {
		on, wasOn bool
		ov        obs.Overlay
		file      string
	}{{s.Goals, was.Goals, obs.GoalOverlay, goalsFile}, {s.Votes, was.Votes, obs.VoteOverlay, voteFile}} {
		switch {
		case o.on && o.file == "":
			errs = append(errs, i18n.E("err.npPage"))
		case o.on:
			created, err := a.obs.SetupOverlay(o.ov, o.file, true)
			switch {
			case errors.Is(err, obs.ErrNotSetUp):
				errs = append(errs, i18n.E("err.npNoScene"))
			case errors.Is(err, obs.ErrNoBrowserSource):
				errs = append(errs, i18n.E("err.npNoBrowser"))
			case err != nil:
				errs = append(errs, err)
			case created:
				a.log.Infof("Added the browser source %q to the darkbot scene", o.ov.Input)
			}
		case o.wasOn:
			if err := a.obs.ShowOverlay(o.ov.Input, false); err != nil {
				errs = append(errs, err)
			}
		}
	}
	if len(errs) > 0 {
		return errs[0] // both overlays fail for the same reason (OBS, scene)
	}
	return nil
}

// overlaysOnOBSConnect re-applies the overlays after OBS (re)connected.
func (a *App) overlaysOnOBSConnect() {
	if s := a.cfg.Get().Overlays; !s.Goals && !s.Votes {
		return
	}
	if err := a.applyOverlays(config.Overlays{}); err != nil {
		a.log.Warnf("Overlays: %v", i18n.Localize(err, "en"))
		return
	}
	a.pushOverlays(true, true)
}

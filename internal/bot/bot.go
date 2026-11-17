// Package bot connects redemptions to sounds: it finds the configured sound,
// enforces cooldowns and plays sounds one after another through an Output.
package bot

import (
	"context"
	"strings"
	"sync"
	"time"

	"github.com/miwidot/darkbot/internal/config"
	"github.com/miwidot/darkbot/internal/i18n"
	"github.com/miwidot/darkbot/internal/twitch"
)

// MaxQueue is how many sounds may wait; further redemptions are dropped so a
// raid spamming rewards can't queue up minutes of audio.
const MaxQueue = 20

// Output plays one sound and returns when it finished (or failed).
type Output interface {
	Play(ctx context.Context, s config.Sound, masterVolume float64) error
}

// Logger is what the bot needs for logging.
type Logger interface {
	Infof(format string, args ...any)
	Warnf(format string, args ...any)
	Errorf(format string, args ...any)
}

// Activity kinds reported to the UI.
const (
	ActQueued    = "queued"     // accepted, waiting in the queue
	ActPlaying   = "playing"    // playback started
	ActDone      = "done"       // playback finished
	ActCooldown  = "cooldown"   // ignored: sound still on cooldown
	ActDisabled  = "disabled"   // ignored: sound switched off
	ActQueueFull = "queue_full" // ignored: too many sounds waiting
	ActError     = "error"      // playback failed
)

// Activity is one event for the UI's live feed and pad lights.
type Activity struct {
	Kind        string `json:"kind"`
	SoundID     string `json:"soundId"`
	RewardTitle string `json:"rewardTitle"`
	User        string `json:"user"`
	Test        bool   `json:"test"`
	// Event: the sound is the action of a channel event (follow, sub, ...),
	// not a redemption; stats and pads ignore it.
	Event bool      `json:"event"`
	At    time.Time `json:"at"`
	// CooldownUntil is when the sound is free again (zero = no cooldown).
	CooldownUntil time.Time `json:"cooldownUntil"`
	// CooldownSeconds is the configured global cooldown, for the pad ring.
	CooldownSeconds int    `json:"cooldownSeconds"`
	Message         string `json:"message,omitempty"`
}

// Bot handles redemptions.
type Bot struct {
	cfg        *config.Store
	log        Logger
	output     func() Output // resolved per sound so mode changes apply instantly
	queue      chan job
	cd         *Cooldowns
	onActivity func(Activity)
	// resolve fulfills (true) or rejects (false) a redemption of a reward
	// darkbot manages. Twitch only allows this for rewards darkbot created.
	resolve func(r twitch.Redemption, fulfill bool)
	// localize renders a playback error for the activity feed in the UI
	// language; nil shows the plain (English) error.
	localize func(error) string
}

type job struct {
	sound config.Sound
	user  string
	test  bool
	event bool               // action sound of a channel event
	red   *twitch.Redemption // nil for test plays and events
}

// SetResolver installs the fulfill/reject callback for managed rewards.
// Call before Run.
func (b *Bot) SetResolver(fn func(r twitch.Redemption, fulfill bool)) { b.resolve = fn }

// SetLocalizer sets how errors are worded in activity events. Call before Run.
func (b *Bot) SetLocalizer(fn func(error) string) { b.localize = fn }

func (b *Bot) errText(err error) string {
	if b.localize == nil {
		return err.Error()
	}
	return b.localize(err)
}

// finish fulfills or rejects the redemption when the reward is managed.
func (b *Bot) finish(s config.Sound, r *twitch.Redemption, fulfill bool) {
	if r == nil || !s.Managed || b.resolve == nil {
		return
	}
	go b.resolve(*r, fulfill)
}

// New creates a bot. outputFor returns the Output for the current mode;
// onActivity (may be nil) receives every event for the UI.
func New(cfg *config.Store, log Logger, outputFor func() Output, onActivity func(Activity)) *Bot {
	if onActivity == nil {
		onActivity = func(Activity) {}
	}
	return &Bot{
		cfg:        cfg,
		log:        log,
		output:     outputFor,
		queue:      make(chan job, MaxQueue),
		cd:         NewCooldowns(),
		onActivity: onActivity,
	}
}

func (b *Bot) emit(kind string, s config.Sound, user string, test bool, msg string) {
	b.emitJob(kind, job{sound: s, user: user, test: test}, msg)
}

func (b *Bot) emitJob(kind string, j job, msg string) {
	s := j.sound
	a := Activity{
		Kind: kind, SoundID: s.ID, RewardTitle: s.RewardTitle, User: j.user, Test: j.test, Event: j.event,
		At: time.Now(), CooldownSeconds: s.CooldownSeconds, Message: msg,
	}
	a.CooldownUntil = b.cd.GlobalReadyAt(s.ID)
	b.onActivity(a)
}

// Run plays queued sounds until ctx is cancelled.
func (b *Bot) Run(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case j := <-b.queue:
			b.play(ctx, j)
		}
	}
}

func (b *Bot) play(ctx context.Context, j job) {
	out := b.output()
	if out == nil {
		b.log.Errorf("No audio output available for %q", j.sound.RewardTitle)
		b.emitJob(ActError, j, b.errText(i18n.E("err.noOutput")))
		b.finish(j.sound, j.red, false)
		return
	}
	master := b.cfg.Get().MasterVolume
	b.emitJob(ActPlaying, j, "")
	if err := out.Play(ctx, j.sound, master); err != nil {
		b.log.Errorf("Play %q failed: %v", j.sound.RewardTitle, err)
		b.emitJob(ActError, j, b.errText(err))
		b.finish(j.sound, j.red, false)
		return
	}
	b.emitJob(ActDone, j, "")
	b.finish(j.sound, j.red, true)
}

// HandleRedemption is called for every channel point redemption.
func (b *Bot) HandleRedemption(r twitch.Redemption) {
	cfg := b.cfg.Get()
	s, ok := FindSound(cfg.Sounds, r.RewardID, r.RewardTitle)
	if !ok {
		return // not a sound reward
	}
	s.RewardTitle = r.RewardTitle // show the current Twitch title
	if !s.Enabled {
		b.log.Infof("%s redeemed %q, but the sound is disabled", r.UserName, r.RewardTitle)
		b.emit(ActDisabled, s, r.UserName, false, "")
		b.finish(s, &r, false)
		return
	}
	if wait, blocked := b.cd.Try(s, r.UserID, time.Now()); blocked {
		b.log.Infof("%s redeemed %q, still on cooldown (%s left)", r.UserName, r.RewardTitle, wait.Round(time.Second))
		b.emit(ActCooldown, s, r.UserName, false, wait.Round(time.Second).String())
		b.finish(s, &r, false)
		return
	}
	select {
	case b.queue <- job{sound: s, user: r.UserName, red: &r}:
		b.log.Infof("%s redeemed %q", r.UserName, r.RewardTitle)
		b.emit(ActQueued, s, r.UserName, false, "")
	default:
		b.log.Warnf("%s redeemed %q, but %d sounds are already waiting, skipped", r.UserName, r.RewardTitle, MaxQueue)
		b.emit(ActQueueFull, s, r.UserName, false, "")
		b.finish(s, &r, false)
	}
}

// PlayEvent queues the action sound of a channel event. It shares the queue
// with redemptions, so a sub sound never talks over a redemption.
func (b *Bot) PlayEvent(s config.Sound, user string) {
	select {
	case b.queue <- job{sound: s, user: user, event: true}:
	default:
		b.log.Warnf("Event sound %q skipped, %d sounds are already waiting", s.RewardTitle, MaxQueue)
	}
}

// Test plays a sound immediately, bypassing cooldowns and the queue.
func (b *Bot) Test(ctx context.Context, soundID string) {
	for _, s := range b.cfg.Get().Sounds {
		if s.ID == soundID {
			b.play(ctx, job{sound: s, test: true})
			return
		}
	}
	b.log.Warnf("Test: sound %s not found", soundID)
}

// FindSound matches by reward ID first; falls back to a case-insensitive
// title match for sounds that were configured without a reward ID.
func FindSound(sounds []config.Sound, rewardID, title string) (config.Sound, bool) {
	for _, s := range sounds {
		if s.RewardID != "" && s.RewardID == rewardID {
			return s, true
		}
	}
	for _, s := range sounds {
		if s.RewardID == "" && s.RewardTitle != "" && strings.EqualFold(strings.TrimSpace(s.RewardTitle), strings.TrimSpace(title)) {
			return s, true
		}
	}
	return config.Sound{}, false
}

// Cooldowns tracks when a sound (globally and per user) may play again.
// Cooldowns start when a redemption is accepted (not when playback ends), so
// they behave like Twitch's own reward cooldown.
type Cooldowns struct {
	mu     sync.Mutex
	global map[string]time.Time // soundID -> ready at
	user   map[string]time.Time // soundID+"|"+userID -> ready at
}

// NewCooldowns creates an empty tracker.
func NewCooldowns() *Cooldowns {
	return &Cooldowns{global: map[string]time.Time{}, user: map[string]time.Time{}}
}

// Try checks and, if allowed, starts the cooldowns. It returns how long the
// caller still has to wait when blocked.
func (c *Cooldowns) Try(s config.Sound, userID string, now time.Time) (time.Duration, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()

	userKey := s.ID + "|" + userID
	if ready, ok := c.global[s.ID]; ok && now.Before(ready) {
		return ready.Sub(now), true
	}
	if ready, ok := c.user[userKey]; ok && now.Before(ready) {
		return ready.Sub(now), true
	}
	if s.CooldownSeconds > 0 {
		c.global[s.ID] = now.Add(time.Duration(s.CooldownSeconds) * time.Second)
	}
	if s.UserCooldownSeconds > 0 {
		c.user[userKey] = now.Add(time.Duration(s.UserCooldownSeconds) * time.Second)
	}
	c.pruneLocked(now)
	return 0, false
}

// GlobalReadyAt returns when the sound's global cooldown ends (zero = free).
func (c *Cooldowns) GlobalReadyAt(soundID string) time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	t := c.global[soundID]
	if !time.Now().Before(t) {
		return time.Time{}
	}
	return t
}

// pruneLocked drops expired entries so the maps don't grow forever.
func (c *Cooldowns) pruneLocked(now time.Time) {
	for k, t := range c.global {
		if !now.Before(t) {
			delete(c.global, k)
		}
	}
	for k, t := range c.user {
		if !now.Before(t) {
			delete(c.user, k)
		}
	}
}

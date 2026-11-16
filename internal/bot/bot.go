// Package bot connects redemptions to sounds: it finds the configured sound,
// enforces cooldowns and plays sounds one after another through an Output.
package bot

import (
	"context"
	"strings"
	"sync"
	"time"

	"github.com/miwi/darkbot/internal/config"
	"github.com/miwi/darkbot/internal/twitch"
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

// Bot handles redemptions.
type Bot struct {
	cfg    *config.Store
	log    Logger
	output func() Output // resolved per sound so mode changes apply instantly
	queue  chan job
	cd     *Cooldowns
}

type job struct {
	sound config.Sound
	user  string
}

// New creates a bot. outputFor returns the Output for the current mode.
func New(cfg *config.Store, log Logger, outputFor func() Output) *Bot {
	return &Bot{
		cfg:    cfg,
		log:    log,
		output: outputFor,
		queue:  make(chan job, MaxQueue),
		cd:     NewCooldowns(),
	}
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
		return
	}
	master := b.cfg.Get().MasterVolume
	if err := out.Play(ctx, j.sound, master); err != nil {
		b.log.Errorf("Play %q failed: %v", j.sound.RewardTitle, err)
	}
}

// HandleRedemption is called for every channel point redemption.
func (b *Bot) HandleRedemption(r twitch.Redemption) {
	cfg := b.cfg.Get()
	s, ok := FindSound(cfg.Sounds, r.RewardID, r.RewardTitle)
	if !ok {
		return // not a sound reward
	}
	if !s.Enabled {
		b.log.Infof("%s redeemed %q, but the sound is disabled", r.UserName, r.RewardTitle)
		return
	}
	if wait, blocked := b.cd.Try(s, r.UserID, time.Now()); blocked {
		b.log.Infof("%s redeemed %q, still on cooldown (%s left)", r.UserName, r.RewardTitle, wait.Round(time.Second))
		return
	}
	select {
	case b.queue <- job{sound: s, user: r.UserName}:
		b.log.Infof("%s redeemed %q", r.UserName, r.RewardTitle)
	default:
		b.log.Warnf("%s redeemed %q, but %d sounds are already waiting, skipped", r.UserName, r.RewardTitle, MaxQueue)
	}
}

// Test plays a sound immediately, bypassing cooldowns and the queue.
func (b *Bot) Test(ctx context.Context, soundID string) {
	for _, s := range b.cfg.Get().Sounds {
		if s.ID == soundID {
			b.play(ctx, job{sound: s, user: "test"})
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

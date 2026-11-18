package chatbot

import (
	"context"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/miwidot/darkbot/internal/config"
	"github.com/miwidot/darkbot/internal/twitch"
)

type nopLog struct{}

func (nopLog) Infof(string, ...any) {}
func (nopLog) Warnf(string, ...any) {}

// rig is a bot with a fake clock, chat and live state.
type rig struct {
	*Bot
	mu     sync.Mutex
	now    time.Time
	cfg    config.Chat
	live   bool
	out    []string
	played []string
}

func newRig(cfg config.Chat) *rig {
	r := &rig{now: time.Date(2026, 11, 18, 20, 0, 0, 0, time.UTC), cfg: cfg, live: true}
	r.Bot = New(Deps{
		Settings: func() config.Chat { r.mu.Lock(); defer r.mu.Unlock(); return r.cfg },
		Send:     func(_ context.Context, text string) { r.mu.Lock(); r.out = append(r.out, text); r.mu.Unlock() },
		Play:     func(c config.ChatCommand, _ string) { r.played = append(r.played, c.Name) },
		Live:     func() bool { return r.live },
		Now:      func() time.Time { return r.now },
		Log:      nopLog{},
	})
	return r
}

func (r *rig) advance(d time.Duration) { r.now = r.now.Add(d) }

func (r *rig) say(m twitch.ChatMessage) []string {
	before := len(r.out)
	r.Handle(context.Background(), m)
	return r.out[before:]
}

func viewer(text string) twitch.ChatMessage {
	return twitch.ChatMessage{ChatterID: "v1", ChatterName: "Anna", Text: text}
}

func cmds(c ...config.ChatCommand) config.Chat {
	return config.Chat{CommandsEnabled: true, Commands: c}
}

var discord = config.ChatCommand{ID: "d", Name: "discord", Aliases: []string{"dc"}, Response: "Join: discord.gg/x, {user}!", Permission: config.PermEveryone, Enabled: true}

func TestCommandAnswersNameAndAlias(t *testing.T) {
	r := newRig(cmds(discord))
	if got := r.say(viewer("!discord")); !slices.Equal(got, []string{"Join: discord.gg/x, Anna!"}) {
		t.Fatalf("got %v", got)
	}
	if got := r.say(viewer("  !DC please")); len(got) != 1 {
		t.Fatalf("alias, any case: got %v", got)
	}
	for _, text := range []string{"discord", "!discordx", "hey !discord", "!"} {
		if got := r.say(viewer(text)); len(got) != 0 {
			t.Errorf("%q answered %v", text, got)
		}
	}
}

func TestCommandsOffByDefault(t *testing.T) {
	c := cmds(discord)
	c.CommandsEnabled = false
	r := newRig(c)
	if got := r.say(viewer("!discord")); len(got) != 0 {
		t.Fatalf("commands are off: got %v", got)
	}
	off := discord
	off.Enabled = false
	r = newRig(cmds(off))
	if got := r.say(viewer("!discord")); len(got) != 0 {
		t.Fatalf("command is off: got %v", got)
	}
}

func TestPermissions(t *testing.T) {
	who := map[string]twitch.ChatMessage{
		"viewer":      {ChatterID: "1"},
		"subscriber":  {ChatterID: "2", IsSubscriber: true},
		"vip":         {ChatterID: "3", IsVIP: true},
		"moderator":   {ChatterID: "4", IsModerator: true},
		"broadcaster": {ChatterID: "5", IsBroadcaster: true},
	}
	order := []string{"viewer", "subscriber", "vip", "moderator", "broadcaster"}
	for pi, p := range config.Permissions {
		for wi, w := range order {
			if got, want := Allowed(p, who[w]), wi >= pi; got != want {
				t.Errorf("%s using a %s command: %v, want %v", w, p, got, want)
			}
		}
	}
	if Allowed("bogus", who["broadcaster"]) {
		t.Error("an unknown permission must allow nobody")
	}
}

func TestCooldowns(t *testing.T) {
	c := discord
	c.CooldownSeconds, c.UserCooldownSeconds = 10, 60
	r := newRig(cmds(c))
	other := twitch.ChatMessage{ChatterID: "v2", ChatterName: "Ben", Text: "!discord"}
	if len(r.say(viewer("!discord"))) != 1 {
		t.Fatal("first use")
	}
	if len(r.say(other)) != 0 {
		t.Fatal("global cooldown must block everyone")
	}
	r.advance(11 * time.Second)
	if len(r.say(viewer("!discord"))) != 0 {
		t.Fatal("user cooldown must still block Anna")
	}
	if len(r.say(other)) != 1 {
		t.Fatal("Ben may use it after the global cooldown")
	}
	streamer := twitch.ChatMessage{ChatterID: "s", ChatterName: "Streamer", Text: "!discord", IsBroadcaster: true}
	if len(r.say(streamer)) != 1 || len(r.say(streamer)) != 1 {
		t.Fatal("the broadcaster skips cooldowns")
	}
}

func TestTargetTakesOnlyALogin(t *testing.T) {
	m := viewer("")
	for arg, want := range map[string]string{
		"@Ben":               "hug Ben",
		"ben_99 extra words": "hug ben_99",
		"":                   "hug Anna",
		"/ban someone":       "hug Anna",
		"<script>":           "hug Anna",
		"@":                  "hug Anna",
	} {
		if got := Render("hug {target}", m, arg); got != want {
			t.Errorf("arg %q: %q, want %q", arg, got, want)
		}
	}
}

func TestOwnMessagesTriggerNothing(t *testing.T) {
	echo := config.ChatCommand{ID: "e", Name: "a", Response: "!a", Permission: config.PermEveryone, Enabled: true}
	r := newRig(cmds(echo))
	streamer := twitch.ChatMessage{ChatterID: "s", ChatterName: "S", Text: "!a", IsBroadcaster: true}
	if len(r.say(streamer)) != 1 {
		t.Fatal("the streamer's !a answers")
	}
	// The answer "!a" comes back through chat as the streamer's message.
	if got := r.say(streamer); len(got) != 0 {
		t.Fatalf("darkbot's own answer triggered %v (a loop)", got)
	}
	// Typed again by the streamer, it works.
	if len(r.say(streamer)) != 1 {
		t.Fatal("a real second message must answer")
	}
}

func TestAnswerCap(t *testing.T) {
	r := newRig(cmds(discord))
	n := 0
	for i := range maxReplies + 5 {
		m := twitch.ChatMessage{ChatterID: string(rune('a' + i)), ChatterName: "x", Text: "!discord"}
		n += len(r.say(m))
	}
	if n != maxReplies {
		t.Fatalf("%d answers, want the cap %d", n, maxReplies)
	}
	r.advance(replyWindow)
	if len(r.say(viewer("!discord"))) != 1 {
		t.Fatal("after the window answers resume")
	}
}

func TestCommandSound(t *testing.T) {
	c := discord
	c.Response, c.Sound = "", "/airhorn.mp3"
	r := newRig(cmds(c))
	if got := r.say(viewer("!discord")); len(got) != 0 || !slices.Equal(r.played, []string{"discord"}) {
		t.Fatalf("sound-only command: chat %v played %v", got, r.played)
	}
}

// ---------- timers ----------

func timers(t ...config.ChatTimer) config.Chat {
	return config.Chat{TimersEnabled: true, Timers: t}
}

// tick runs the timer check every timerTick for d and returns the posts.
func (r *rig) tick(d time.Duration) []string {
	var out []string
	for end := r.now.Add(d); r.now.Before(end); r.advance(timerTick) {
		if tm, ok := r.dueTimer(); ok {
			out = append(out, tm.Message)
		}
	}
	return out
}

func TestTimerPostsEveryIntervalWhileLive(t *testing.T) {
	r := newRig(timers(config.ChatTimer{ID: "f", Message: "Follow!", IntervalMinutes: 15, Enabled: true}))
	if got := r.tick(14 * time.Minute); len(got) != 0 {
		t.Fatalf("posted before its interval: %v", got)
	}
	if got := r.tick(2 * time.Minute); !slices.Equal(got, []string{"Follow!"}) {
		t.Fatalf("got %v", got)
	}
	if got := r.tick(46 * time.Minute); len(got) != 3 {
		t.Fatalf("3 more in 46 minutes, got %d", len(got))
	}
	r.live = false
	if got := r.tick(time.Hour); len(got) != 0 {
		t.Fatalf("offline: %v", got)
	}
	r.live = true // going live: a full interval first
	if got := r.tick(14 * time.Minute); len(got) != 0 {
		t.Fatalf("right after going live: %v", got)
	}
}

func TestTimersOff(t *testing.T) {
	c := timers(config.ChatTimer{ID: "f", Message: "x", IntervalMinutes: 5, Enabled: true})
	c.TimersEnabled = false
	r := newRig(c)
	if got := r.tick(time.Hour); len(got) != 0 {
		t.Fatalf("timers are off: %v", got)
	}
}

func TestTimerWaitsForChat(t *testing.T) {
	r := newRig(timers(config.ChatTimer{ID: "f", Message: "x", IntervalMinutes: 5, MinLines: 3, Enabled: true}))
	r.tick(timerTick) // starts waiting
	if got := r.tick(time.Hour); len(got) != 0 {
		t.Fatalf("empty chat: %v", got)
	}
	for range 3 {
		r.say(viewer("hello"))
	}
	if got := r.tick(timerTick); len(got) != 1 {
		t.Fatalf("after 3 messages: %v", got)
	}
}

func TestTimersTakeTurns(t *testing.T) {
	r := newRig(timers(
		config.ChatTimer{ID: "a", Message: "A", IntervalMinutes: 5, Enabled: true},
		config.ChatTimer{ID: "b", Message: "B", IntervalMinutes: 5, Enabled: true},
	))
	// Both are due at 5 minutes: never two within timerGap.
	got := r.tick(5*time.Minute + timerGap + timerTick)
	if !slices.Equal(got, []string{"A", "B"}) {
		t.Fatalf("got %v, want A then B a minute apart", got)
	}
}

func TestDisabledTimerWaitsAfterTurnedOn(t *testing.T) {
	c := timers(config.ChatTimer{ID: "a", Message: "A", IntervalMinutes: 5})
	r := newRig(c)
	r.tick(time.Hour)
	r.mu.Lock()
	r.cfg.Timers[0].Enabled = true
	r.mu.Unlock()
	if got := r.tick(4 * time.Minute); len(got) != 0 {
		t.Fatalf("posted right after being turned on: %v", got)
	}
}

func TestNormalizeCommand(t *testing.T) {
	reserved := []string{"sr", "skip"}
	base := config.ChatCommand{ID: "n", Name: " !Lurk ", Aliases: []string{"!AFK", "lurk", "afk", ""}, Response: "{user}\n is  lurking", Permission: config.PermEveryone}
	c, err := NormalizeCommand(base, []config.ChatCommand{discord}, reserved)
	if err != nil || c.Name != "lurk" || !slices.Equal(c.Aliases, []string{"afk"}) || c.Response != "{user} is lurking" || c.Volume != config.DefaultSoundVolume {
		t.Fatalf("got %+v %v", c, err)
	}
	if c, err := NormalizeCommand(config.ChatCommand{Name: "würfel", Response: "x", Permission: config.PermEveryone}, nil, nil); err != nil {
		t.Fatalf("letters of any language: %+v %v", c, err)
	}
	bad := map[string]func(*config.ChatCommand){
		"empty name":     func(c *config.ChatCommand) { c.Name = "!" },
		"space in name":  func(c *config.ChatCommand) { c.Name = "a b" },
		"no answer":      func(c *config.ChatCommand) { c.Response = "" },
		"reserved":       func(c *config.ChatCommand) { c.Name = "sr" },
		"reserved alias": func(c *config.ChatCommand) { c.Aliases = []string{"skip"} },
		"taken":          func(c *config.ChatCommand) { c.Name = "discord" },
		"taken alias":    func(c *config.ChatCommand) { c.Aliases = []string{"dc"} },
		"permission":     func(c *config.ChatCommand) { c.Permission = "admin" },
		"cooldown":       func(c *config.ChatCommand) { c.CooldownSeconds = config.MaxCommandCooldown + 1 },
		"too long":       func(c *config.ChatCommand) { c.Response = strings.Repeat("x", config.MaxChatTemplate+1) },
	}
	for name, edit := range bad {
		c := base
		edit(&c)
		if _, err := NormalizeCommand(c, []config.ChatCommand{discord}, reserved); err == nil {
			t.Errorf("%s: must be refused", name)
		}
	}
	// Editing a command keeps its own name.
	if _, err := NormalizeCommand(discord, []config.ChatCommand{discord}, reserved); err != nil {
		t.Fatalf("re-saving: %v", err)
	}
}

func TestNormalizeTimer(t *testing.T) {
	if tm, err := NormalizeTimer(config.ChatTimer{Message: " Follow \n me ", IntervalMinutes: 15}); err != nil || tm.Message != "Follow me" {
		t.Fatalf("%+v %v", tm, err)
	}
	for _, tm := range []config.ChatTimer{
		{Message: "", IntervalMinutes: 15},
		{Message: "x", IntervalMinutes: config.MinTimerMinutes - 1},
		{Message: "x", IntervalMinutes: config.MaxTimerMinutes + 1},
		{Message: "x", IntervalMinutes: 15, MinLines: -1},
	} {
		if _, err := NormalizeTimer(tm); err == nil {
			t.Errorf("%+v must be refused", tm)
		}
	}
}

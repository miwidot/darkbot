// Package chatbot runs the streamer's own chat commands ("!discord") and
// timed messages. Both are off until the streamer turns them on.
//
// Twitch (checked 2026-11-18):
//   - Chat arrives through EventSub channel.chat.message; roles come from
//     the message's badges, read per message
//     (https://dev.twitch.tv/docs/chat/send-receive-messages/).
//   - The broadcaster may send 100 messages per 30 s; going over makes
//     Twitch ignore the sender's messages for an hour
//     (https://dev.twitch.tv/docs/chat/). Answers are capped well below.
//   - darkbot writes as the broadcaster, so its own messages come back
//     through the chat subscription; they are recognised and ignored.
package chatbot

import (
	"context"
	"regexp"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/miwidot/darkbot/internal/config"
	"github.com/miwidot/darkbot/internal/twitch"
)

const (
	// Prefix starts every command.
	Prefix = "!"
	// timerTick: how often timers are checked; a timer may post this much
	// later than its interval.
	timerTick = 15 * time.Second
	// timerGap: at least this long between two timer posts, so timers that
	// are due together go out one after another instead of in a burst.
	timerGap = time.Minute
	// maxReplies per replyWindow caps command answers far below Twitch's
	// 100 messages per 30 s, so chat spam can't use up the budget.
	maxReplies  = 20
	replyWindow = 30 * time.Second
	// echoWindow: for this long a broadcaster message equal to one darkbot
	// sent counts as darkbot's own and triggers nothing.
	echoWindow = 30 * time.Second
)

var (
	// namePattern: a command name or alias, lowercase, without "!".
	// Letters of any language (e.g. !würfel), digits and "_".
	namePattern = regexp.MustCompile(`^[\p{Ll}\p{Lo}\p{N}_]{1,25}$`)
	// loginPattern: a Twitch login (letters, digits, "_", at most 25), the
	// only viewer text {target} takes over.
	loginPattern = regexp.MustCompile(`^[A-Za-z0-9_]{1,25}$`)
)

// Logger is the logging this package needs.
type Logger interface {
	Infof(format string, args ...any)
	Warnf(format string, args ...any)
}

// Deps connects the bot to the app.
type Deps struct {
	Settings func() config.Chat
	// Send posts a message in the streamer's chat.
	Send func(ctx context.Context, text string)
	// Play queues a command's sound with the other sounds.
	Play func(cmd config.ChatCommand, user string)
	// Used counts a command use for the dashboard (optional).
	Used func()
	// Live reports whether the stream is live (timers run only then).
	Live func() bool
	Now  func() time.Time
	Log  Logger
}

// Bot answers commands and posts timers.
type Bot struct {
	d Deps

	mu       sync.Mutex
	used     map[string]time.Time // command ID -> last use
	usedBy   map[string]time.Time // command ID + "|" + user ID -> last use
	replies  []time.Time          // command answers within replyWindow
	sent     []sentMsg            // darkbot's messages within echoWindow
	lines    int                  // chat messages seen (darkbot's own excluded)
	timers   map[string]timerState
	lastPost time.Time // last timer post
	running  bool      // timers were active at the last tick
}

type sentMsg struct {
	text string
	at   time.Time
}

type timerState struct {
	last  time.Time // last post, or when the timer started waiting
	lines int       // Bot.lines at that moment
}

// New creates a bot. Call Run for the timers and Handle for each message.
func New(d Deps) *Bot {
	if d.Now == nil {
		d.Now = time.Now
	}
	return &Bot{d: d, used: map[string]time.Time{}, usedBy: map[string]time.Time{}, timers: map[string]timerState{}}
}

// Handle looks at one chat message: it counts for the timers' minimum and
// may be a command.
func (b *Bot) Handle(ctx context.Context, m twitch.ChatMessage) {
	if b.ownEcho(m) {
		return
	}
	b.mu.Lock()
	b.lines++
	b.mu.Unlock()

	s := b.d.Settings()
	if !s.CommandsEnabled {
		return
	}
	name, arg := split(m.Text)
	if name == "" {
		return
	}
	cmd, ok := Find(s.Commands, name)
	if !ok || !cmd.Enabled || !Allowed(cmd.Permission, m) {
		return
	}
	text := Render(cmd.Response, m, arg)
	if !b.take(cmd, m, text != "") {
		return
	}
	b.d.Log.Infof("%s used %s%s", m.ChatterName, Prefix, cmd.Name)
	if b.d.Used != nil {
		b.d.Used()
	}
	if text != "" {
		b.send(ctx, text)
	}
	if cmd.Sound != "" && b.d.Play != nil {
		b.d.Play(cmd, m.ChatterName)
	}
}

// split returns the command name (lowercase, without "!") and the rest.
func split(text string) (name, arg string) {
	text = strings.TrimSpace(text)
	if !strings.HasPrefix(text, Prefix) {
		return "", ""
	}
	first, rest, _ := strings.Cut(text[len(Prefix):], " ")
	return strings.ToLower(first), strings.TrimSpace(rest)
}

// Find returns the command answering to name (a name or an alias).
func Find(cmds []config.ChatCommand, name string) (config.ChatCommand, bool) {
	for _, c := range cmds {
		if c.Name == name || slices.Contains(c.Aliases, name) {
			return c, true
		}
	}
	return config.ChatCommand{}, false
}

// level ranks a chatter; each permission needs at least its rank. VIPs
// rank above subscribers, as in common chat bots.
func level(m twitch.ChatMessage) int {
	switch {
	case m.IsBroadcaster:
		return 4
	case m.IsModerator:
		return 3
	case m.IsVIP:
		return 2
	case m.IsSubscriber:
		return 1
	}
	return 0
}

// Allowed reports whether the chatter may use a command with permission p.
func Allowed(p config.Permission, m twitch.ChatMessage) bool {
	need := slices.Index(config.Permissions, p)
	return need >= 0 && level(m) >= need
}

// Render fills {user} (the chatter) and {target} (the login after the
// command, "@" removed, else the chatter). Other viewer text is never
// inserted, so nobody can make the streamer's account say anything.
func Render(tmpl string, m twitch.ChatMessage, arg string) string {
	target := m.ChatterName
	if first, _, _ := strings.Cut(arg, " "); first != "" {
		if login := strings.TrimPrefix(first, "@"); loginPattern.MatchString(login) {
			target = login
		}
	}
	r := strings.NewReplacer("{user}", m.ChatterName, "{target}", target)
	return strings.TrimSpace(r.Replace(tmpl))
}

// take checks the cooldowns and the answer cap and, if the command may
// run, starts its cooldowns. The broadcaster skips the cooldowns.
func (b *Bot) take(cmd config.ChatCommand, m twitch.ChatMessage, answers bool) bool {
	now := b.d.Now()
	key := cmd.ID + "|" + m.ChatterID
	b.mu.Lock()
	defer b.mu.Unlock()
	if !m.IsBroadcaster {
		if last, ok := b.used[cmd.ID]; ok && now.Sub(last) < time.Duration(cmd.CooldownSeconds)*time.Second {
			return false
		}
		if last, ok := b.usedBy[key]; ok && now.Sub(last) < time.Duration(cmd.UserCooldownSeconds)*time.Second {
			return false
		}
	}
	if answers {
		b.replies = slices.DeleteFunc(b.replies, func(t time.Time) bool { return now.Sub(t) >= replyWindow })
		if len(b.replies) >= maxReplies {
			b.d.Log.Warnf("Command %s%s skipped: %d answers in the last %s", Prefix, cmd.Name, maxReplies, replyWindow)
			return false
		}
		b.replies = append(b.replies, now)
	}
	b.used[cmd.ID] = now
	b.usedBy[key] = now
	b.forgetOld(now)
	return true
}

// forgetOld drops per-viewer cooldowns that are over for every command
// (the longest possible cooldown), so the map can't grow without end.
func (b *Bot) forgetOld(now time.Time) {
	for k, t := range b.usedBy {
		if now.Sub(t) > config.MaxCommandCooldown*time.Second {
			delete(b.usedBy, k)
		}
	}
}

// send posts text and remembers it to recognise its echo.
func (b *Bot) send(ctx context.Context, text string) {
	text = twitch.TruncateChat(text)
	b.mu.Lock()
	b.sent = append(b.sent, sentMsg{text: text, at: b.d.Now()})
	b.mu.Unlock()
	b.d.Send(ctx, text)
}

// ownEcho reports (and forgets) a message darkbot sent itself.
func (b *Bot) ownEcho(m twitch.ChatMessage) bool {
	if !m.IsBroadcaster {
		return false
	}
	now := b.d.Now()
	text := strings.TrimSpace(m.Text)
	b.mu.Lock()
	defer b.mu.Unlock()
	b.sent = slices.DeleteFunc(b.sent, func(s sentMsg) bool { return now.Sub(s.at) >= echoWindow })
	i := slices.IndexFunc(b.sent, func(s sentMsg) bool { return s.text == text })
	if i < 0 {
		return false
	}
	b.sent = slices.Delete(b.sent, i, i+1)
	return true
}

// Run posts the timers until ctx ends.
func (b *Bot) Run(ctx context.Context) {
	tick := time.NewTicker(timerTick)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
			if t, ok := b.dueTimer(); ok {
				b.d.Log.Infof("Timer message posted")
				b.send(ctx, t.Message)
			}
		}
	}
}

// dueTimer picks the timer to post now, if any: timers run only while
// live and turned on; each waits its interval and its minimum of chat
// messages; of several due ones the longest waiting goes first, and
// never two within timerGap.
func (b *Bot) dueTimer() (config.ChatTimer, bool) {
	s := b.d.Settings()
	active := s.TimersEnabled && b.d.Live()
	now := b.d.Now()
	b.mu.Lock()
	defer b.mu.Unlock()
	if !active {
		b.running = false
		return config.ChatTimer{}, false
	}
	if !b.running {
		// Just went live (or timers were turned on): every timer waits a
		// full interval first instead of all posting at once.
		b.running = true
		clear(b.timers)
	}
	var pick *config.ChatTimer
	var pickSince time.Time
	seen := map[string]bool{}
	for i := range s.Timers {
		t := &s.Timers[i]
		seen[t.ID] = true
		st, ok := b.timers[t.ID]
		if !ok || !t.Enabled {
			// New or off: (re)starts waiting from now.
			b.timers[t.ID] = timerState{last: now, lines: b.lines}
			continue
		}
		if now.Sub(st.last) < time.Duration(t.IntervalMinutes)*time.Minute || b.lines-st.lines < t.MinLines {
			continue
		}
		if pick == nil || st.last.Before(pickSince) {
			pick, pickSince = t, st.last
		}
	}
	for id := range b.timers {
		if !seen[id] {
			delete(b.timers, id) // deleted timer
		}
	}
	if pick == nil || (!b.lastPost.IsZero() && now.Sub(b.lastPost) < timerGap) {
		return config.ChatTimer{}, false
	}
	b.timers[pick.ID] = timerState{last: now, lines: b.lines}
	b.lastPost = now
	return *pick, true
}

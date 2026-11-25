// Package chatbot runs the streamer's own chat commands ("!discord", or a
// word at the start of a message like "hi") and timed messages. Both are
// off until the streamer turns them on.
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
	"math/rand/v2"
	"regexp"
	"slices"
	"strings"
	"sync"
	"time"
	"unicode"

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
	// Send posts a message in the streamer's chat (already remembered as
	// darkbot's own, see Remember).
	Send func(ctx context.Context, text string)
	// Play queues a command's sound with the other sounds.
	Play func(cmd config.ChatCommand, user string)
	// Used counts a command use for the dashboard (optional).
	Used func()
	// Live reports whether the stream is live (timers and greetings run
	// only then).
	Live func() bool
	// IsBot reports whether a chatter is a known bot, which is never
	// greeted (optional).
	IsBot func(login, name string) bool
	Now   func() time.Time
	Log   Logger
}

// Bot answers commands and posts timers.
type Bot struct {
	d Deps

	mu       sync.Mutex
	used     map[string]time.Time // command ID -> last use
	usedBy   map[string]time.Time // command ID + "|" + user ID -> last use
	answered map[string]bool      // command ID + "|" + user ID, this stream (OncePerStream)
	lines    *Lines               // answer files
	replies  []time.Time          // command answers within replyWindow
	sent     []sentMsg            // darkbot's messages within echoWindow
	seen     int                  // chat messages seen (darkbot's own excluded)
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
	// The deck the timer's messages are dealt from: deck holds the indexes
	// still to post this round, dealt for msgs; prev is the index posted
	// last (noPrev = none yet).
	deck []int
	msgs []string
	prev int
}

const noPrev = -1

// next takes the message to post from the deck and deals a new round when
// it is empty. Changed messages start a fresh deck. In random order a new
// round never starts with the message that ended the last one.
func (st *timerState) next(t config.ChatTimer) string {
	msgs := t.Lines()
	if !slices.Equal(st.msgs, msgs) {
		st.msgs, st.deck, st.prev = slices.Clone(msgs), nil, noPrev
	}
	if len(st.deck) == 0 {
		st.deck = deal(len(msgs), t.Order, st.prev)
	}
	i := st.deck[0]
	st.deck, st.prev = st.deck[1:], i
	return msgs[i]
}

// deal returns one round of n message indexes: top to bottom for
// TimerInOrder, shuffled otherwise with prev moved off the first place.
func deal(n int, order string, prev int) []int {
	deck := make([]int, n)
	for i := range deck {
		deck[i] = i
	}
	if order == config.TimerInOrder {
		return deck
	}
	rand.Shuffle(n, func(i, j int) { deck[i], deck[j] = deck[j], deck[i] })
	if n > 1 && deck[0] == prev {
		j := 1 + rand.IntN(n-1)
		deck[0], deck[j] = deck[j], deck[0]
	}
	return deck
}

// New creates a bot. Call Run for the timers and Handle for each message.
func New(d Deps) *Bot {
	if d.Now == nil {
		d.Now = time.Now
	}
	return &Bot{d: d, used: map[string]time.Time{}, usedBy: map[string]time.Time{}, answered: map[string]bool{}, lines: NewLines(), timers: map[string]timerState{}}
}

// NewStream starts a new stream: OncePerStream commands answer everyone
// again. Call when the stream goes live.
func (b *Bot) NewStream() {
	b.mu.Lock()
	clear(b.answered)
	b.mu.Unlock()
}

// Handle looks at one chat message: it counts for the timers' minimum and
// may be a command.
func (b *Bot) Handle(ctx context.Context, m twitch.ChatMessage) {
	if b.ownEcho(m) {
		return
	}
	b.mu.Lock()
	b.seen++
	b.mu.Unlock()

	s := b.d.Settings()
	if !s.CommandsEnabled {
		return
	}
	// A first message is greeted and may still be a command ("!discord").
	if g, ok := b.greeting(s.Commands, m); ok {
		b.run(ctx, g, m, "")
	}
	cmd, arg, ok := match(s.Commands, m)
	if !ok || !cmd.Enabled || !Allowed(cmd.Permission, m) {
		return
	}
	b.run(ctx, cmd, m, arg)
}

// greeting returns the greeting for m: the turned-on TriggerFirst command,
// while live, for a viewer it may greet (not the broadcaster, not a known
// bot). Whether this is the viewer's first message is take's
// once-per-stream rule, so a greeting a full answer cap held back comes
// with the viewer's next message.
func (b *Bot) greeting(cmds []config.ChatCommand, m twitch.ChatMessage) (config.ChatCommand, bool) {
	if m.IsBroadcaster || !b.d.Live() || (b.d.IsBot != nil && b.d.IsBot(m.ChatterLogin, m.ChatterName)) {
		return config.ChatCommand{}, false
	}
	i := slices.IndexFunc(cmds, func(c config.ChatCommand) bool { return Trigger(c) == config.TriggerFirst && c.Enabled })
	if i < 0 || !Allowed(cmds[i].Permission, m) {
		return config.ChatCommand{}, false
	}
	g := cmds[i]
	g.OncePerStream = true
	return g, true
}

// run answers a command (or greeting) for m: its text, its sound, within
// its cooldowns.
func (b *Bot) run(ctx context.Context, cmd config.ChatCommand, m twitch.ChatMessage, arg string) {
	tmpl := cmd.Response
	if cmd.ResponseFile != "" {
		line, err := b.lines.Pick(cmd.ResponseFile)
		if err != nil {
			b.d.Log.Warnf("Command %s: no answer from %s: %v", Display(cmd), cmd.ResponseFile, err)
		}
		tmpl = line
	}
	if Trigger(cmd) == config.TriggerFirst {
		tmpl = WithName(tmpl)
	}
	text := Render(tmpl, m, arg)
	if text == "" && cmd.Sound == "" {
		return
	}
	if !b.take(cmd, m, text != "") {
		return
	}
	b.d.Log.Infof("%s used %s", m.ChatterName, Display(cmd))
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

// match finds the command a message triggers, and the text after the
// trigger. A "!" message can only be a command; any other message may
// start with a word trigger. The broadcaster's own words trigger nothing:
// their "hi" to chat must not make their account answer itself.
func match(cmds []config.ChatCommand, m twitch.ChatMessage) (config.ChatCommand, string, bool) {
	if name, arg := split(m.Text); name != "" {
		cmd, ok := Find(cmds, config.TriggerCommand, name)
		return cmd, arg, ok
	}
	if m.IsBroadcaster {
		return config.ChatCommand{}, "", false
	}
	word, arg := firstWord(m.Text)
	if word == "" {
		return config.ChatCommand{}, "", false
	}
	cmd, ok := Find(cmds, config.TriggerWord, word)
	return cmd, arg, ok
}

// firstWord returns a message's first word, lowercase and without the
// punctuation around it ("Hi!" and "hallo," count as "hi" and "hallo"),
// and the rest of the message.
func firstWord(text string) (word, rest string) {
	first, rest, _ := strings.Cut(strings.TrimSpace(text), " ")
	word = strings.TrimFunc(first, func(r rune) bool { return unicode.IsPunct(r) || unicode.IsSymbol(r) })
	return strings.ToLower(word), strings.TrimSpace(rest)
}

// Trigger is a command's trigger; settings from before word triggers have
// none and are "!" commands.
func Trigger(c config.ChatCommand) string {
	if c.Trigger == config.TriggerWord || c.Trigger == config.TriggerFirst {
		return c.Trigger
	}
	return config.TriggerCommand
}

// greetingName names the greeting in logs, it has no name of its own.
const greetingName = "the greeting"

// Display names a command as chat sees it: "!discord", or "hi" for a word.
func Display(c config.ChatCommand) string {
	switch Trigger(c) {
	case config.TriggerWord:
		return c.Name
	case config.TriggerFirst:
		return greetingName
	}
	return Prefix + c.Name
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

// Find returns the command with that trigger answering to name (a name or
// an alias).
func Find(cmds []config.ChatCommand, trigger, name string) (config.ChatCommand, bool) {
	for _, c := range cmds {
		if Trigger(c) != trigger {
			continue
		}
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

// meAction is the "/me " that makes a chat message an action; the name
// goes after it.
const meAction = "/me "

// afterName: a greeting starting with one of these was written to follow
// the name directly (", welcome!", "! Hi").
const afterName = ",.!?:;"

// WithName puts {user} in front of a greeting that names nobody, so every
// greeting greets someone. Lines written for Streamer.bot's "%user%" +
// line (", welcome!") get the name right before their punctuation; other
// text gets "{user}, " in front. An empty greeting stays empty.
func WithName(tmpl string) string {
	tmpl = strings.TrimSpace(tmpl)
	if tmpl == "" || strings.Contains(tmpl, "{user}") || strings.Contains(tmpl, "{target}") {
		return tmpl
	}
	prefix := ""
	if len(tmpl) >= len(meAction) && strings.EqualFold(tmpl[:len(meAction)], meAction) {
		prefix, tmpl = tmpl[:len(meAction)], strings.TrimSpace(tmpl[len(meAction):])
	}
	switch {
	case tmpl == "":
		return prefix + "{user}"
	case strings.ContainsAny(tmpl[:1], afterName):
		return prefix + "{user}" + tmpl
	}
	return prefix + "{user}, " + tmpl
}

// take checks the cooldowns, the once-per-stream rule and the answer cap
// and, if the command may run, starts its cooldowns. The broadcaster skips
// the cooldowns and the once-per-stream rule.
func (b *Bot) take(cmd config.ChatCommand, m twitch.ChatMessage, answers bool) bool {
	now := b.d.Now()
	key := cmd.ID + "|" + m.ChatterID
	b.mu.Lock()
	defer b.mu.Unlock()
	if !m.IsBroadcaster {
		if cmd.OncePerStream && b.answered[key] {
			return false
		}
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
			b.d.Log.Warnf("Command %s skipped: %d answers in the last %s", Display(cmd), maxReplies, replyWindow)
			return false
		}
		b.replies = append(b.replies, now)
	}
	b.used[cmd.ID] = now
	b.usedBy[key] = now
	if cmd.OncePerStream && !m.IsBroadcaster {
		b.answered[key] = true
	}
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
	b.d.Send(ctx, b.Remember(text))
}

// Remember notes a message darkbot is about to post as the broadcaster
// (also song replies and event messages), so its echo in chat counts
// neither as a command nor as a chat line for the timers. It returns the
// text as it will be posted (cut to Twitch's limit).
func (b *Bot) Remember(text string) string {
	text = twitch.TruncateChat(text)
	now := b.d.Now()
	b.mu.Lock()
	defer b.mu.Unlock()
	b.sent = slices.DeleteFunc(b.sent, func(s sentMsg) bool { return now.Sub(s.at) >= echoWindow })
	b.sent = append(b.sent, sentMsg{text: text, at: now})
	return text
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
			if msg, ok := b.dueTimer(); ok {
				b.d.Log.Infof("Timer message posted")
				b.send(ctx, msg)
			}
		}
	}
}

// dueTimer picks the timer to post now, if any: timers run only while
// live and turned on; each waits its interval and its minimum of chat
// messages; of several due ones the longest waiting goes first, and
// never two within timerGap. It returns the message to post.
func (b *Bot) dueTimer() (string, bool) {
	s := b.d.Settings()
	active := s.TimersEnabled && b.d.Live()
	now := b.d.Now()
	b.mu.Lock()
	defer b.mu.Unlock()
	if !active {
		b.running = false
		return "", false
	}
	if !b.running {
		// Just went live (or timers were turned on): every timer waits a
		// full interval first instead of all posting at once.
		b.running = true
		clear(b.timers)
	}
	var pick *config.ChatTimer
	var pickState timerState
	seen := map[string]bool{}
	for i := range s.Timers {
		t := &s.Timers[i]
		seen[t.ID] = true
		st, ok := b.timers[t.ID]
		if !ok || !t.Enabled || len(t.Lines()) == 0 {
			// New or off: (re)starts waiting from now.
			b.timers[t.ID] = timerState{last: now, lines: b.seen}
			continue
		}
		if now.Sub(st.last) < time.Duration(t.IntervalMinutes)*time.Minute || b.seen-st.lines < t.MinLines {
			continue
		}
		if pick == nil || st.last.Before(pickState.last) {
			pick, pickState = t, st
		}
	}
	for id := range b.timers {
		if !seen[id] {
			delete(b.timers, id) // deleted timer
		}
	}
	if pick == nil || (!b.lastPost.IsZero() && now.Sub(b.lastPost) < timerGap) {
		return "", false
	}
	pickState.last, pickState.lines = now, b.seen
	msg := pickState.next(*pick)
	b.timers[pick.ID] = pickState
	b.lastPost = now
	return msg, true
}

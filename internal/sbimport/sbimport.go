// Package sbimport reads a Streamer.bot import code and turns its simple
// chat commands and timers into darkbot ones.
//
// The format is not specified by Streamer.bot (its docs call the code
// "UUEncoded"); it was read from published exports: base64 of the four
// bytes "SBAE" followed by gzip-compressed JSON:
//
//	{"version": 23, "data": {"actions": [...], "commands": [...], "timers": [...]}}
//
// A Streamer.bot command or timer has no text of its own: it triggers an
// action (trigger type 401 "Command Triggered" with commandId, type 701
// "Timed Action" with timerId), and the action's sub-actions do the work.
// Only actions that just send chat messages (sub-action type 10, field
// "text") can be imported; everything else (C# code, variables, OBS, ...)
// is reported as skipped with the reason. Exports before version 23 keep
// the sub-actions under "actions" instead of "subActions".
//
// Docs: https://docs.streamer.bot/guide/core/import-export,
// https://docs.streamer.bot/guide/commands, https://docs.streamer.bot/guide/core/timers
package sbimport

import (
	"bytes"
	"compress/gzip"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"regexp"
	"slices"
	"strings"

	"github.com/miwidot/darkbot/internal/chatbot"
	"github.com/miwidot/darkbot/internal/config"
	"github.com/miwidot/darkbot/internal/i18n"
)

// header starts every decoded import code.
const header = "SBAE"

// Size limits: an export of a whole Streamer.bot setup is a few hundred
// kilobytes; the JSON limit also stops a gzip bomb.
const (
	MaxCodeBytes = 20 << 20
	maxJSONBytes = 64 << 20
)

// Streamer.bot type numbers (from published exports).
const (
	triggerCommand   = 401
	triggerTimer     = 701
	subPlaySound     = 1    // "Play Sound": soundFile, volume 0..1
	subSendMessage   = 10   // Twitch "Send Message to Channel"
	subDelay         = 1002 // a pause; darkbot answers right away
	subComment       = 1009 // a note in the action, does nothing
	modeRegex        = 1
	locationAnywhere = 2
	grantDeny        = 1
	// firstDataVersion: exports from before it have another layout.
	firstDataVersion = 3
)

const secondsPerMinute = 60

// Export is the part of a Streamer.bot export darkbot reads.
type Export struct {
	Version int `json:"version"`
	Data    *struct {
		Actions  []action  `json:"actions"`
		Commands []command `json:"commands"`
		Timers   []timer   `json:"timers"`
	} `json:"data"`
}

type action struct {
	ID           string      `json:"id"`
	Name         string      `json:"name"`
	RandomAction bool        `json:"randomAction"`
	Triggers     []trigger   `json:"triggers"`
	SubActions   []subAction `json:"subActions"`
	// Old holds the sub-actions in exports before version 23.
	Old []subAction `json:"actions"`
}

type trigger struct {
	Type      int    `json:"type"`
	Enabled   *bool  `json:"enabled"`
	CommandID string `json:"commandId"`
	TimerID   string `json:"timerId"`
}

type subAction struct {
	Type      int     `json:"type"`
	Enabled   *bool   `json:"enabled"`
	Text      string  `json:"text"`
	SoundFile string  `json:"soundFile"`
	Volume    float64 `json:"volume"`
}

type command struct {
	ID              string            `json:"id"`
	Name            string            `json:"name"`
	Command         string            `json:"command"`
	Mode            int               `json:"mode"`
	Location        int               `json:"location"`
	GlobalCooldown  int               `json:"globalCooldown"`
	UserCooldown    int               `json:"userCooldown"`
	GrantType       int               `json:"grantType"`
	PermittedGroups []string          `json:"permittedGroups"`
	PermittedUsers  []json.RawMessage `json:"permittedUsers"`
}

type timer struct {
	ID             string `json:"id"`
	Name           string `json:"name"`
	Repeat         bool   `json:"repeat"`
	Interval       int    `json:"interval"`
	RandomInterval bool   `json:"randomInterval"`
	Lines          int    `json:"lines"`
}

// on: a missing "enabled" counts as on.
func on(b *bool) bool { return b == nil || *b }

// Decode reads an import code. Whitespace and line breaks (codes copied
// from a text file or chat) are ignored.
func Decode(code string) (Export, error) {
	var exp Export
	code = strings.Join(strings.Fields(code), "")
	if code == "" {
		return exp, i18n.E("err.sbEmpty")
	}
	if len(code) > MaxCodeBytes {
		return exp, i18n.E("err.sbTooBig")
	}
	raw, err := base64.StdEncoding.DecodeString(code)
	if err != nil {
		// Some sites drop the trailing "=".
		if raw, err = base64.RawStdEncoding.DecodeString(strings.TrimRight(code, "=")); err != nil {
			return exp, i18n.Wrap(err, "err.sbNotACode")
		}
	}
	body, ok := bytes.CutPrefix(raw, []byte(header))
	if !ok {
		return exp, i18n.E("err.sbNotACode")
	}
	zr, err := gzip.NewReader(bytes.NewReader(body))
	if err != nil {
		return exp, i18n.Wrap(err, "err.sbDamaged")
	}
	js, err := io.ReadAll(io.LimitReader(zr, maxJSONBytes+1))
	if err != nil {
		// Also a cut-off code: gzip checks the length and checksum at the end.
		return exp, i18n.Wrap(err, "err.sbDamaged")
	}
	if len(js) > maxJSONBytes {
		return exp, i18n.E("err.sbTooBig")
	}
	if err := json.Unmarshal(js, &exp); err != nil {
		return exp, i18n.Wrap(err, "err.sbDamaged")
	}
	if exp.Data == nil || exp.Version < firstDataVersion {
		return exp, i18n.E("err.sbOldVersion")
	}
	return exp, nil
}

// Item is one Streamer.bot command or timer: either the darkbot command
// or timer it becomes (turned off), or why it can't be imported.
type Item struct {
	// Name is the command or timer's name in Streamer.bot.
	Name    string
	Command *config.ChatCommand
	Timer   *config.ChatTimer
	// Skip is why it can't be imported (nil = it can).
	Skip error
	// Notes: what changed on the way (e.g. a cooldown cut to the maximum).
	Notes []error
}

// Convert turns the export's commands and timers into darkbot ones, in
// export order. They still have to pass darkbot's checks against the
// commands already there (chatbot.NormalizeCommand / NormalizeTimer).
func Convert(exp Export) (cmds, timers []Item) {
	if exp.Data == nil {
		return nil, nil
	}
	for _, c := range exp.Data.Commands {
		cmds = append(cmds, convertCommand(c, actionsFor(exp, triggerCommand, c.ID)))
	}
	for _, t := range exp.Data.Timers {
		timers = append(timers, convertTimer(t, actionsFor(exp, triggerTimer, t.ID)))
	}
	return cmds, timers
}

// actionsFor returns the actions an enabled trigger of type kind links to
// the command or timer id.
func actionsFor(exp Export, kind int, id string) []action {
	var out []action
	for _, a := range exp.Data.Actions {
		if slices.ContainsFunc(a.Triggers, func(t trigger) bool {
			linked := t.CommandID
			if kind == triggerTimer {
				linked = t.TimerID
			}
			return t.Type == kind && on(t.Enabled) && linked == id
		}) {
			out = append(out, a)
		}
	}
	return out
}

// content is what an action does that darkbot can do too.
type content struct {
	texts  []string
	random bool // Streamer.bot picks one sub-action at random
	sound  string
	volume float64
}

// contentOf returns what the only action does, or why darkbot can't do
// it: only chat messages, at most one sound, pauses and notes.
func contentOf(acts []action) (content, error) {
	var c content
	switch len(acts) {
	case 0:
		return c, i18n.E("err.sbNoAction")
	case 1:
	default:
		return c, i18n.E("err.sbManyActions", "n", len(acts))
	}
	a := acts[0]
	subs := a.SubActions
	if subs == nil {
		subs = a.Old
	}
	for _, s := range subs {
		switch {
		case !on(s.Enabled), s.Type == subComment, s.Type == subDelay:
		case s.Type == subSendMessage:
			if t := strings.TrimSpace(s.Text); t != "" {
				c.texts = append(c.texts, t)
			}
		case s.Type == subPlaySound && c.sound == "":
			c.sound, c.volume = strings.TrimSpace(s.SoundFile), s.Volume
		case s.Type == subPlaySound:
			return c, i18n.E("err.sbManySounds")
		default:
			return c, i18n.E("err.sbNotOnlyChat")
		}
	}
	if len(c.texts) == 0 && c.sound == "" {
		return c, i18n.E("err.sbNoMessage")
	}
	c.random = a.RandomAction
	return c, nil
}

// variable is a Streamer.bot variable in a message, e.g. %user%.
var variable = regexp.MustCompile(`%([A-Za-z0-9_.]+)%`)

// placeholders maps Streamer.bot variables (lowercase) to darkbot's: the
// chatter and, for commands, the viewer named after the command.
var placeholders = map[string]string{
	"user":           "{user}",
	"username":       "{user}",
	"targetuser":     "{target}",
	"targetusername": "{target}",
}

// slashCommand is a Twitch chat command at the start of a message
// ("/me waves"); darkbot posts through the Twitch API, which sends it as
// plain text.
const slashCommand = "/"

// translate swaps the Streamer.bot variables of text for darkbot
// placeholders; viewer names only exist for commands.
func translate(text string, forCommand bool) (string, error) {
	if strings.HasPrefix(text, slashCommand) {
		first, _, _ := strings.Cut(text, " ")
		return "", i18n.E("err.sbSlash", "cmd", first)
	}
	var unknown string
	out := variable.ReplaceAllStringFunc(text, func(v string) string {
		name := strings.ToLower(strings.Trim(v, "%"))
		if p, ok := placeholders[name]; ok && forCommand {
			return p
		}
		if unknown == "" {
			unknown = v
		}
		return v
	})
	if unknown != "" {
		return "", i18n.E("err.sbVariable", "name", unknown)
	}
	return out, nil
}

// Role groups as Streamer.bot users name them, lowest first: an "Allow"
// list with only these becomes the lowest one's darkbot permission.
type roleGroup struct {
	names []string
	perm  config.Permission
}

var roleGroups = []roleGroup{
	{[]string{"subscribers", "subscriber", "subs"}, config.PermSubscriber},
	{[]string{"vips", "vip"}, config.PermVIP},
	{[]string{"moderators", "moderator", "mods"}, config.PermModerator},
}

// permission maps Streamer.bot's allow/deny lists. Empty lists apply to
// everyone (Allow = everyone may, Deny = only the broadcaster, who is
// always exempt). Lists of single users or own groups have no darkbot
// counterpart.
func permission(c command) (config.Permission, error) {
	if len(c.PermittedUsers) > 0 {
		return "", i18n.E("err.sbPermUsers")
	}
	if len(c.PermittedGroups) == 0 {
		if c.GrantType == grantDeny {
			return config.PermBroadcaster, nil
		}
		return config.PermEveryone, nil
	}
	if c.GrantType == grantDeny {
		return "", i18n.E("err.sbPermDeny")
	}
	lowest := len(roleGroups)
	for _, g := range c.PermittedGroups {
		i := slices.IndexFunc(roleGroups, func(r roleGroup) bool {
			return slices.Contains(r.names, strings.ToLower(strings.TrimSpace(g)))
		})
		if i < 0 {
			return "", i18n.E("err.sbPermGroup", "name", g)
		}
		lowest = min(lowest, i)
	}
	return roleGroups[lowest].perm, nil
}

// cooldown clamps a Streamer.bot cooldown to darkbot's range.
func cooldown(sec int, notes *[]error) int {
	if sec > config.MaxCommandCooldown {
		*notes = append(*notes, i18n.E("note.sbCooldownCut", "max", config.MaxCommandCooldown))
		return config.MaxCommandCooldown
	}
	return max(sec, 0)
}

func convertCommand(c command, acts []action) Item {
	it := Item{Name: c.Name}
	if it.Name == "" {
		it.Name = strings.TrimSpace(c.Command)
	}
	fail := func(err error) Item { it.Skip = err; return it }
	switch {
	case c.Mode == modeRegex:
		return fail(i18n.E("err.sbRegex"))
	case c.Location == locationAnywhere:
		return fail(i18n.E("err.sbAnywhere"))
	}
	// One name per line; "!" names become a "!" command, the others a
	// word command. darkbot has one kind per command, so a mix keeps the
	// "!" names.
	var bang, words []string
	for _, line := range strings.Split(c.Command, "\n") {
		n := strings.ToLower(strings.TrimSpace(line))
		switch {
		case n == "":
		case strings.HasPrefix(n, chatbot.Prefix):
			if n = strings.TrimPrefix(n, chatbot.Prefix); !slices.Contains(bang, n) {
				bang = append(bang, n)
			}
		case !slices.Contains(words, n):
			words = append(words, n)
		}
	}
	names, trigger := bang, config.TriggerCommand
	if len(bang) == 0 {
		names, trigger = words, config.TriggerWord
	} else if len(words) > 0 {
		it.Notes = append(it.Notes, i18n.E("note.sbWordsDropped", "names", strings.Join(words, ", ")))
	}
	if len(names) == 0 {
		return fail(i18n.E("err.sbNoName"))
	}
	ct, err := contentOf(acts)
	if err != nil {
		return fail(err)
	}
	if len(ct.texts) > 1 {
		if ct.random {
			return fail(i18n.E("err.sbRandomAnswers"))
		}
		return fail(i18n.E("err.sbManyMessages"))
	}
	if ct.random && ct.sound != "" && len(ct.texts) > 0 {
		// One of the two at random, not both.
		return fail(i18n.E("err.sbRandomAnswers"))
	}
	var text string
	if len(ct.texts) == 1 {
		if text, err = translate(ct.texts[0], true); err != nil {
			return fail(err)
		}
	}
	perm, err := permission(c)
	if err != nil {
		return fail(err)
	}
	it.Command = &config.ChatCommand{
		Name:                names[0],
		Aliases:             names[1:],
		Trigger:             trigger,
		Response:            text,
		Sound:               ct.sound,
		Volume:              ct.volume,
		Permission:          perm,
		CooldownSeconds:     cooldown(c.GlobalCooldown, &it.Notes),
		UserCooldownSeconds: cooldown(c.UserCooldown, &it.Notes),
	}
	return it
}

func convertTimer(t timer, acts []action) Item {
	it := Item{Name: t.Name}
	fail := func(err error) Item { it.Skip = err; return it }
	if !t.Repeat {
		return fail(i18n.E("err.sbTimerOnce"))
	}
	ct, err := contentOf(acts)
	switch {
	case err != nil:
		return fail(err)
	case ct.sound != "":
		return fail(i18n.E("err.sbTimerSound"))
	}
	texts := ct.texts
	if len(texts) > 1 && !ct.random {
		return fail(i18n.E("err.sbManyMessages"))
	}
	if len(texts) > config.MaxTimerMessages {
		return fail(i18n.E("err.timerTooMany", "max", config.MaxTimerMessages))
	}
	for i, text := range texts {
		if texts[i], err = translate(text, false); err != nil {
			return fail(err)
		}
	}
	// Streamer.bot counts seconds, darkbot whole minutes: round up, so the
	// timer never posts more often than before.
	minutes := (max(t.Interval, 0) + secondsPerMinute - 1) / secondsPerMinute
	if t.RandomInterval {
		it.Notes = append(it.Notes, i18n.E("note.sbRandomInterval"))
	}
	switch {
	case minutes < config.MinTimerMinutes:
		it.Notes = append(it.Notes, i18n.E("note.sbIntervalRaised", "min", config.MinTimerMinutes))
		minutes = config.MinTimerMinutes
	case minutes > config.MaxTimerMinutes:
		it.Notes = append(it.Notes, i18n.E("note.sbIntervalCut", "max", config.MaxTimerMinutes))
		minutes = config.MaxTimerMinutes
	}
	lines := min(max(t.Lines, 0), config.MaxTimerLines)
	it.Timer = &config.ChatTimer{
		Messages:        texts,
		Order:           config.TimerRandom,
		IntervalMinutes: minutes,
		MinLines:        lines,
	}
	return it
}

// String is for logs and tests.
func (it Item) String() string {
	if it.Skip != nil {
		return fmt.Sprintf("%s: skipped (%v)", it.Name, it.Skip)
	}
	return it.Name + ": ok"
}

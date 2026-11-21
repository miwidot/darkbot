package chatbot

import (
	"slices"
	"strings"
	"unicode/utf8"

	"github.com/miwidot/darkbot/internal/config"
	"github.com/miwidot/darkbot/internal/i18n"
)

// oneLine joins text into one line: a chat message has no line breaks.
func oneLine(s string) string { return strings.Join(strings.Fields(s), " ") }

// cleanName lowercases a name and drops a leading "!".
func cleanName(s string) string {
	return strings.ToLower(strings.TrimPrefix(strings.TrimSpace(s), Prefix))
}

// NormalizeCommand cleans up c and checks it against the other commands
// with the same trigger (same ID = the one being edited) and the reserved
// names (song commands, only for "!" commands). The sound and answer
// files are checked by the caller.
func NormalizeCommand(c config.ChatCommand, others []config.ChatCommand, reserved []string) (config.ChatCommand, error) {
	c.Name = cleanName(c.Name)
	var aliases []string
	for _, a := range c.Aliases {
		if a = cleanName(a); a != "" && a != c.Name && !slices.Contains(aliases, a) {
			aliases = append(aliases, a)
		}
	}
	c.Aliases = aliases
	c.Response = oneLine(c.Response)
	c.ResponseFile = strings.TrimSpace(c.ResponseFile)
	c.Sound = strings.TrimSpace(c.Sound)
	c.Trigger = Trigger(c)
	switch {
	case !namePattern.MatchString(c.Name):
		return c, i18n.E("err.cmdName", "name", c.Name)
	case len(c.Aliases) > config.MaxAliases:
		return c, i18n.E("err.cmdAliases", "max", config.MaxAliases)
	case c.Response == "" && c.ResponseFile == "" && c.Sound == "":
		return c, i18n.E("err.cmdEmpty")
	case utf8.RuneCountInString(c.Response) > config.MaxChatTemplate:
		return c, i18n.E("err.chatTooLong", "max", config.MaxChatTemplate)
	case !slices.Contains(config.Permissions, c.Permission):
		return c, i18n.E("err.cmdPermission")
	case c.CooldownSeconds < 0 || c.CooldownSeconds > config.MaxCommandCooldown,
		c.UserCooldownSeconds < 0 || c.UserCooldownSeconds > config.MaxCommandCooldown:
		return c, i18n.E("err.cmdCooldown", "max", config.MaxCommandCooldown)
	}
	for _, a := range c.Aliases {
		if !namePattern.MatchString(a) {
			return c, i18n.E("err.cmdName", "name", a)
		}
	}
	names := append([]string{c.Name}, c.Aliases...)
	for _, n := range names {
		shown := Display(config.ChatCommand{Name: n, Trigger: c.Trigger})
		if c.Trigger == config.TriggerCommand && slices.Contains(reserved, n) {
			return c, i18n.E("err.cmdReserved", "name", shown)
		}
		for _, o := range others {
			if o.ID != c.ID && Trigger(o) == c.Trigger && (o.Name == n || slices.Contains(o.Aliases, n)) {
				return c, i18n.E("err.cmdTaken", "name", shown)
			}
		}
	}
	if c.Volume <= 0 || c.Volume > config.MaxVolume {
		c.Volume = config.DefaultSoundVolume
	}
	return c, nil
}

// NormalizeTimer cleans up and checks a timer.
// Each message is one chat line; empty ones are dropped.
func NormalizeTimer(t config.ChatTimer) (config.ChatTimer, error) {
	var msgs []string
	for _, m := range t.Lines() {
		if m = oneLine(m); m != "" {
			msgs = append(msgs, m)
		}
	}
	t.Messages, t.Message = msgs, ""
	if t.Order != config.TimerInOrder {
		t.Order = config.TimerRandom
	}
	for i, m := range msgs {
		if utf8.RuneCountInString(m) > config.MaxChatTemplate {
			return t, i18n.E("err.timerLineTooLong", "n", i+1, "max", config.MaxChatTemplate)
		}
	}
	switch {
	case len(msgs) == 0:
		return t, i18n.E("err.timerEmpty")
	case len(msgs) > config.MaxTimerMessages:
		return t, i18n.E("err.timerTooMany", "max", config.MaxTimerMessages)
	case t.IntervalMinutes < config.MinTimerMinutes || t.IntervalMinutes > config.MaxTimerMinutes:
		return t, i18n.E("err.timerInterval", "min", config.MinTimerMinutes, "max", config.MaxTimerMinutes)
	case t.MinLines < 0 || t.MinLines > config.MaxTimerLines:
		return t, i18n.E("err.timerLines", "max", config.MaxTimerLines)
	}
	return t, nil
}

// CleanName turns what the streamer typed ("!Clips ") into a command name
// ("clips"); ok reports whether it is a valid one.
func CleanName(s string) (name string, ok bool) {
	name = cleanName(s)
	return name, namePattern.MatchString(name)
}

// NormalizeBuiltin cleans up a built-in command's name, who may use it and
// its cooldown. The name may not be reserved (song commands, the other
// built-in commands) or one of the streamer's own "!" commands.
func NormalizeBuiltin(c config.BuiltinCommand, cmds []config.ChatCommand, reserved []string) (config.BuiltinCommand, error) {
	name, ok := CleanName(c.Name)
	c.Name = name
	shown := Prefix + name
	switch {
	case !ok:
		return c, i18n.E("err.cmdName", "name", name)
	case slices.Contains(reserved, name):
		return c, i18n.E("err.cmdReserved", "name", shown)
	case !slices.Contains(config.Permissions, c.Permission):
		return c, i18n.E("err.cmdPermission")
	case c.CooldownSeconds < 0 || c.CooldownSeconds > config.MaxCommandCooldown:
		return c, i18n.E("err.cmdCooldown", "max", config.MaxCommandCooldown)
	}
	if _, taken := Find(cmds, config.TriggerCommand, name); taken {
		return c, i18n.E("err.cmdTaken", "name", shown)
	}
	return c, nil
}

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
// (same ID = the one being edited) and the reserved names (song
// commands). The sound file is checked by the caller.
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
	c.Sound = strings.TrimSpace(c.Sound)
	switch {
	case !namePattern.MatchString(c.Name):
		return c, i18n.E("err.cmdName", "name", c.Name)
	case len(c.Aliases) > config.MaxAliases:
		return c, i18n.E("err.cmdAliases", "max", config.MaxAliases)
	case c.Response == "" && c.Sound == "":
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
		if slices.Contains(reserved, n) {
			return c, i18n.E("err.cmdReserved", "name", Prefix+n)
		}
		for _, o := range others {
			if o.ID != c.ID && (o.Name == n || slices.Contains(o.Aliases, n)) {
				return c, i18n.E("err.cmdTaken", "name", Prefix+n)
			}
		}
	}
	if c.Volume <= 0 || c.Volume > config.MaxVolume {
		c.Volume = config.DefaultSoundVolume
	}
	return c, nil
}

// NormalizeTimer cleans up and checks a timer.
func NormalizeTimer(t config.ChatTimer) (config.ChatTimer, error) {
	t.Message = oneLine(t.Message)
	switch {
	case t.Message == "":
		return t, i18n.E("err.timerEmpty")
	case utf8.RuneCountInString(t.Message) > config.MaxChatTemplate:
		return t, i18n.E("err.chatTooLong", "max", config.MaxChatTemplate)
	case t.IntervalMinutes < config.MinTimerMinutes || t.IntervalMinutes > config.MaxTimerMinutes:
		return t, i18n.E("err.timerInterval", "min", config.MinTimerMinutes, "max", config.MaxTimerMinutes)
	case t.MinLines < 0 || t.MinLines > config.MaxTimerLines:
		return t, i18n.E("err.timerLines", "max", config.MaxTimerLines)
	}
	return t, nil
}

package main

import (
	"path/filepath"
	"slices"

	"github.com/miwidot/darkbot/internal/audio"
	"github.com/miwidot/darkbot/internal/chatbot"
	"github.com/miwidot/darkbot/internal/config"
	"github.com/miwidot/darkbot/internal/i18n"
	"github.com/miwidot/darkbot/internal/sbimport"
)

// Import of chat commands and timers from a Streamer.bot import code
// (internal/sbimport). The streamer pastes the code, sees what darkbot
// takes over and what not (with the reason), then imports. Everything
// imported starts turned off, so nothing answers twice while Streamer.bot
// still runs.

// Import row kinds.
const (
	importCommand = "command"
	importTimer   = "timer"
)

// ImportRow is one Streamer.bot command or timer in the preview.
type ImportRow struct {
	Kind string `json:"kind"`
	// Name in Streamer.bot.
	Name    string              `json:"name"`
	Command *config.ChatCommand `json:"command,omitempty"`
	Timer   *config.ChatTimer   `json:"timer,omitempty"`
	// Skip: why it is not imported ("" = it is).
	Skip  string   `json:"skip,omitempty"`
	Notes []string `json:"notes,omitempty"`
}

// ImportResult lists every row and how many commands and timers are
// (or would be) imported.
type ImportResult struct {
	Rows     []ImportRow `json:"rows"`
	Commands int         `json:"commands"`
	Timers   int         `json:"timers"`
}

// PreviewStreamerbot shows what an import code would add; nothing is saved.
func (a *App) PreviewStreamerbot(code string) (ImportResult, error) {
	exp, err := sbimport.Decode(code)
	if err != nil {
		return ImportResult{}, a.uiErr(err)
	}
	chat := a.cfg.Get().Chat
	return planImport(&chat, exp, a.reservedCommands(), a.lang()), nil
}

// ImportStreamerbot adds the importable commands and timers of a code,
// turned off. The code is read again, so the result matches the settings
// at this moment even if they changed since the preview.
func (a *App) ImportStreamerbot(code string) (ImportResult, error) {
	exp, err := sbimport.Decode(code)
	if err != nil {
		return ImportResult{}, a.uiErr(err)
	}
	// Read before updateChat: both read the settings, which are locked
	// while it runs.
	reserved, lang := a.reservedCommands(), a.lang()
	var res ImportResult
	err = a.updateChat(func(c *config.Chat) error {
		res = planImport(c, exp, reserved, lang)
		if res.Commands+res.Timers == 0 {
			return i18n.E("err.sbNothing")
		}
		return nil
	})
	if err != nil {
		return ImportResult{}, err
	}
	a.log.Infof("Imported %d commands and %d timers from Streamer.bot (turned off)", res.Commands, res.Timers)
	return res, nil
}

// planImport adds the importable items to c, each checked like a command
// or timer the streamer saves (names taken by earlier rows count too).
func planImport(c *config.Chat, exp sbimport.Export, reserved []string, lang string) ImportResult {
	var res ImportResult
	row := func(kind string, it sbimport.Item) ImportRow {
		r := ImportRow{Kind: kind, Name: it.Name}
		for _, n := range it.Notes {
			r.Notes = append(r.Notes, i18n.Localize(n, lang))
		}
		return r
	}
	cmds, timers := sbimport.Convert(exp)
	for _, it := range cmds {
		r := row(importCommand, it)
		err := it.Skip
		if err == nil {
			err = keepSound(it.Command, &r, lang)
		}
		if err == nil {
			var norm config.ChatCommand
			if norm, err = chatbot.NormalizeCommand(*it.Command, c.Commands, reserved); err == nil {
				if len(c.Commands) >= config.MaxCommands {
					err = i18n.E("err.tooManyCommands", "max", config.MaxCommands)
				} else {
					norm.ID, norm.Enabled = config.NewID(), false
					c.Commands = append(c.Commands, norm)
					r.Command = &norm
					res.Commands++
				}
			}
		}
		if err != nil {
			r.Skip = i18n.Localize(err, lang)
		}
		res.Rows = append(res.Rows, r)
	}
	for _, it := range timers {
		r := row(importTimer, it)
		err := it.Skip
		if err == nil {
			var norm config.ChatTimer
			if norm, err = chatbot.NormalizeTimer(*it.Timer); err == nil {
				if slices.ContainsFunc(c.Timers, func(o config.ChatTimer) bool { return slices.Equal(o.Lines(), norm.Messages) }) {
					err = i18n.E("err.sbTimerExists")
				} else if len(c.Timers) >= config.MaxTimers {
					err = i18n.E("err.tooManyTimers", "max", config.MaxTimers)
				} else {
					norm.ID, norm.Enabled = config.NewID(), false
					c.Timers = append(c.Timers, norm)
					r.Timer = &norm
					res.Timers++
				}
			}
		}
		if err != nil {
			r.Skip = i18n.Localize(err, lang)
		}
		res.Rows = append(res.Rows, r)
	}
	return res
}

// keepSound checks the sound file of an imported command like SaveCommand
// does. Streamer.bot often runs on another computer (or Windows), so a
// missing file is left out with a note; a command that only played it is
// skipped.
func keepSound(cmd *config.ChatCommand, r *ImportRow, lang string) error {
	if cmd.Sound == "" {
		return nil
	}
	if filepath.IsAbs(cmd.Sound) {
		if _, err := audio.Check(cmd.Sound); err == nil {
			return nil
		}
	}
	if cmd.Response == "" {
		return i18n.E("err.sbSoundMissing", "file", cmd.Sound)
	}
	r.Notes = append(r.Notes, i18n.T(lang, "note.sbSoundMissing", "file", cmd.Sound))
	cmd.Sound, cmd.Volume = "", 0
	return nil
}

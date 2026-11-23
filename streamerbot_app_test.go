package main

import (
	"bytes"
	"compress/gzip"
	"encoding/base64"
	"encoding/json"
	"testing"

	"github.com/miwidot/darkbot/internal/config"
)

// sbCode builds a Streamer.bot import code (see internal/sbimport).
func sbCode(t *testing.T, data map[string]any) string {
	t.Helper()
	js, err := json.Marshal(map[string]any{"version": 23, "data": data})
	if err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	buf.WriteString("SBAE")
	zw := gzip.NewWriter(&buf)
	zw.Write(js)
	zw.Close()
	return base64.StdEncoding.EncodeToString(buf.Bytes())
}

func sbCmd(id, name, text string, subs ...map[string]any) (cmd, act map[string]any) {
	if text != "" {
		subs = append(subs, map[string]any{"type": 10, "enabled": true, "text": text})
	}
	cmd = map[string]any{"id": id, "name": name, "command": name, "globalCooldown": 0, "userCooldown": 0, "permittedGroups": []string{}}
	act = map[string]any{"id": "a" + id, "name": name, "triggers": []map[string]any{{"type": 401, "enabled": true, "commandId": id}}, "subActions": subs}
	return cmd, act
}

func TestImportStreamerbot(t *testing.T) {
	a := newChatApp(t)
	if err := a.SaveCommand(config.ChatCommand{Name: "discord", Response: "mine", Permission: config.PermEveryone, Enabled: true}); err != nil {
		t.Fatal(err)
	}
	sound := func(f string) map[string]any {
		return map[string]any{"type": 1, "enabled": true, "soundFile": f, "volume": 0.5}
	}
	c1, a1 := sbCmd("1", "!discord", "discord.gg/theirs")
	c2, a2 := sbCmd("2", "!socials", "twitter.com/x")
	c3, a3 := sbCmd("3", "!horn", "", sound(`C:\sounds\horn.mp3`))
	c4, a4 := sbCmd("4", "!hype", "HYPE", sound(`C:\sounds\hype.mp3`))
	tm := map[string]any{"id": "t", "name": "Socials", "repeat": true, "interval": 1200, "lines": 0}
	ta := map[string]any{"id": "at", "name": "t", "randomAction": true, "triggers": []map[string]any{{"type": 701, "enabled": true, "timerId": "t"}},
		"subActions": []map[string]any{{"type": 10, "enabled": true, "text": "A"}, {"type": 10, "enabled": true, "text": "B"}}}
	code := sbCode(t, map[string]any{
		"actions":  []map[string]any{a1, a2, a3, a4, ta},
		"commands": []map[string]any{c1, c2, c3, c4},
		"timers":   []map[string]any{tm},
	})

	before := a.cfg.Get().Chat
	res, err := a.PreviewStreamerbot(code)
	if err != nil {
		t.Fatal(err)
	}
	if res.Commands != 2 || res.Timers != 1 || len(res.Rows) != 5 {
		t.Fatalf("preview: %+v", res)
	}
	// discord: name taken by an own command; horn: its only sound is not here;
	// hype: imported without the missing sound, with a note.
	if res.Rows[0].Skip == "" || res.Rows[1].Skip != "" || res.Rows[2].Skip == "" || res.Rows[3].Skip != "" || len(res.Rows[3].Notes) != 1 || res.Rows[3].Command.Sound != "" {
		t.Fatalf("rows: %+v", res.Rows)
	}
	if after := a.cfg.Get().Chat; len(after.Commands) != len(before.Commands) || len(after.Timers) != len(before.Timers) {
		t.Fatal("the preview changed the settings")
	}

	res, err = a.ImportStreamerbot(code)
	if err != nil || res.Commands != 2 || res.Timers != 1 {
		t.Fatalf("import: %v %+v", err, res)
	}
	chat := a.cfg.Get().Chat
	if len(chat.Commands) != 3 || len(chat.Timers) != 1 {
		t.Fatalf("after import: %+v", chat)
	}
	for _, c := range chat.Commands[1:] {
		if c.ID == "" || c.Enabled {
			t.Fatalf("an imported command must have an ID and be off: %+v", c)
		}
	}
	if tm := chat.Timers[0]; tm.ID == "" || tm.Enabled || tm.IntervalMinutes != 20 || len(tm.Messages) != 2 {
		t.Fatalf("timer: %+v", tm)
	}

	// The same code again: everything is there already, nothing changes.
	if _, err := a.ImportStreamerbot(code); err == nil {
		t.Fatal("a second import must find nothing new")
	}
	if again := a.cfg.Get().Chat; len(again.Commands) != 3 || len(again.Timers) != 1 {
		t.Fatalf("second import changed the settings: %+v", again)
	}
	if _, err := a.PreviewStreamerbot("hello"); err == nil {
		t.Fatal("not a code")
	}
}

package sbimport

import (
	"bytes"
	"compress/gzip"
	"encoding/base64"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/miwidot/darkbot/internal/config"
	"github.com/miwidot/darkbot/internal/i18n"
)

// encode builds an import code the way Streamer.bot does.
func encode(t *testing.T, v any) string {
	t.Helper()
	js, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	buf.WriteString(header)
	zw := gzip.NewWriter(&buf)
	zw.Write(js)
	zw.Close()
	return base64.StdEncoding.EncodeToString(buf.Bytes())
}

type obj = map[string]any

func send(text string) obj { return obj{"type": subSendMessage, "enabled": true, "text": text} }

func cmdAction(cmdID string, random bool, subs ...obj) obj {
	return obj{"id": "a-" + cmdID, "name": "act " + cmdID, "randomAction": random,
		"triggers":   []obj{{"type": triggerCommand, "enabled": true, "commandId": cmdID}},
		"subActions": subs}
}

func timerAction(timerID string, random bool, subs ...obj) obj {
	return obj{"id": "a-" + timerID, "name": "act " + timerID, "randomAction": random,
		"triggers":   []obj{{"type": triggerTimer, "enabled": true, "timerId": timerID}},
		"subActions": subs}
}

func cmd(id, names string, extra obj) obj {
	c := obj{"id": id, "name": names, "command": names, "mode": 0, "location": 0,
		"globalCooldown": 10, "userCooldown": 0, "grantType": 0,
		"permittedGroups": []string{}, "permittedUsers": []any{}}
	for k, v := range extra {
		c[k] = v
	}
	return c
}

func export(actions, commands, timers []obj) obj {
	return obj{"version": 23, "data": obj{"actions": actions, "commands": commands, "timers": timers}}
}

func errKey(err error) string {
	if e, ok := errors.AsType[*i18n.Error](err); ok {
		return e.Key
	}
	return ""
}

func TestDecode(t *testing.T) {
	code := encode(t, export(nil, nil, nil))
	if !strings.HasPrefix(code, "U0JBRR+LCA") {
		t.Fatalf("not the Streamer.bot shape: %.20s", code)
	}
	// Wrapped over lines and without the base64 padding still reads.
	wrapped := code[:10] + "\r\n  " + code[10:]
	for _, c := range []string{code, wrapped, strings.TrimRight(code, "=")} {
		if _, err := Decode(c); err != nil {
			t.Errorf("valid code refused: %v", err)
		}
	}
	plain := base64.StdEncoding.EncodeToString([]byte("hello world"))
	cut := code[:len(code)/2]
	cut = cut[:len(cut)/4*4]
	old := encode(t, obj{"version": 2, "name": "x", "actions": []any{}})
	for _, c := range []struct{ code, key string }{
		{"  ", "err.sbEmpty"},
		{"not base64 at all!", "err.sbNotACode"},
		{plain, "err.sbNotACode"},
		{cut, "err.sbDamaged"},
		{old, "err.sbOldVersion"},
		{strings.Repeat("A", MaxCodeBytes+4), "err.sbTooBig"},
	} {
		if _, err := Decode(c.code); errKey(err) != c.key {
			t.Errorf("%.20q: got %v, want %s", c.code, err, c.key)
		}
	}
}

func convert(t *testing.T, e obj) (cmds, timers []Item) {
	t.Helper()
	exp, err := Decode(encode(t, e))
	if err != nil {
		t.Fatal(err)
	}
	return Convert(exp)
}

func TestSimpleCommand(t *testing.T) {
	cmds, _ := convert(t, export(
		[]obj{cmdAction("c1", false,
			obj{"type": subComment, "enabled": true, "value": "a note"},
			obj{"type": 99999, "enabled": false},
			send("Hi %user%, join %targetUser%'s Discord: discord.gg/x"))},
		[]obj{cmd("c1", "!Discord\r\n!dc\r\ndc\r\nDC\r\n", obj{"userCooldown": 5000})}, nil))
	if len(cmds) != 1 || cmds[0].Skip != nil {
		t.Fatalf("got %v", cmds)
	}
	c := cmds[0].Command
	want := config.ChatCommand{Name: "discord", Aliases: []string{"dc"}, Trigger: config.TriggerCommand,
		Response: "Hi {user}, join {target}'s Discord: discord.gg/x", Permission: config.PermEveryone,
		CooldownSeconds: 10, UserCooldownSeconds: config.MaxCommandCooldown}
	if c.Name != want.Name || !slices.Equal(c.Aliases, want.Aliases) || c.Trigger != want.Trigger || c.Response != want.Response ||
		c.Permission != want.Permission || c.CooldownSeconds != want.CooldownSeconds || c.UserCooldownSeconds != want.UserCooldownSeconds || c.Enabled {
		t.Fatalf("got %+v", c)
	}
	var notes []string
	for _, n := range cmds[0].Notes {
		notes = append(notes, errKey(n))
	}
	if !slices.Equal(notes, []string{"note.sbWordsDropped", "note.sbCooldownCut"}) {
		t.Fatalf("notes %v", notes)
	}
}

// "/me" with text works with Send Chat Message (an action message).
func TestMeMessage(t *testing.T) {
	cmds, _ := convert(t, export([]obj{cmdAction("c", false, send("/me hugs %targetUser%"))}, []obj{cmd("c", "!hug", nil)}, nil))
	if cmds[0].Skip != nil || cmds[0].Command.Response != "/me hugs {target}" {
		t.Fatalf("got %v", cmds[0])
	}
}

func TestWordCommandAndOldLayout(t *testing.T) {
	// Exports before version 23 keep the sub-actions under "actions".
	act := obj{"id": "a", "name": "a", "triggers": []obj{{"type": triggerCommand, "enabled": true, "commandId": "w"}},
		"actions": []obj{{"type": subSendMessage, "enabled": true, "text": "o/"}}}
	e := export([]obj{act}, []obj{cmd("w", "hi\nhallo", nil)}, nil)
	e["version"] = 10
	cmds, _ := convert(t, e)
	if c := cmds[0].Command; cmds[0].Skip != nil || c.Trigger != config.TriggerWord || c.Name != "hi" || !slices.Equal(c.Aliases, []string{"hallo"}) || c.Response != "o/" {
		t.Fatalf("got %v %+v", cmds[0], c)
	}
}

func TestPermissions(t *testing.T) {
	cases := []struct {
		extra obj
		perm  config.Permission
		skip  string
	}{
		{obj{"permittedGroups": []string{"Moderators"}}, config.PermModerator, ""},
		{obj{"permittedGroups": []string{"Moderators", "VIPs"}}, config.PermVIP, ""},
		{obj{"permittedGroups": []string{"Moderators", "Subscribers", "VIPs"}}, config.PermSubscriber, ""},
		{obj{"grantType": grantDeny}, config.PermBroadcaster, ""},
		{obj{"permittedGroups": []string{"Moderators", "Mods2024"}}, "", "err.sbPermGroup"},
		{obj{"permittedGroups": []string{"VIPs"}, "grantType": grantDeny}, "", "err.sbPermDeny"},
		{obj{"permittedUsers": []obj{{"id": "1", "login": "anna"}}}, "", "err.sbPermUsers"},
	}
	for _, c := range cases {
		cmds, _ := convert(t, export([]obj{cmdAction("c", false, send("x"))}, []obj{cmd("c", "!x", c.extra)}, nil))
		it := cmds[0]
		if errKey(it.Skip) != c.skip || (c.skip == "" && it.Command.Permission != c.perm) {
			t.Errorf("%v: got %v %+v", c.extra, it.Skip, it.Command)
		}
	}
}

func TestCommandSkips(t *testing.T) {
	off := false
	disabledTrigger := cmdAction("c", false, send("x"))
	disabledTrigger["triggers"] = []obj{{"type": triggerCommand, "enabled": off, "commandId": "c"}}
	cases := []struct {
		name    string
		actions []obj
		extra   obj
		key     string
	}{
		{"regex", []obj{cmdAction("c", false, send("x"))}, obj{"mode": modeRegex}, "err.sbRegex"},
		{"anywhere", []obj{cmdAction("c", false, send("x"))}, obj{"location": locationAnywhere}, "err.sbAnywhere"},
		{"no action", nil, nil, "err.sbNoAction"},
		{"disabled trigger", []obj{disabledTrigger}, nil, "err.sbNoAction"},
		{"two actions", []obj{cmdAction("c", false, send("x")), cmdAction("c", false, send("y"))}, nil, "err.sbManyActions"},
		{"code", []obj{cmdAction("c", false, send("x"), obj{"type": 99999, "enabled": true})}, nil, "err.sbNotOnlyChat"},
		{"only a comment", []obj{cmdAction("c", false, obj{"type": subComment, "enabled": true})}, nil, "err.sbNoMessage"},
		{"random answers", []obj{cmdAction("c", true, send("a"), send("b"))}, nil, "err.sbRandomAnswers"},
		{"two messages", []obj{cmdAction("c", false, send("a"), send("b"))}, nil, "err.sbManyMessages"},
		{"variable", []obj{cmdAction("c", false, send("Quote: %quote%"))}, nil, "err.sbVariable"},
		{"slash command", []obj{cmdAction("c", false, send("/announce Hype!"))}, nil, "err.sbSlash"},
		{"bare /me", []obj{cmdAction("c", false, send("/me"))}, nil, "err.sbSlash"},
		{"dot command", []obj{cmdAction("c", false, send(".foo bar"))}, nil, "err.sbSlash"},
		{"no name", []obj{cmdAction("c", false, send("x"))}, obj{"command": " \r\n "}, "err.sbNoName"},
	}
	for _, c := range cases {
		cmds, _ := convert(t, export(c.actions, []obj{cmd("c", "!x", c.extra)}, nil))
		if errKey(cmds[0].Skip) != c.key {
			t.Errorf("%s: got %v, want %s", c.name, cmds[0].Skip, c.key)
		}
	}
}

func sbTimer(id string, extra obj) obj {
	tm := obj{"id": id, "name": "Socials", "enabled": true, "repeat": true, "interval": 900,
		"randomInterval": false, "upperInterval": 0, "lines": 5}
	for k, v := range extra {
		tm[k] = v
	}
	return tm
}

func TestTimers(t *testing.T) {
	_, timers := convert(t, export(
		[]obj{
			timerAction("t1", true, send("Twitter"), send("YouTube"), send("Discord")),
			timerAction("t2", false, send("Follow!")),
			timerAction("t3", false, send("once")),
			timerAction("t4", false, send("Hi %user%")),
			timerAction("t5", false, send("a"), send("b")),
		},
		nil,
		[]obj{
			sbTimer("t1", nil),
			sbTimer("t2", obj{"interval": 90, "randomInterval": true, "upperInterval": 300, "lines": 0}),
			sbTimer("t3", obj{"repeat": false}),
			sbTimer("t4", nil),
			sbTimer("t5", nil),
		}))
	if tm := timers[0].Timer; timers[0].Skip != nil || len(tm.Messages) != 3 || tm.Order != config.TimerRandom || tm.IntervalMinutes != 15 || tm.MinLines != 5 || tm.Enabled {
		t.Fatalf("socials: %v %+v", timers[0], tm)
	}
	var notes []string
	for _, n := range timers[1].Notes {
		notes = append(notes, errKey(n))
	}
	if tm := timers[1].Timer; timers[1].Skip != nil || tm.IntervalMinutes != config.MinTimerMinutes ||
		!slices.Equal(notes, []string{"note.sbRandomInterval", "note.sbIntervalRaised"}) {
		t.Fatalf("short timer: %v %+v %v", timers[1], tm, notes)
	}
	for i, key := range map[int]string{2: "err.sbTimerOnce", 3: "err.sbVariable", 4: "err.sbManyMessages"} {
		if errKey(timers[i].Skip) != key {
			t.Errorf("timer %d: got %v, want %s", i, timers[i].Skip, key)
		}
	}
}

func sound(file string) obj {
	return obj{"type": subPlaySound, "enabled": true, "soundFile": file, "volume": 0.3}
}

func TestSounds(t *testing.T) {
	delay := obj{"type": subDelay, "enabled": true, "value": "1000"}
	cmds, timers := convert(t, export(
		[]obj{
			cmdAction("bonk", false, sound("/sounds/bonk.mp3"), delay, send("%userName% bonks %targetUserName%")),
			cmdAction("horn", false, sound("/sounds/horn.mp3")),
			cmdAction("two", false, sound("/a.mp3"), sound("/b.mp3")),
			cmdAction("either", true, sound("/a.mp3"), send("x")),
			timerAction("t", false, send("x"), sound("/a.mp3")),
		},
		[]obj{cmd("bonk", "!bonk", nil), cmd("horn", "!horn", nil), cmd("two", "!two", nil), cmd("either", "!either", nil)},
		[]obj{sbTimer("t", nil)}))
	if c := cmds[0].Command; cmds[0].Skip != nil || c.Sound != "/sounds/bonk.mp3" || c.Volume != 0.3 || c.Response != "{user} bonks {target}" {
		t.Fatalf("bonk: %v %+v", cmds[0], c)
	}
	if c := cmds[1].Command; cmds[1].Skip != nil || c.Sound != "/sounds/horn.mp3" || c.Response != "" {
		t.Fatalf("horn: %v %+v", cmds[1], c)
	}
	for i, key := range map[int]string{2: "err.sbManySounds", 3: "err.sbRandomAnswers"} {
		if errKey(cmds[i].Skip) != key {
			t.Errorf("command %d: got %v, want %s", i, cmds[i].Skip, key)
		}
	}
	if errKey(timers[0].Skip) != "err.sbTimerSound" {
		t.Errorf("timer with sound: %v", timers[0].Skip)
	}
}

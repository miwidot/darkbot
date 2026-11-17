//go:build darwin

package autostart

import (
	"encoding/xml"
	"strings"
	"testing"
)

func TestLaunchAgentIsValidXMLAndEscapes(t *testing.T) {
	data := launchAgent(`/Applications/Dark & Bot/darkbot.app/Contents/MacOS/darkbot`)
	if err := xml.Unmarshal(data, new(struct{})); err != nil {
		t.Fatalf("plist is not valid XML: %v", err)
	}
	if !strings.Contains(string(data), "Dark &amp; Bot") || !strings.Contains(string(data), "<key>RunAtLoad</key>") {
		t.Fatalf("unexpected plist:\n%s", data)
	}
}

func TestSetAndRemove(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	if on, _ := Enabled(); on {
		t.Fatal("fresh home must have no entry")
	}
	if err := Set(true); err != nil {
		t.Fatal(err)
	}
	if on, err := Enabled(); !on || err != nil {
		t.Fatalf("enabled=%v err=%v", on, err)
	}
	if err := Refresh(); err != nil {
		t.Fatal(err)
	}
	if err := Set(false); err != nil {
		t.Fatal(err)
	}
	if on, _ := Enabled(); on {
		t.Fatal("entry must be gone")
	}
	if err := Set(false); err != nil {
		t.Fatal("removing twice must not fail")
	}
}

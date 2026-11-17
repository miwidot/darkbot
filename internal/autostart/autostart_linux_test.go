//go:build linux

package autostart

import (
	"strings"
	"testing"
)

func TestDesktopEntryQuoting(t *testing.T) {
	got := desktopEntry(`/home/jane doe/$apps/dark"bot`)
	// argument level: "/home/jane doe/\$apps/dark\"bot", then each \ doubled
	want := `Exec="/home/jane doe/\\$apps/dark\\"bot"`
	if !strings.Contains(got, want) {
		t.Fatalf("got:\n%s\nwant line %s", got, want)
	}
}

func TestSetAndRemove(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	if err := Set(true); err != nil {
		t.Fatal(err)
	}
	if on, err := Enabled(); !on || err != nil {
		t.Fatalf("enabled=%v err=%v", on, err)
	}
	if err := Set(false); err != nil {
		t.Fatal(err)
	}
	if on, _ := Enabled(); on {
		t.Fatal("entry must be gone")
	}
}

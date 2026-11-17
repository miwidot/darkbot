//go:build windows

package autostart

import (
	"strings"
	"testing"

	"golang.org/x/sys/windows/registry"
)

// Uses a test key under HKCU, never the real Run key.
func TestSetAndRemoveWindows(t *testing.T) {
	const testKey = `Software\darkbot-test\Run`
	old := runKey
	runKey = testKey
	t.Cleanup(func() {
		runKey = old
		_ = registry.DeleteKey(registry.CURRENT_USER, testKey)
		_ = registry.DeleteKey(registry.CURRENT_USER, `Software\darkbot-test`)
	})
	if on, err := Enabled(); on || err != nil {
		t.Fatalf("fresh key: enabled=%v err=%v", on, err)
	}
	if err := Set(true); err != nil {
		t.Fatal(err)
	}
	k, err := registry.OpenKey(registry.CURRENT_USER, testKey, registry.QUERY_VALUE)
	if err != nil {
		t.Fatal(err)
	}
	v, _, err := k.GetStringValue(entryName)
	k.Close()
	exe, _ := executable()
	if err != nil || v != `"`+exe+`" `+TrayArg || !strings.HasPrefix(v, `"`) {
		t.Fatalf("entry %q, %v", v, err)
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
		t.Fatalf("removing twice must be fine: %v", err)
	}
}

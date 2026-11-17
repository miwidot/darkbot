//go:build linux

package autostart

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
)

// Supported reports whether autostart works on this system.
func Supported() bool { return true }

func desktopPath() (string, error) {
	dir, err := os.UserConfigDir() // $XDG_CONFIG_HOME or ~/.config
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "autostart", entryName+".desktop"), nil
}

// Enabled reports whether the autostart entry exists.
func Enabled() (bool, error) {
	p, err := desktopPath()
	if err != nil {
		return false, err
	}
	_, err = os.Stat(p)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	return err == nil, err
}

// Set writes or removes the XDG autostart entry.
func Set(on bool) error {
	p, err := desktopPath()
	if err != nil {
		return err
	}
	if !on {
		if err := os.Remove(p); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
		return nil
	}
	exe, err := executable()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		return err
	}
	return os.WriteFile(p, []byte(desktopEntry(exe)), 0o644)
}

// desktopEntry builds the .desktop file. The Desktop Entry spec escapes Exec
// twice: inside the quoted argument ", `, $ and \ get a backslash, then the
// whole value is a "string" in which every backslash is written doubled.
func desktopEntry(exe string) string {
	arg := strings.NewReplacer(`\`, `\\`, `"`, `\"`, "`", "\\`", `$`, `\$`).Replace(exe)
	quoted := strings.ReplaceAll(`"`+arg+`"`, `\`, `\\`)
	return "[Desktop Entry]\nType=Application\nName=darkbot\nExec=" + quoted + "\nX-GNOME-Autostart-enabled=true\nTerminal=false\n"
}

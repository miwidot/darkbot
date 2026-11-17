// Package autostart starts darkbot when the user logs in to the computer.
// Per user, no admin rights:
//
//   - Windows: value "darkbot" under HKCU\Software\Microsoft\Windows\CurrentVersion\Run
//   - macOS:   ~/Library/LaunchAgents/com.miwidot.darkbot.plist (RunAtLoad)
//   - Linux:   ~/.config/autostart/darkbot.desktop (XDG autostart)
//
// The entry points at the running program. The self-updater replaces the
// program in place, so the entry stays valid across updates; if the user
// moves darkbot, Refresh re-points an existing entry on the next start.
package autostart

import (
	"os"
	"path/filepath"
)

// entryName is the name of the autostart entry on every platform.
const entryName = "darkbot"

// TrayArg is passed by the Windows autostart entry: darkbot then starts
// hidden in the tray instead of opening its window on every login.
const TrayArg = "--tray"

// executable returns the absolute path of the running program.
func executable() (string, error) {
	p, err := os.Executable()
	if err != nil {
		return "", err
	}
	if resolved, err := filepath.EvalSymlinks(p); err == nil {
		p = resolved
	}
	return p, nil
}

// Refresh re-registers an existing entry so it points at the program that
// runs now (darkbot was moved). It does nothing when autostart is off.
func Refresh() error {
	on, err := Enabled()
	if err != nil || !on {
		return err
	}
	return Set(true)
}

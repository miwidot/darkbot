//go:build darwin

package autostart

import (
	"bytes"
	"encoding/xml"
	"errors"
	"os"
	"path/filepath"
)

const launchAgentLabel = "com.miwidot.darkbot"

// Supported reports whether autostart works on this system.
func Supported() bool { return true }

func plistPath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, "Library", "LaunchAgents", launchAgentLabel+".plist"), nil
}

// Enabled reports whether the LaunchAgent exists.
func Enabled() (bool, error) {
	p, err := plistPath()
	if err != nil {
		return false, err
	}
	_, err = os.Stat(p)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	return err == nil, err
}

// Set writes or removes the LaunchAgent. launchd reads it at the next login.
func Set(on bool) error {
	p, err := plistPath()
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
	return os.WriteFile(p, launchAgent(exe), 0o644)
}

// launchAgent builds the plist; the path is XML-escaped.
func launchAgent(exe string) []byte {
	var esc bytes.Buffer
	_ = xml.EscapeText(&esc, []byte(exe))
	return []byte(`<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
	<key>Label</key>
	<string>` + launchAgentLabel + `</string>
	<key>ProgramArguments</key>
	<array>
		<string>` + esc.String() + `</string>
	</array>
	<key>RunAtLoad</key>
	<true/>
	<key>ProcessType</key>
	<string>Interactive</string>
</dict>
</plist>
`)
}

//go:build !windows && !darwin && !linux

package autostart

import "errors"

var errUnsupported = errors.New("autostart is not supported on this system")

// Supported reports whether autostart works on this system.
func Supported() bool { return false }

// Enabled is always false here.
func Enabled() (bool, error) { return false, nil }

// Set is not available here.
func Set(bool) error { return errUnsupported }

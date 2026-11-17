//go:build !windows

package tray

// Supported reports whether this system shows darkbot in the tray.
func Supported() bool { return false }

// Start does nothing on this system.
func Start(Texts, Handlers) {}

// SetTexts does nothing on this system.
func SetTexts(Texts) {}

// Stop does nothing on this system.
func Stop() {}

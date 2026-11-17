//go:build windows

package selfupdate

import (
	"time"

	"golang.org/x/sys/windows"
)

// WaitForExit blocks until process pid has exited or timeout passed.
// It reports false on timeout.
func WaitForExit(pid int, timeout time.Duration) bool {
	h, err := windows.OpenProcess(windows.SYNCHRONIZE, false, uint32(pid))
	if err != nil {
		return true // no such process (any more)
	}
	defer windows.CloseHandle(h)
	ev, err := windows.WaitForSingleObject(h, uint32(timeout.Milliseconds()))
	return err == nil && ev == windows.WAIT_OBJECT_0
}

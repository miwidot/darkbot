//go:build !windows

package selfupdate

import (
	"errors"
	"syscall"
	"time"
)

// waitPoll: how often the process table is checked.
const waitPoll = 100 * time.Millisecond

// WaitForExit blocks until process pid has exited or timeout passed.
// It reports false on timeout.
func WaitForExit(pid int, timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for {
		// Signal 0 only checks that the process exists.
		if err := syscall.Kill(pid, 0); errors.Is(err, syscall.ESRCH) {
			return true
		}
		if time.Now().After(deadline) {
			return false
		}
		time.Sleep(waitPoll)
	}
}

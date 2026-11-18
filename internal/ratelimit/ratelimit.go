// Package ratelimit caps how often something may happen within a sliding
// time window (e.g. chat messages per 30 seconds).
package ratelimit

import (
	"slices"
	"sync"
	"time"
)

// Window allows at most Max events within any Per. The zero value allows
// nothing; create it with New.
type Window struct {
	max int
	per time.Duration

	mu     sync.Mutex
	times  []time.Time // allowed events within per, oldest first
	denied bool        // an event was denied since the last allowed one
}

// New creates a window of max events per per.
func New(max int, per time.Duration) *Window {
	return &Window{max: max, per: per}
}

// Allow reports whether an event at now fits and, if so, counts it.
// first is true for the first denial after an allowed event, so callers
// can log once per burst instead of once per dropped event.
func (w *Window) Allow(now time.Time) (ok, first bool) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.times = slices.DeleteFunc(w.times, func(t time.Time) bool { return now.Sub(t) >= w.per })
	if len(w.times) >= w.max {
		first = !w.denied
		w.denied = true
		return false, first
	}
	w.times = append(w.times, now)
	w.denied = false
	return true, false
}

// Package activity keeps the activity feed on disk, so what happened while
// the window was closed, between streams or before an update is still there
// after a restart.
//
// Entries are stored as the JSON the UI already renders (bot, song and
// channel event activities), newest first, at most MaxEntries.
package activity

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"slices"
	"sync"
	"time"

	"github.com/miwidot/darkbot/internal/secret"
)

const (
	// MaxEntries is how many feed lines are kept; older ones are dropped.
	// The UI shows the same number.
	MaxEntries = 200
	// saveDelay batches disk writes: a sub bomb or a raid is one write.
	saveDelay = 2 * time.Second
)

// Logger is the logging this package needs.
type Logger interface {
	Warnf(format string, args ...any)
}

type entry struct {
	at   time.Time
	data json.RawMessage
}

// Log is the kept activity feed.
type Log struct {
	path string
	log  Logger

	mu        sync.Mutex
	entries   []entry // newest first
	saveTimer *time.Timer

	// writeMu orders the disk writes: each takes its snapshot while
	// holding it, so a save can't write an older list over a newer one
	// (e.g. bring back a feed that was just cleared).
	writeMu sync.Mutex
}

// stamped reads the "at" field every feed entry has.
type stamped struct {
	At time.Time `json:"at"`
}

// Open loads the feed from path. A missing or unreadable file starts an
// empty feed: a lost feed must never stop darkbot from starting.
func Open(path string, log Logger) *Log {
	l := &Log{path: path, log: log}
	data, err := os.ReadFile(path)
	switch {
	case errors.Is(err, os.ErrNotExist):
		return l
	case err != nil:
		log.Warnf("Could not read the activity feed %s: %v", path, err)
		return l
	}
	var raw []json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		log.Warnf("Ignoring unreadable activity feed %s: %v", path, err)
		return l
	}
	for _, r := range raw {
		var s stamped
		if err := json.Unmarshal(r, &s); err != nil || s.At.IsZero() {
			continue // not a feed entry; skip it rather than lose the rest
		}
		l.entries = append(l.entries, entry{at: s.At, data: r})
	}
	l.sortAndTrim()
	return l
}

// Add keeps one feed entry. v must marshal to an object with an "at" time.
func (l *Log) Add(v any) error {
	data, err := json.Marshal(v)
	if err != nil {
		return fmt.Errorf("activity entry: %w", err)
	}
	var s stamped
	if err := json.Unmarshal(data, &s); err != nil || s.At.IsZero() {
		return fmt.Errorf("activity entry has no time: %s", data)
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	l.entries = append(l.entries, entry{at: s.At, data: data})
	l.sortAndTrim()
	l.scheduleSave()
	return nil
}

// List returns the feed, newest first.
func (l *Log) List() []json.RawMessage {
	l.mu.Lock()
	defer l.mu.Unlock()
	out := make([]json.RawMessage, len(l.entries))
	for i, e := range l.entries {
		out[i] = e.data
	}
	return out
}

// Clear empties the feed and saves that right away.
func (l *Log) Clear() error {
	l.writeMu.Lock()
	defer l.writeMu.Unlock()
	l.mu.Lock()
	l.entries = nil
	if l.saveTimer != nil {
		l.saveTimer.Stop()
		l.saveTimer = nil
	}
	l.mu.Unlock()
	return l.write(nil)
}

// sortAndTrim orders newest first (an entry may arrive late, e.g. a follow
// found after darkbot was off) and drops the oldest past MaxEntries; call
// with mu held or before the Log is shared.
func (l *Log) sortAndTrim() {
	slices.SortStableFunc(l.entries, func(a, b entry) int { return b.at.Compare(a.at) })
	if len(l.entries) > MaxEntries {
		l.entries = slices.Clip(l.entries[:MaxEntries])
	}
}

// scheduleSave writes after saveDelay; call with mu held.
func (l *Log) scheduleSave() {
	if l.saveTimer == nil {
		l.saveTimer = time.AfterFunc(saveDelay, l.saveNow)
	}
}

func (l *Log) saveNow() {
	l.writeMu.Lock()
	defer l.writeMu.Unlock()
	l.mu.Lock()
	l.saveTimer = nil
	l.mu.Unlock()
	if err := l.write(l.List()); err != nil {
		l.log.Warnf("Could not save the activity feed: %v", err)
	}
}

func (l *Log) write(list []json.RawMessage) error {
	if list == nil {
		list = []json.RawMessage{}
	}
	data, err := json.Marshal(list)
	if err != nil {
		return err
	}
	if err := secret.WriteFileAtomic(l.path, data); err != nil {
		return fmt.Errorf("write %s: %w", l.path, err)
	}
	return nil
}

// Flush saves a pending change now (call on shutdown).
func (l *Log) Flush() {
	l.mu.Lock()
	pending := l.saveTimer != nil
	if pending {
		l.saveTimer.Stop()
		l.saveTimer = nil
	}
	l.mu.Unlock()
	if pending {
		l.saveNow()
	}
}

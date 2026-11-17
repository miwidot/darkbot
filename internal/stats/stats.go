// Package stats counts what happened for the dashboard: per session (since
// darkbot started or the user reset it) and in total (saved to disk).
package stats

import (
	"cmp"
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
	// TopN is how many sounds and viewers the dashboard ranks.
	TopN = 5
	// saveDelay batches disk writes: a burst of redemptions is one write.
	saveDelay = 2 * time.Second
)

// Counts are the dashboard numbers.
type Counts struct {
	Redemptions   int `json:"redemptions"`   // channel point redemptions of a sound reward
	Played        int `json:"played"`        // sounds that played to the end (no previews)
	Refused       int `json:"refused"`       // redemptions refused (cooldown, off, queue full, error)
	Commands      int `json:"commands"`      // chat commands darkbot handled
	SongsAdded    int `json:"songsAdded"`    // song requests that went into the Spotify queue
	SongsRejected int `json:"songsRejected"` // song requests that were refused
	Skips         int `json:"skips"`         // songs skipped with !skip
}

// Entry is one ranked name.
type Entry struct {
	Name  string `json:"name"`
	Count int    `json:"count"`
}

// Snapshot is what the UI shows.
type Snapshot struct {
	Since      time.Time `json:"since"` // session start
	TotalSince time.Time `json:"totalSince"`
	Session    Counts    `json:"session"`
	Total      Counts    `json:"total"`
	TopSounds  []Entry   `json:"topSounds"`
	TopViewers []Entry   `json:"topViewers"`
	// Viewers is how many different viewers redeemed a sound or got a song
	// into the queue this session.
	Viewers int `json:"viewers"`
}

type persisted struct {
	Since time.Time `json:"since"`
	Total Counts    `json:"total"`
}

// Logger is the minimal logging interface this package needs.
type Logger interface {
	Warnf(format string, args ...any)
}

// Stats is safe for concurrent use.
type Stats struct {
	path     string
	log      Logger
	onChange func()

	mu         sync.Mutex
	since      time.Time
	totalSince time.Time
	session    Counts
	total      Counts
	sounds     map[string]int
	viewers    map[string]int
	saveTimer  *time.Timer
}

// Open loads the totals from path (missing or unreadable = start at zero;
// lost numbers must never stop darkbot from starting).
func Open(path string, log Logger) *Stats {
	now := time.Now()
	s := &Stats{path: path, log: log, since: now, totalSince: now, sounds: map[string]int{}, viewers: map[string]int{}}
	data, err := os.ReadFile(path)
	switch {
	case errors.Is(err, os.ErrNotExist):
	case err != nil:
		log.Warnf("Could not read statistics %s: %v", path, err)
	default:
		var p persisted
		if err := json.Unmarshal(data, &p); err != nil {
			log.Warnf("Ignoring unreadable statistics %s: %v", path, err)
		} else {
			s.total = p.Total
			if !p.Since.IsZero() {
				s.totalSince = p.Since
			}
		}
	}
	return s
}

// OnChange is called (outside the lock) after every change.
func (s *Stats) OnChange(fn func()) {
	s.mu.Lock()
	s.onChange = fn
	s.mu.Unlock()
}

// update applies fn to both counters and schedules a save.
func (s *Stats) update(fn func(c *Counts), rank func()) {
	s.mu.Lock()
	fn(&s.session)
	fn(&s.total)
	if rank != nil {
		rank()
	}
	if s.saveTimer == nil {
		s.saveTimer = time.AfterFunc(saveDelay, s.saveNow)
	}
	cb := s.onChange
	s.mu.Unlock()
	if cb != nil {
		cb()
	}
}

// Redemption counts a redemption of a sound reward; accepted=false means
// darkbot refused it right away.
func (s *Stats) Redemption(user, sound string, accepted bool) {
	s.update(func(c *Counts) {
		c.Redemptions++
		if !accepted {
			c.Refused++
		}
	}, func() {
		s.sounds[sound]++
		s.viewers[user]++
	})
}

// Played counts a sound that played (not a preview).
func (s *Stats) Played() { s.update(func(c *Counts) { c.Played++ }, nil) }

// Failed counts an accepted redemption whose playback failed.
func (s *Stats) Failed() { s.update(func(c *Counts) { c.Refused++ }, nil) }

// Command counts a handled chat command.
func (s *Stats) Command() { s.update(func(c *Counts) { c.Commands++ }, nil) }

// SongAdded counts a song request that was queued.
func (s *Stats) SongAdded(user string) {
	s.update(func(c *Counts) { c.SongsAdded++ }, func() { s.viewers[user]++ })
}

// SongRejected counts a refused song request.
func (s *Stats) SongRejected() { s.update(func(c *Counts) { c.SongsRejected++ }, nil) }

// Skip counts a skipped song.
func (s *Stats) Skip() { s.update(func(c *Counts) { c.Skips++ }, nil) }

// ResetSession starts a new session (e.g. a new stream). Totals stay.
func (s *Stats) ResetSession() {
	s.mu.Lock()
	s.since = time.Now()
	s.session = Counts{}
	s.sounds = map[string]int{}
	s.viewers = map[string]int{}
	cb := s.onChange
	s.mu.Unlock()
	if cb != nil {
		cb()
	}
}

// Snapshot returns the current numbers.
func (s *Stats) Snapshot() Snapshot {
	s.mu.Lock()
	defer s.mu.Unlock()
	return Snapshot{
		Since: s.since, TotalSince: s.totalSince,
		Session: s.session, Total: s.total,
		TopSounds: top(s.sounds), TopViewers: top(s.viewers),
		Viewers: len(s.viewers),
	}
}

// top ranks by count, then name, so equal counts don't jump around.
func top(m map[string]int) []Entry {
	out := make([]Entry, 0, len(m))
	for name, n := range m {
		if name != "" {
			out = append(out, Entry{name, n})
		}
	}
	slices.SortFunc(out, func(a, b Entry) int {
		if c := cmp.Compare(b.Count, a.Count); c != 0 {
			return c
		}
		return cmp.Compare(a.Name, b.Name)
	})
	return out[:min(TopN, len(out))]
}

func (s *Stats) saveNow() {
	s.mu.Lock()
	s.saveTimer = nil
	p := persisted{Since: s.totalSince, Total: s.total}
	s.mu.Unlock()
	if err := s.write(p); err != nil {
		s.log.Warnf("Could not save statistics: %v", err)
	}
}

func (s *Stats) write(p persisted) error {
	data, err := json.MarshalIndent(p, "", "  ")
	if err != nil {
		return err
	}
	if err := secret.WriteFileAtomic(s.path, data); err != nil {
		return fmt.Errorf("write %s: %w", s.path, err)
	}
	return nil
}

// Flush saves pending totals now (call on shutdown).
func (s *Stats) Flush() {
	s.mu.Lock()
	pending := s.saveTimer != nil
	if pending {
		s.saveTimer.Stop()
	}
	s.mu.Unlock()
	if pending {
		s.saveNow()
	}
}

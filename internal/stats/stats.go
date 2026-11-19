// Package stats counts what happened for the dashboard: per session (since
// the stream started or the user reset it) and in total. Both are saved to
// disk, so a restart or an update in the middle of a stream keeps the
// session; the next stream starts a new one.
package stats

import (
	"cmp"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
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
	Follows       int `json:"follows"`
	Unfollows     int `json:"unfollows"`  // only with unfollow detection on
	Subs          int `json:"subs"`       // new subs and resubs (not gift recipients)
	GiftedSubs    int `json:"giftedSubs"` // subs given as gifts
	Bits          int `json:"bits"`
	Raids         int `json:"raids"`
	RaidViewers   int `json:"raidViewers"` // viewers brought by raids
	// ChatMessages: viewers' chat messages (not the broadcaster's, not bots').
	ChatMessages int `json:"chatMessages"`
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
	Since   time.Time         `json:"since"`
	Total   Counts            `json:"total"`
	Session *persistedSession `json:"session,omitempty"`
	// Days: the history of the statistics view (people.go).
	Days []*day `json:"days,omitempty"`
}

// persistedSession is the running session (files from before v0.2.8
// have none: the session then starts at launch, as it used to).
type persistedSession struct {
	Since   time.Time      `json:"since"`
	Counts  Counts         `json:"counts"`
	Sounds  map[string]int `json:"sounds"`
	Viewers map[string]int `json:"viewers"`
	People  people         `json:"people,omitempty"`
	Capped  bool           `json:"peopleCapped,omitempty"`
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

	// people.go: this session's people, the day history and the accounts
	// that never count.
	people       people
	peopleCapped bool
	days         []*day
	ignored      map[string]bool
	now          func() time.Time
}

// Open loads the totals from path (missing or unreadable = start at zero;
// lost numbers must never stop darkbot from starting).
func Open(path string, log Logger) *Stats {
	now := time.Now()
	s := &Stats{path: path, log: log, since: now, totalSince: now, sounds: map[string]int{}, viewers: map[string]int{}, people: people{}, now: time.Now}
	s.SetIgnored(nil)
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
			if ps := p.Session; ps != nil && !ps.Since.IsZero() {
				s.since, s.session = ps.Since, ps.Counts
				if ps.Sounds != nil {
					s.sounds = ps.Sounds
				}
				if ps.Viewers != nil {
					s.viewers = ps.Viewers
				}
				if ps.People != nil {
					s.people, s.peopleCapped = ps.People, ps.Capped
				}
			}
			for _, d := range p.Days {
				if d == nil {
					continue
				}
				if d.People == nil {
					d.People = people{}
				}
				s.days = append(s.days, d)
			}
			slices.SortFunc(s.days, func(a, b *day) int { return cmp.Compare(a.Date, b.Date) })
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
	s.scheduleSave()
	cb := s.onChange
	s.mu.Unlock()
	if cb != nil {
		cb()
	}
}

// scheduleSave writes after saveDelay; call with mu held.
func (s *Stats) scheduleSave() {
	if s.saveTimer == nil {
		s.saveTimer = time.AfterFunc(saveDelay, s.saveNow)
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

// Follow, Unfollow, Sub, Gift, Cheer and Raid count channel events. id and
// user (Twitch user ID and display name, "" when anonymous) rank the viewer.
func (s *Stats) Follow(user string) {
	s.update(func(c *Counts) { c.Follows++ }, nil)
}
func (s *Stats) Unfollow() { s.update(func(c *Counts) { c.Unfollows++ }, nil) }
func (s *Stats) Sub(id, user string) {
	s.update(func(c *Counts) { c.Subs++ }, func() {
		s.rankViewer(user)
		s.todayLocked(s.now()).Subs++
		s.touchLocked(id, "", user, func(p *Person) { p.Subs++ })
	})
}
func (s *Stats) Gift(id, user string, n int) {
	s.update(func(c *Counts) { c.GiftedSubs += n }, func() {
		s.rankViewer(user)
		s.todayLocked(s.now()).Subs += n
		s.touchLocked(id, "", user, func(p *Person) { p.Subs += n })
	})
}
func (s *Stats) Cheer(id, user string, bits int) {
	s.update(func(c *Counts) { c.Bits += bits }, func() {
		s.rankViewer(user)
		s.todayLocked(s.now()).Bits += bits
		s.touchLocked(id, "", user, func(p *Person) { p.Bits += bits })
	})
}
func (s *Stats) Raid(id, user string, viewers int) {
	s.update(func(c *Counts) { c.Raids++; c.RaidViewers += viewers }, func() {
		s.touchLocked(id, "", user, func(p *Person) { p.Raid += viewers })
	})
}

// rankViewer must run under mu; anonymous gifts/cheers aren't ranked.
func (s *Stats) rankViewer(user string) {
	if user != "" {
		s.viewers[user]++
	}
}

// ResetSession starts a new session now (the user's reset, or a stream
// that just started). Totals stay.
func (s *Stats) ResetSession() { s.ResetSessionAt(time.Now()) }

// ResetSessionAt starts a new session that began at since (a stream that
// started while darkbot was not running).
func (s *Stats) ResetSessionAt(since time.Time) {
	s.mu.Lock()
	s.since = since
	s.session = Counts{}
	s.sounds = map[string]int{}
	s.viewers = map[string]int{}
	s.people, s.peopleCapped = people{}, false
	s.scheduleSave()
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
	s.pruneDaysLocked(s.now())
	p := persisted{Since: s.totalSince, Total: s.total, Session: &persistedSession{
		Since: s.since, Counts: s.session, Sounds: maps.Clone(s.sounds), Viewers: maps.Clone(s.viewers),
		People: s.people, Capped: s.peopleCapped,
	}, Days: s.days}
	// Marshalled under the lock: People and Days are shared, not copied.
	// Compact, not indented: the history makes up most of the file.
	data, err := json.Marshal(p)
	s.mu.Unlock()
	if err != nil {
		s.log.Warnf("Could not save statistics: %v", err)
		return
	}
	if err := s.write(data); err != nil {
		s.log.Warnf("Could not save statistics: %v", err)
	}
}

func (s *Stats) write(data []byte) error {
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

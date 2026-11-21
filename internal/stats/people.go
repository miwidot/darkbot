package stats

import (
	"cmp"
	"encoding/json"
	"errors"
	"os"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/miwidot/darkbot/internal/i18n"
)

// People: who chatted, subbed, cheered and raided, for the statistics
// view. Two scopes: this session (the stream) and the history, kept per
// local calendar day with chat messages per hour for the activity graph.
//
// Size: today (and the session) lives in stats.json, which is saved a few
// seconds after every change. Finished days move to a history file that
// is written only when a day ends, so a busy chat never rewrites the whole
// history. A day keeps at most MaxPeople people; once it is older than
// FullDays it keeps only its OldDayPeople most active ones. The top lists
// stay right (whoever makes a 90-day top 5 is never below place 200 on
// their days); only "different chatters" over a long range becomes a
// floor, shown as such.

const (
	// FullDays: the last days kept with everyone (up to MaxPeople).
	FullDays = 7
	// MaxHistoryDays: the longest range; older days are dropped.
	MaxHistoryDays = 90
	// MaxPeople caps the people kept per day and per session. When a new
	// one would exceed it, the least active one is dropped (see evict).
	MaxPeople = 1000
	// OldDayPeople: the people a day keeps once it is older than FullDays.
	OldDayPeople = 200
	// dayLayout is the key of a calendar day (local time).
	dayLayout   = "2006-01-02"
	hoursPerDay = 24
)

// Ranges are the statistics view's time ranges in days.
var Ranges = []int{FullDays, 30, 60, MaxHistoryDays}

// KnownBots are chat bot accounts (Twitch logins) that never count: the
// common bot services. Twitch has no flag for bots in chat messages (only a
// badge for app-token bots, see https://dev.twitch.tv/docs/chat/), so it
// goes by name; the streamer adds their own in the statistics view.
var KnownBots = []string{
	"nightbot", "streamelements", "streamlabs", "moobot", "fossabot",
	"wizebot", "sery_bot", "botrixoficial", "kofistreambot", "soundalerts",
	"streamlootsbot", "pokemoncommunitygame", "commanderroot", "lumiastream",
	"blerp", "tangiabot", "songlistbot", "frostytoolsdotcom", "streamstickers",
}

// MaxIgnored caps the streamer's own list of ignored accounts.
const MaxIgnored = 100

// loginPattern: a Twitch login (letters, digits, underscore; at most 25).
var loginPattern = regexp.MustCompile(`^[a-z0-9_]{1,25}$`)

// NormalizeLogins cleans up the list of ignored accounts: lower case, no
// "@", no duplicates or built-in bots, sorted. A name that can't be a
// Twitch login is refused.
func NormalizeLogins(in []string) ([]string, error) {
	out := []string{}
	for _, l := range in {
		l = strings.ToLower(strings.TrimPrefix(strings.TrimSpace(l), "@"))
		switch {
		case l == "":
			continue
		case !loginPattern.MatchString(l):
			return nil, i18n.E("err.ignoredName", "name", l)
		case slices.Contains(KnownBots, l) || slices.Contains(out, l):
			continue
		}
		out = append(out, l)
	}
	if len(out) > MaxIgnored {
		return nil, i18n.E("err.ignoredMany", "max", MaxIgnored)
	}
	slices.Sort(out)
	return out, nil
}

// Person is one viewer's numbers in a scope.
type Person struct {
	Name string `json:"name"` // display name (the latest seen)
	// Login: only from chat, and only when it differs from the display
	// name in lower case (non-Latin display names).
	Login    string `json:"login,omitempty"`
	Messages int    `json:"messages,omitempty"`
	// Subs: own subs and resubs plus subs gifted to others.
	Subs int `json:"subs,omitempty"`
	Bits int `json:"bits,omitempty"`
	// Raid: viewers brought by their raids.
	Raid int `json:"raid,omitempty"`
}

// activity is what decides who is dropped when a scope is full.
func (p *Person) activity() int { return p.Messages + p.Subs + p.Bits + p.Raid }

// people is a capped set of persons by key (Twitch user ID).
type people map[string]*Person

// get returns the person for key, adding them (and dropping the least
// active one when full); capped reports that someone was dropped.
func (ps people) get(key, name string) (p *Person, capped bool) {
	if p = ps[key]; p == nil {
		if len(ps) >= MaxPeople {
			ps.evict()
			capped = true
		}
		p = &Person{}
		ps[key] = p
	}
	if name != "" {
		p.Name = name
	}
	return p, capped
}

// evict drops the least active person (ties: the smallest key, so it is
// deterministic).
func (ps people) evict() {
	var low string
	for k, p := range ps {
		if low == "" || p.activity() < ps[low].activity() || (p.activity() == ps[low].activity() && k < low) {
			low = k
		}
	}
	delete(ps, low)
}

// day is one calendar day of the history.
type day struct {
	Date  string           `json:"date"`
	Hours [hoursPerDay]int `json:"hours"` // chat messages per local hour
	Subs  int              `json:"subs"`  // incl. gifts and anonymous ones
	Bits  int              `json:"bits"`  // incl. anonymous ones
	// People is never nil after load.
	People people `json:"people"`
	// Capped: MaxPeople was reached, so the chatter count is a floor.
	Capped bool `json:"capped,omitempty"`
	// Trimmed: cut down to OldDayPeople; Chatters is then the day's
	// chatter count from before the cut.
	Trimmed  bool `json:"trimmed,omitempty"`
	Chatters int  `json:"chatters,omitempty"`
}

func (d *day) messages() int {
	n := 0
	for _, h := range d.Hours {
		n += h
	}
	return n
}

// trim keeps the n most active people (ties: the smaller key).
func (d *day) trim(n int, chatters int) {
	d.Chatters, d.Trimmed = chatters, true
	if len(d.People) <= n {
		return
	}
	keys := make([]string, 0, len(d.People))
	for k := range d.People {
		keys = append(keys, k)
	}
	slices.SortFunc(keys, func(a, b string) int {
		if c := cmp.Compare(d.People[b].activity(), d.People[a].activity()); c != 0 {
			return c
		}
		return cmp.Compare(a, b)
	})
	// A new map: Go maps never shrink, so deleting from the old one would
	// keep the memory of all MaxPeople.
	kept := make(people, n)
	for _, k := range keys[:n] {
		kept[k] = d.People[k]
	}
	d.People = kept
}

// merge adds o (the same date) into d; only after a clock change.
func (d *day) merge(o *day) {
	for h := range d.Hours {
		d.Hours[h] += o.Hours[h]
	}
	d.Subs += o.Subs
	d.Bits += o.Bits
	d.Capped = d.Capped || o.Capped
	for k, p := range o.People {
		q, capped := d.People.get(k, p.Name)
		q.Login = cmp.Or(q.Login, p.Login)
		q.Messages += p.Messages
		q.Subs += p.Subs
		q.Bits += p.Bits
		q.Raid += p.Raid
		d.Capped = d.Capped || capped
	}
}

// validDays drops broken entries of a loaded file and sorts by date.
func validDays(in []*day) []*day {
	out := make([]*day, 0, len(in))
	for _, d := range in {
		if d == nil {
			continue
		}
		if _, err := time.Parse(dayLayout, d.Date); err != nil {
			continue
		}
		if d.People == nil {
			d.People = people{}
		}
		out = append(out, d)
	}
	slices.SortFunc(out, func(a, b *day) int { return cmp.Compare(a.Date, b.Date) })
	return out
}

// persistedHistory is the history file.
type persistedHistory struct {
	Days []*day `json:"days"`
}

// HistoryPath is the history file next to the statistics file.
func HistoryPath(statsPath string) string {
	return strings.TrimSuffix(statsPath, ".json") + "-history.json"
}

// loadHistory reads the history file (missing or unreadable = empty; lost
// history must never stop darkbot from starting).
func loadHistory(path string, log Logger) []*day {
	data, err := os.ReadFile(path)
	switch {
	case errors.Is(err, os.ErrNotExist):
		return nil
	case err != nil:
		log.Warnf("Could not read the statistics history %s: %v", path, err)
		return nil
	}
	var h persistedHistory
	if err := json.Unmarshal(data, &h); err != nil {
		log.Warnf("Ignoring an unreadable statistics history %s: %v", path, err)
		return nil
	}
	return validDays(h.Days)
}

// rollLocked runs once per date: days before today move to the history,
// days older than FullDays are trimmed and days outside MaxHistoryDays
// (or after today, after a clock change) are dropped. A changed history
// is saved with the next save. Call with mu held.
func (s *Stats) rollLocked(now time.Time) {
	today := now.Format(dayLayout)
	if s.rolledFor == today {
		return
	}
	s.rolledFor = today
	changed := false
	s.days = slices.DeleteFunc(s.days, func(d *day) bool {
		if d.Date == today {
			return false
		}
		if i := slices.IndexFunc(s.closed, func(c *day) bool { return c.Date == d.Date }); i >= 0 {
			s.closed[i].merge(d)
		} else {
			s.closed = append(s.closed, d)
		}
		changed = true
		return true
	})
	slices.SortFunc(s.closed, func(a, b *day) int { return cmp.Compare(a.Date, b.Date) })
	dates := lastDates(now, MaxHistoryDays)
	first, full := dates[0], dates[len(dates)-FullDays]
	n := len(s.closed)
	s.closed = slices.DeleteFunc(s.closed, func(d *day) bool { return d.Date < first || d.Date >= today })
	changed = changed || len(s.closed) != n
	for _, d := range s.closed {
		if d.Date < full && !d.Trimmed {
			d.trim(OldDayPeople, s.chattersLocked(d.People))
			changed = true
		}
	}
	if changed {
		s.historyDirty = true
		s.scheduleSave()
	}
}

// Board is one scope's numbers and top lists.
type Board struct {
	Messages int `json:"messages"`
	// Chatters: different people who wrote at least once.
	Chatters int `json:"chatters"`
	Subs     int `json:"subs"`
	Bits     int `json:"bits"`
	// Capped: the chatter count is a floor (people were left out).
	Capped      bool    `json:"capped"`
	TopChatters []Entry `json:"topChatters"`
	TopSubs     []Entry `json:"topSubs"`
	TopBits     []Entry `json:"topBits"`
	TopRaiders  []Entry `json:"topRaiders"`
}

// DaySummary is one day of the activity graph.
type DaySummary struct {
	Date     string           `json:"date"` // YYYY-MM-DD, local
	Hours    [hoursPerDay]int `json:"hours"`
	Messages int              `json:"messages"`
	Chatters int              `json:"chatters"`
	Capped   bool             `json:"capped"`
}

// Insights is what the statistics view shows.
type Insights struct {
	Since   time.Time `json:"since"` // session start
	Session Board     `json:"session"`
	// Days is the range (one of Ranges); Range its numbers and DaysList
	// its days, oldest first, today last, days without anything included.
	Days     int          `json:"days"`
	Range    Board        `json:"range"`
	DaysList []DaySummary `json:"daysList"`
	// Bots: KnownBots, for the view's "never counted" note.
	Bots []string `json:"bots"`
}

// SetIgnored sets the streamer's own list of accounts that never count
// (logins, any case), on top of KnownBots. Counting already done stays
// stored but is hidden from then on.
func (s *Stats) SetIgnored(logins []string) {
	set := map[string]bool{}
	for _, l := range append(slices.Clone(KnownBots), logins...) {
		if l = strings.ToLower(strings.TrimSpace(l)); l != "" {
			set[l] = true
		}
	}
	s.mu.Lock()
	s.ignored = set
	cb := s.onChange
	s.mu.Unlock()
	if cb != nil {
		cb()
	}
}

// ignoredLocked: call with mu held. Events carry only the display name,
// which is the login in another case for every bot account.
// IsIgnored reports whether an account is one of the bots the statistics
// leave out (KnownBots and the streamer's own list).
func (s *Stats) IsIgnored(login, name string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.ignoredLocked(login, name)
}

func (s *Stats) ignoredLocked(login, name string) bool {
	return s.ignored[strings.ToLower(login)] || s.ignored[strings.ToLower(name)]
}

// todayLocked returns today's day, starting a new one (and rolling the
// finished ones into the history). Call with mu held.
func (s *Stats) todayLocked(now time.Time) *day {
	date := now.Format(dayLayout)
	s.rollLocked(now)
	if n := len(s.days); n > 0 && s.days[n-1].Date == date {
		return s.days[n-1]
	}
	d := &day{Date: date, People: people{}}
	s.days = append(s.days, d)
	return d
}

// lastDates: the dates of the last n days, oldest first. It steps from
// local noon so a daylight saving change can't skip or repeat a day.
func lastDates(now time.Time, n int) []string {
	noon := time.Date(now.Year(), now.Month(), now.Day(), 12, 0, 0, 0, now.Location())
	out := make([]string, n)
	for i := range n {
		out[i] = noon.AddDate(0, 0, i-(n-1)).Format(dayLayout)
	}
	return out
}

// touchLocked applies fn to the person in the session and in today's day.
func (s *Stats) touchLocked(id, login, name string, fn func(p *Person)) {
	if id == "" || s.ignoredLocked(login, name) {
		return
	}
	today := s.todayLocked(s.now())
	for _, scope := range []struct {
		ps     people
		capped *bool
	}{{s.people, &s.peopleCapped}, {today.People, &today.Capped}} {
		p, capped := scope.ps.get(id, name)
		// The login only when it isn't the display name in lower case
		// (almost always it is): saves memory and file size.
		if login != "" && login != strings.ToLower(name) {
			p.Login = login
		} else if login != "" {
			p.Login = ""
		}
		fn(p)
		if capped {
			*scope.capped = true
		}
	}
}

// ChatMessage counts a chat message (the app leaves out the broadcaster's
// own, darkbot's replies among them).
func (s *Stats) ChatMessage(id, login, name string) {
	s.mu.Lock()
	if id == "" || s.ignoredLocked(login, name) {
		s.mu.Unlock()
		return
	}
	now := s.now()
	s.todayLocked(now).Hours[now.Hour()]++
	s.session.ChatMessages++
	s.total.ChatMessages++
	s.touchLocked(id, login, name, func(p *Person) { p.Messages++ })
	s.scheduleSave()
	cb := s.onChange
	s.mu.Unlock()
	if cb != nil {
		cb()
	}
}

// Insights returns the statistics view's numbers for the last days days
// (one of Ranges; anything else means the shortest).
func (s *Stats) Insights(days int) Insights {
	if !slices.Contains(Ranges, days) {
		days = Ranges[0]
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.now()
	s.rollLocked(now)
	out := Insights{Since: s.since, Days: days, Bots: slices.Clone(KnownBots)}

	out.Session = s.boardLocked(s.people, s.peopleCapped)
	out.Session.Messages = s.session.ChatMessages
	out.Session.Subs = s.session.Subs + s.session.GiftedSubs
	out.Session.Bits = s.session.Bits

	byDate := map[string]*day{}
	for _, d := range s.closed {
		byDate[d.Date] = d
	}
	for _, d := range s.days {
		byDate[d.Date] = d
	}
	merged := people{}
	var r Board
	for _, date := range lastDates(now, days) {
		sum := DaySummary{Date: date}
		if d := byDate[date]; d != nil {
			sum.Hours, sum.Messages, sum.Capped = d.Hours, d.messages(), d.Capped
			sum.Chatters = s.chattersLocked(d.People)
			if d.Trimmed {
				sum.Chatters = d.Chatters
			}
			r.Messages += sum.Messages
			r.Subs += d.Subs
			r.Bits += d.Bits
			// Over several days a trimmed day's people are incomplete.
			r.Capped = r.Capped || d.Capped || d.Trimmed
			for k, p := range d.People {
				w := merged[k]
				if w == nil {
					w = &Person{}
					merged[k] = w
				}
				// Days go oldest first: the newest name wins.
				w.Name, w.Login = p.Name, cmp.Or(p.Login, w.Login)
				w.Messages += p.Messages
				w.Subs += p.Subs
				w.Bits += p.Bits
				w.Raid += p.Raid
			}
		}
		out.DaysList = append(out.DaysList, sum)
	}
	b := s.boardLocked(merged, r.Capped)
	b.Messages, b.Subs, b.Bits = r.Messages, r.Subs, r.Bits
	out.Range = b
	return out
}

// boardLocked ranks a scope, leaving out ignored accounts.
func (s *Stats) boardLocked(ps people, capped bool) Board {
	b := Board{Capped: capped, Chatters: s.chattersLocked(ps)}
	ranked := func(val func(*Person) int) []Entry {
		m := map[string]int{}
		for _, p := range ps {
			if v := val(p); v > 0 && !s.ignoredLocked(p.Login, p.Name) {
				m[p.Name] += v
			}
		}
		return top(m)
	}
	b.TopChatters = ranked(func(p *Person) int { return p.Messages })
	b.TopSubs = ranked(func(p *Person) int { return p.Subs })
	b.TopBits = ranked(func(p *Person) int { return p.Bits })
	b.TopRaiders = ranked(func(p *Person) int { return p.Raid })
	return b
}

func (s *Stats) chattersLocked(ps people) int {
	n := 0
	for _, p := range ps {
		if p.Messages > 0 && !s.ignoredLocked(p.Login, p.Name) {
			n++
		}
	}
	return n
}

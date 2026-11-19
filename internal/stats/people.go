package stats

import (
	"cmp"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/miwidot/darkbot/internal/i18n"
)

// People: who chatted, subbed, cheered and raided, for the statistics
// view. Two scopes: this session (the stream) and the last HistoryDays
// days, kept per local calendar day with chat messages per hour for the
// activity graph. Both are capped so stats.json stays small however busy
// the chat gets.

const (
	// HistoryDays is how many days (today included) the history keeps.
	HistoryDays = 7
	// MaxPeople caps the people kept per day and per session. When a new
	// one would exceed it, the least active one is dropped (see evict), so
	// the top lists stay right; only the chatter count becomes a floor.
	MaxPeople = 1000
	// dayLayout is the key of a calendar day (local time).
	dayLayout   = "2006-01-02"
	hoursPerDay = 24
)

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
	Name     string `json:"name"`            // display name (the latest seen)
	Login    string `json:"login,omitempty"` // only known from chat
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
	// Capped: someone was dropped, so the chatter count is a floor.
	Capped bool `json:"capped,omitempty"`
}

func (d *day) messages() int {
	n := 0
	for _, h := range d.Hours {
		n += h
	}
	return n
}

// Board is one scope's numbers and top lists.
type Board struct {
	Messages int `json:"messages"`
	// Chatters: different people who wrote at least once.
	Chatters int `json:"chatters"`
	Subs     int `json:"subs"`
	Bits     int `json:"bits"`
	// Capped: the chatter count is a floor (MaxPeople reached).
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
	Week    Board     `json:"week"`
	// Days: the last HistoryDays days, oldest first, today last; days
	// without anything are there too (zeros).
	Days []DaySummary `json:"days"`
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
func (s *Stats) ignoredLocked(login, name string) bool {
	return s.ignored[strings.ToLower(login)] || s.ignored[strings.ToLower(name)]
}

// todayLocked returns today's day of the history, starting a new one and
// dropping days that fell out of it. Call with mu held.
func (s *Stats) todayLocked(now time.Time) *day {
	date := now.Format(dayLayout)
	if n := len(s.days); n > 0 && s.days[n-1].Date == date {
		return s.days[n-1]
	}
	s.days = append(s.days, &day{Date: date, People: people{}})
	s.pruneDaysLocked(now)
	return s.days[len(s.days)-1]
}

// pruneDaysLocked keeps the days of the last HistoryDays (and drops days
// "from the future" after a clock change, except today).
func (s *Stats) pruneDaysLocked(now time.Time) {
	keep := map[string]bool{}
	for _, d := range lastDates(now) {
		keep[d] = true
	}
	s.days = slices.DeleteFunc(s.days, func(d *day) bool { return !keep[d.Date] })
}

// lastDates: the dates of the last HistoryDays days, oldest first. It steps
// from local noon so a daylight saving change can't skip or repeat a day.
func lastDates(now time.Time) []string {
	noon := time.Date(now.Year(), now.Month(), now.Day(), 12, 0, 0, 0, now.Location())
	out := make([]string, HistoryDays)
	for i := range HistoryDays {
		out[i] = noon.AddDate(0, 0, i-(HistoryDays-1)).Format(dayLayout)
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
		if login != "" {
			p.Login = login
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

// Insights returns the statistics view's numbers.
func (s *Stats) Insights() Insights {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.now()
	s.pruneDaysLocked(now)
	out := Insights{Since: s.since, Bots: slices.Clone(KnownBots)}

	out.Session = s.boardLocked(s.people, s.peopleCapped)
	out.Session.Messages = s.session.ChatMessages
	out.Session.Subs = s.session.Subs + s.session.GiftedSubs
	out.Session.Bits = s.session.Bits

	byDate := map[string]*day{}
	for _, d := range s.days {
		byDate[d.Date] = d
	}
	week := people{}
	weekCapped := false
	for _, date := range lastDates(now) {
		sum := DaySummary{Date: date}
		if d := byDate[date]; d != nil {
			sum.Hours, sum.Messages, sum.Capped = d.Hours, d.messages(), d.Capped
			sum.Chatters = s.chattersLocked(d.People)
			out.Week.Messages += sum.Messages
			out.Week.Subs += d.Subs
			out.Week.Bits += d.Bits
			weekCapped = weekCapped || d.Capped
			for k, p := range d.People {
				w := week[k]
				if w == nil {
					w = &Person{}
					week[k] = w
				}
				// Days go oldest first: the newest name wins.
				w.Name, w.Login = p.Name, cmp.Or(p.Login, w.Login)
				w.Messages += p.Messages
				w.Subs += p.Subs
				w.Bits += p.Bits
				w.Raid += p.Raid
			}
		}
		out.Days = append(out.Days, sum)
	}
	b := s.boardLocked(week, weekCapped)
	b.Messages, b.Subs, b.Bits = out.Week.Messages, out.Week.Subs, out.Week.Bits
	out.Week = b
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

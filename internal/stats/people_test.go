package stats

import (
	"fmt"
	"path/filepath"
	"testing"
	"time"
)

// clock is a settable time for the tests.
type clock struct{ t time.Time }

func (c *clock) now() time.Time { return c.t }

func openAt(t *testing.T, path string, at time.Time) (*Stats, *clock) {
	t.Helper()
	c := &clock{at}
	s := Open(path, nopLog{})
	s.now = c.now
	return s, c
}

func TestPeopleRanking(t *testing.T) {
	start := time.Date(2026, 11, 19, 20, 15, 0, 0, time.Local)
	s, _ := openAt(t, filepath.Join(t.TempDir(), "stats.json"), start)
	for range 3 {
		s.ChatMessage("1", "anna", "Anna")
	}
	s.ChatMessage("2", "ben", "Ben")
	s.ChatMessage("9", "nightbot", "Nightbot") // known bot
	s.ChatMessage("", "ghost", "Ghost")        // no ID: not counted
	s.Sub("2", "Ben")
	s.Gift("1", "Anna", 5)
	s.Gift("", "", 3) // anonymous: total only
	s.Cheer("2", "Ben", 100)
	s.Raid("7", "Carla", 42)

	in := s.Insights()
	b := in.Session
	if b.Messages != 4 || b.Chatters != 2 || b.Subs != 9 || b.Bits != 100 {
		t.Fatalf("session board %+v", b)
	}
	if b.TopChatters[0] != (Entry{"Anna", 3}) || len(b.TopChatters) != 2 {
		t.Fatalf("top chatters %+v", b.TopChatters)
	}
	if b.TopSubs[0] != (Entry{"Anna", 5}) || b.TopSubs[1] != (Entry{"Ben", 1}) {
		t.Fatalf("top subs %+v", b.TopSubs)
	}
	if b.TopBits[0] != (Entry{"Ben", 100}) || b.TopRaiders[0] != (Entry{"Carla", 42}) {
		t.Fatalf("bits %+v raiders %+v", b.TopBits, b.TopRaiders)
	}
	if in.Week.Messages != 4 || in.Week.Subs != 9 || in.Week.Bits != 100 || in.Week.TopChatters[0] != (Entry{"Anna", 3}) {
		t.Fatalf("week %+v", in.Week)
	}
	if len(in.Days) != HistoryDays || in.Days[HistoryDays-1].Date != "2026-11-19" || in.Days[0].Date != "2026-11-13" {
		t.Fatalf("days %+v", in.Days)
	}
	if today := in.Days[HistoryDays-1]; today.Hours[20] != 4 || today.Messages != 4 || today.Chatters != 2 {
		t.Fatalf("today %+v", today)
	}
}

func TestIgnoredAccounts(t *testing.T) {
	s, _ := openAt(t, filepath.Join(t.TempDir(), "stats.json"), time.Date(2026, 11, 19, 10, 0, 0, 0, time.Local))
	s.ChatMessage("1", "mybot", "MyBot")
	s.ChatMessage("2", "anna", "Anna")
	s.SetIgnored([]string{"MyBot"})
	// Counted before: hidden from now on, and no longer counted.
	s.ChatMessage("1", "mybot", "MyBot")
	s.Cheer("1", "MyBot", 50) // events only know the display name
	in := s.Insights()
	if in.Session.Chatters != 1 || len(in.Session.TopChatters) != 1 || len(in.Session.TopBits) != 0 {
		t.Fatalf("session %+v", in.Session)
	}
	s.SetIgnored(nil)
	if got := s.Insights().Session.TopChatters; len(got) != 2 || got[1] != (Entry{"MyBot", 1}) {
		t.Fatalf("after un-ignoring %+v", got)
	}
}

func TestHistoryRollsAndPersists(t *testing.T) {
	p := filepath.Join(t.TempDir(), "stats.json")
	start := time.Date(2026, 11, 10, 12, 0, 0, 0, time.Local)
	s, c := openAt(t, p, start)
	for i := range 10 { // one message a day for ten days
		c.t = start.AddDate(0, 0, i)
		s.ChatMessage("1", "anna", fmt.Sprintf("Anna%d", i))
	}
	in := s.Insights()
	if in.Week.Messages != HistoryDays || in.Week.TopChatters[0] != (Entry{"Anna9", HistoryDays}) {
		t.Fatalf("week %+v", in.Week)
	}
	if in.Session.Messages != 10 {
		t.Fatalf("session %d", in.Session.Messages)
	}
	s.Flush()

	again, c2 := openAt(t, p, c.t)
	if got := again.Insights(); got.Week.Messages != HistoryDays || got.Session.TopChatters[0].Count != 10 {
		t.Fatalf("reloaded %+v", got)
	}
	// Two days later the two oldest days are gone.
	c2.t = c.t.AddDate(0, 0, 2)
	if got := again.Insights().Week.Messages; got != HistoryDays-2 {
		t.Fatalf("after two days %d", got)
	}
	again.ResetSession()
	if got := again.Insights(); got.Session.Messages != 0 || len(got.Session.TopChatters) != 0 || got.Week.Messages != HistoryDays-2 {
		t.Fatalf("after reset %+v", got)
	}
}

func TestPeopleCap(t *testing.T) {
	s, _ := openAt(t, filepath.Join(t.TempDir(), "stats.json"), time.Date(2026, 11, 19, 10, 0, 0, 0, time.Local))
	for range 5 {
		s.ChatMessage("top", "top", "Top")
	}
	for i := range MaxPeople + 10 {
		id := fmt.Sprint(i)
		s.ChatMessage(id, "u"+id, "U"+id)
	}
	in := s.Insights()
	if !in.Session.Capped || in.Session.Chatters != MaxPeople || in.Session.TopChatters[0] != (Entry{"Top", 5}) {
		t.Fatalf("capped %v chatters %d top %+v", in.Session.Capped, in.Session.Chatters, in.Session.TopChatters[0])
	}
	if in.Session.Messages != MaxPeople+15 {
		t.Fatalf("messages %d", in.Session.Messages)
	}
	if d := in.Days[HistoryDays-1]; !d.Capped || d.Chatters != MaxPeople {
		t.Fatalf("day %+v", d)
	}
}

func TestNormalizeLogins(t *testing.T) {
	got, err := NormalizeLogins([]string{" @MyBot ", "mybot", "Nightbot", "", "zed_1"})
	if err != nil || fmt.Sprint(got) != "[mybot zed_1]" {
		t.Fatalf("got %v %v", got, err)
	}
	if _, err := NormalizeLogins([]string{"no spaces"}); err == nil {
		t.Fatal("a name with a space must be refused")
	}
	many := make([]string, MaxIgnored+1)
	for i := range many {
		many[i] = fmt.Sprint("b", i)
	}
	if _, err := NormalizeLogins(many); err == nil {
		t.Fatal("too many names must be refused")
	}
}

func TestLastDatesAcrossDST(t *testing.T) {
	loc, err := time.LoadLocation("Europe/Berlin")
	if err != nil {
		t.Skip("no time zone data")
	}
	// The clocks went back on 2026-10-25 in Berlin.
	got := lastDates(time.Date(2026, 10, 27, 0, 30, 0, 0, loc))
	want := "[2026-10-21 2026-10-22 2026-10-23 2026-10-24 2026-10-25 2026-10-26 2026-10-27]"
	if fmt.Sprint(got) != want {
		t.Fatalf("got %v", got)
	}
}

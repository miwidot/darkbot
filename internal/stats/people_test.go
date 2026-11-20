package stats

import (
	"fmt"
	"os"
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

	in := s.Insights(7)
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
	if in.Range.Messages != 4 || in.Range.Subs != 9 || in.Range.Bits != 100 || in.Range.TopChatters[0] != (Entry{"Anna", 3}) {
		t.Fatalf("range %+v", in.Range)
	}
	if len(in.DaysList) != 7 || in.DaysList[6].Date != "2026-11-19" || in.DaysList[0].Date != "2026-11-13" {
		t.Fatalf("days %+v", in.DaysList)
	}
	if got := s.Insights(90).DaysList; len(got) != 90 || got[0].Date != "2026-08-22" {
		t.Fatalf("90 days from %v (%d)", got[0].Date, len(got))
	}
	if s.Insights(12).Days != 7 {
		t.Fatal("an unknown range must fall back to 7 days")
	}
	if today := in.DaysList[6]; today.Hours[20] != 4 || today.Messages != 4 || today.Chatters != 2 {
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
	in := s.Insights(7)
	if in.Session.Chatters != 1 || len(in.Session.TopChatters) != 1 || len(in.Session.TopBits) != 0 {
		t.Fatalf("session %+v", in.Session)
	}
	s.SetIgnored(nil)
	if got := s.Insights(7).Session.TopChatters; len(got) != 2 || got[1] != (Entry{"MyBot", 1}) {
		t.Fatalf("after un-ignoring %+v", got)
	}
}

func TestHistoryRollsAndPersists(t *testing.T) {
	p := filepath.Join(t.TempDir(), "stats.json")
	start := time.Date(2026, 11, 10, 12, 0, 0, 0, time.Local)
	s, c := openAt(t, p, start)
	for range 3 {
		s.ChatMessage("1", "anna", "Anna0")
	}
	s.Flush()
	if _, err := os.Stat(HistoryPath(p)); !os.IsNotExist(err) {
		t.Fatalf("no day has ended yet, the history must not be written (%v)", err)
	}
	for i := 1; i < 10; i++ { // one message a day for nine more days
		c.t = start.AddDate(0, 0, i)
		s.ChatMessage("1", "anna", fmt.Sprintf("Anna%d", i))
	}
	in := s.Insights(7)
	if in.Range.Messages != 7 || in.Range.TopChatters[0] != (Entry{"Anna9", 7}) {
		t.Fatalf("7 days %+v", in.Range)
	}
	if got := s.Insights(30).Range; got.Messages != 12 || got.TopChatters[0].Count != 12 {
		t.Fatalf("30 days %+v", got)
	}
	if in.Session.Messages != 12 {
		t.Fatalf("session %d", in.Session.Messages)
	}
	s.Flush()
	if _, err := os.Stat(HistoryPath(p)); err != nil {
		t.Fatalf("days have ended, the history must be written: %v", err)
	}

	again, c2 := openAt(t, p, c.t)
	if got := again.Insights(30); got.Range.Messages != 12 || got.Session.TopChatters[0].Count != 12 {
		t.Fatalf("reloaded %+v", got)
	}
	// 85 days later only the last five days are within 90 days.
	c2.t = c.t.AddDate(0, 0, 85)
	if got := again.Insights(90).Range.Messages; got != 5 {
		t.Fatalf("after 85 days %d", got)
	}
	again.ResetSession()
	if got := again.Insights(90); got.Session.Messages != 0 || len(got.Session.TopChatters) != 0 || got.Range.Messages != 5 {
		t.Fatalf("after reset %+v", got)
	}
}

func TestOldDaysAreTrimmed(t *testing.T) {
	p := filepath.Join(t.TempDir(), "stats.json")
	start := time.Date(2026, 11, 1, 20, 0, 0, 0, time.Local)
	s, c := openAt(t, p, start)
	for i := range OldDayPeople + 50 {
		id := fmt.Sprint(i)
		for range i%7 + 1 {
			s.ChatMessage(id, "u"+id, "U"+id)
		}
	}
	c.t = start.AddDate(0, 0, 3) // still a full day
	d := s.Insights(7).DaysList[3]
	if d.Chatters != OldDayPeople+50 || d.Capped {
		t.Fatalf("recent day %+v", d)
	}
	c.t = start.AddDate(0, 0, FullDays+1)
	in := s.Insights(30)
	d = in.DaysList[len(in.DaysList)-1-(FullDays+1)]
	if d.Date != "2026-11-01" || d.Chatters != OldDayPeople+50 || d.Capped {
		t.Fatalf("old day %+v", d)
	}
	if !in.Range.Capped || in.Range.Chatters != OldDayPeople || in.Range.TopChatters[0].Count != 7 {
		t.Fatalf("range %+v", in.Range)
	}
	s.Flush()
	again, c2 := openAt(t, p, c.t)
	c2.t = c.t
	if got := again.Insights(30).DaysList[len(in.DaysList)-1-(FullDays+1)]; got.Chatters != OldDayPeople+50 {
		t.Fatalf("reloaded old day %+v", got)
	}
}

func TestDaysOfOldFilesMoveToHistory(t *testing.T) {
	// v0.7.2 kept all seven days in stats.json.
	p := filepath.Join(t.TempDir(), "stats.json")
	old := `{"since":"2026-11-01T00:00:00Z","total":{},"days":[` +
		`{"date":"2026-11-18","hours":[0,0,0,0,0,0,0,0,0,0,0,0,0,0,0,0,0,0,0,0,5,0,0,0],"people":{"1":{"name":"Anna","login":"anna","messages":5}}},` +
		`{"date":"2026-11-19","hours":[0,0,0,0,0,0,0,0,0,0,2,0,0,0,0,0,0,0,0,0,0,0,0,0],"people":{"2":{"name":"Ben","login":"ben","messages":2}}},` +
		`{"date":"bogus"},null]}`
	if err := os.WriteFile(p, []byte(old), 0o600); err != nil {
		t.Fatal(err)
	}
	s, _ := openAt(t, p, time.Date(2026, 11, 19, 12, 0, 0, 0, time.Local))
	in := s.Insights(7)
	if in.Range.Messages != 7 || in.DaysList[5].Messages != 5 || in.DaysList[6].Messages != 2 {
		t.Fatalf("migrated %+v", in.DaysList)
	}
	s.Flush()
	again, _ := openAt(t, p, time.Date(2026, 11, 19, 12, 0, 0, 0, time.Local))
	if got := again.Insights(7).Range.Messages; got != 7 {
		t.Fatalf("after save and reload %d", got)
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
	in := s.Insights(7)
	if !in.Session.Capped || in.Session.Chatters != MaxPeople || in.Session.TopChatters[0] != (Entry{"Top", 5}) {
		t.Fatalf("capped %v chatters %d top %+v", in.Session.Capped, in.Session.Chatters, in.Session.TopChatters[0])
	}
	if in.Session.Messages != MaxPeople+15 {
		t.Fatalf("messages %d", in.Session.Messages)
	}
	if d := in.DaysList[6]; !d.Capped || d.Chatters != MaxPeople {
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
	got := lastDates(time.Date(2026, 10, 27, 0, 30, 0, 0, loc), 7)
	want := "[2026-10-21 2026-10-22 2026-10-23 2026-10-24 2026-10-25 2026-10-26 2026-10-27]"
	if fmt.Sprint(got) != want {
		t.Fatalf("got %v", got)
	}
}

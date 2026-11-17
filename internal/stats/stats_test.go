package stats

import (
	"os"
	"path/filepath"
	"sync"
	"testing"
)

type nopLog struct{}

func (nopLog) Warnf(string, ...any) {}

func TestCountsAndRanking(t *testing.T) {
	s := Open(filepath.Join(t.TempDir(), "stats.json"), nopLog{})
	s.Redemption("Anna", "Airhorn", true)
	s.Played()
	s.Redemption("Anna", "Airhorn", false)
	s.Redemption("Ben", "Bruh", true)
	s.Failed()
	s.Command()
	s.SongAdded("Ben")
	s.SongAdded("Ben")
	s.SongRejected()
	s.Skip()

	got := s.Snapshot()
	want := Counts{Redemptions: 3, Played: 1, Refused: 2, Commands: 1, SongsAdded: 2, SongsRejected: 1, Skips: 1}
	if got.Session != want || got.Total != want {
		t.Fatalf("session %+v total %+v, want %+v", got.Session, got.Total, want)
	}
	if got.Viewers != 2 {
		t.Fatalf("viewers %d", got.Viewers)
	}
	if len(got.TopSounds) != 2 || got.TopSounds[0] != (Entry{"Airhorn", 2}) {
		t.Fatalf("top sounds %+v", got.TopSounds)
	}
	if got.TopViewers[0] != (Entry{"Ben", 3}) || got.TopViewers[1] != (Entry{"Anna", 2}) {
		t.Fatalf("top viewers %+v", got.TopViewers)
	}
}

func TestTopIsLimitedAndStable(t *testing.T) {
	m := map[string]int{"e": 1, "d": 1, "c": 1, "b": 1, "a": 1, "z": 5, "": 9}
	got := top(m)
	if len(got) != TopN || got[0].Name != "z" || got[1].Name != "a" || got[4].Name != "d" {
		t.Fatalf("got %+v", got)
	}
}

func TestTotalsPersistSessionResets(t *testing.T) {
	p := filepath.Join(t.TempDir(), "stats.json")
	s := Open(p, nopLog{})
	s.Redemption("Anna", "Airhorn", true)
	s.ResetSession()
	if snap := s.Snapshot(); snap.Session != (Counts{}) || snap.Total.Redemptions != 1 || len(snap.TopSounds) != 0 {
		t.Fatalf("after reset %+v", snap)
	}
	s.Flush()

	again := Open(p, nopLog{})
	snap := again.Snapshot()
	if snap.Total.Redemptions != 1 || snap.Session.Redemptions != 0 {
		t.Fatalf("reloaded %+v", snap)
	}
	if !snap.TotalSince.Equal(s.Snapshot().TotalSince) {
		t.Fatal("total start date must survive restarts")
	}
}

func TestCorruptFileStartsAtZero(t *testing.T) {
	p := filepath.Join(t.TempDir(), "stats.json")
	if err := os.WriteFile(p, []byte("{nope"), 0o600); err != nil {
		t.Fatal(err)
	}
	if snap := Open(p, nopLog{}).Snapshot(); snap.Total != (Counts{}) {
		t.Fatalf("got %+v", snap.Total)
	}
}

func TestConcurrentUpdates(t *testing.T) {
	s := Open(filepath.Join(t.TempDir(), "stats.json"), nopLog{})
	var wg sync.WaitGroup
	for range 50 {
		wg.Go(func() { s.Redemption("x", "y", true); s.Command(); _ = s.Snapshot() })
	}
	wg.Wait()
	s.Flush()
	if snap := s.Snapshot(); snap.Session.Redemptions != 50 || snap.Session.Commands != 50 {
		t.Fatalf("got %+v", snap.Session)
	}
}

package stats

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"
)

// TestWorstCaseSize measures memory and file sizes with every day of the
// history full (up to MaxPeople different chatters on each of
// MaxHistoryDays days, one stream a day). It is a measurement, not a check that runs every time:
// DARKBOT_MEASURE=1 go test -run WorstCaseSize -v ./internal/stats/
func TestWorstCaseSize(t *testing.T) {
	if os.Getenv("DARKBOT_MEASURE") == "" {
		t.Skip("set DARKBOT_MEASURE=1 to measure")
	}
	for _, perDay := range []int{50, 300, MaxPeople} {
		p := filepath.Join(t.TempDir(), "stats.json")
		heap := func() uint64 {
			runtime.GC()
			var m runtime.MemStats
			runtime.ReadMemStats(&m)
			return m.HeapAlloc
		}
		before := heap()
		start := time.Date(2026, 8, 1, 20, 0, 0, 0, time.Local)
		s, c := openAt(t, p, start)
		for d := range MaxHistoryDays {
			c.t = start.AddDate(0, 0, d)
			s.ResetSession() // a stream a day, as the stream start does
			for i := range perDay {
				// Different people every day: the worst case for the merge.
				id := fmt.Sprint(d*100000 + i)
				s.ChatMessage(id, "viewer_"+id, "Viewer_"+id)
			}
		}
		_ = s.Insights(MaxHistoryDays) // rolls the last day, like the view
		s.Flush()
		used := heap() - before
		t0 := time.Now()
		in := s.Insights(MaxHistoryDays)
		took := time.Since(t0)
		st, _ := os.Stat(p)
		hi, _ := os.Stat(HistoryPath(p))
		t.Logf("%4d chatters/day x %d days: heap %.1f MB, stats.json %.0f KB, history %.0f KB, Insights(90) %v (%d msgs)",
			perDay, MaxHistoryDays, float64(used)/1e6, float64(st.Size())/1e3, float64(hi.Size())/1e3, took, in.Range.Messages)
		runtime.KeepAlive(s)
	}
}

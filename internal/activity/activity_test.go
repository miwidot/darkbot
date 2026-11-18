package activity

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"
)

type nopLog struct{ warned int }

func (n *nopLog) Warnf(string, ...any) { n.warned++ }

type item struct {
	Kind string    `json:"kind"`
	At   time.Time `json:"at"`
}

func kinds(t *testing.T, l *Log) []string {
	t.Helper()
	var out []string
	for _, r := range l.List() {
		var it item
		if err := json.Unmarshal(r, &it); err != nil {
			t.Fatal(err)
		}
		out = append(out, it.Kind)
	}
	return out
}

func TestFeedSurvivesRestart(t *testing.T) {
	path := filepath.Join(t.TempDir(), "activity.json")
	l := Open(path, &nopLog{})
	base := time.Date(2026, 11, 18, 20, 0, 0, 0, time.UTC)
	for i, k := range []string{"follow", "sub", "raid"} {
		if err := l.Add(item{Kind: k, At: base.Add(time.Duration(i) * time.Minute)}); err != nil {
			t.Fatal(err)
		}
	}
	// A follow found later that happened before the others goes below them.
	if err := l.Add(item{Kind: "missed", At: base.Add(-time.Hour)}); err != nil {
		t.Fatal(err)
	}
	l.Flush()

	got := kinds(t, Open(path, &nopLog{}))
	want := []string{"raid", "sub", "follow", "missed"}
	if len(got) != len(want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("got %v, want %v", got, want)
		}
	}
}

func TestOldestAreDropped(t *testing.T) {
	l := Open(filepath.Join(t.TempDir(), "activity.json"), &nopLog{})
	base := time.Now()
	for i := range MaxEntries + 5 {
		if err := l.Add(item{Kind: "e", At: base.Add(time.Duration(i) * time.Second)}); err != nil {
			t.Fatal(err)
		}
	}
	list := l.List()
	if len(list) != MaxEntries {
		t.Fatalf("kept %d, want %d", len(list), MaxEntries)
	}
	var newest item
	_ = json.Unmarshal(list[0], &newest)
	if !newest.At.Equal(base.Add(time.Duration(MaxEntries+4) * time.Second)) {
		t.Fatalf("newest entry is %v", newest.At)
	}
	l.Flush()
}

func TestEntryNeedsTime(t *testing.T) {
	l := Open(filepath.Join(t.TempDir(), "activity.json"), &nopLog{})
	if err := l.Add(struct{ Kind string }{"x"}); err == nil {
		t.Fatal("entry without time was kept")
	}
}

func TestClear(t *testing.T) {
	path := filepath.Join(t.TempDir(), "activity.json")
	l := Open(path, &nopLog{})
	_ = l.Add(item{Kind: "follow", At: time.Now()})
	if err := l.Clear(); err != nil {
		t.Fatal(err)
	}
	l.Flush()
	if n := len(Open(path, &nopLog{}).List()); n != 0 {
		t.Fatalf("%d entries after clear", n)
	}
}

func TestUnreadableFileStartsEmpty(t *testing.T) {
	path := filepath.Join(t.TempDir(), "activity.json")
	if err := os.WriteFile(path, []byte("{broken"), 0o600); err != nil {
		t.Fatal(err)
	}
	lg := &nopLog{}
	if n := len(Open(path, lg).List()); n != 0 || lg.warned != 1 {
		t.Fatalf("entries %d, warnings %d", n, lg.warned)
	}
}

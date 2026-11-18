package ytmusic

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"testing"
	"time"
)

// The testdata files are real Pear Desktop 3.12.1 answers (trimmed of
// tracking data and images):
//
//	search_songs.json       songs search for "eminem without me"
//	lookup_song_miss.json   songs search for the video ID dQw4w9WgXcQ (no match: it's a video)
//	lookup_all.json         full search for dQw4w9WgXcQ (top result is the video)
//	queue.json              a queue of five, the second one playing
//	song.json               the paused current song
func fixture(t *testing.T, name string) []byte {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatal(err)
	}
	return data
}

type memStore struct{ t *Token }

func (m *memStore) Load() (*Token, error) { return m.t, nil }
func (m *memStore) Save(t *Token) error   { m.t = t; return nil }
func (m *memStore) Delete() error         { m.t = nil; return nil }

const testToken = "test-token"

// fakePear serves the API on 127.0.0.1 like Pear Desktop.
type fakePear struct {
	t        *testing.T
	mu       sync.Mutex
	search   func(query, params string) []byte
	queue    []byte
	song     []byte
	added    []string
	onAdd    func(id string)
	nexts    int
	authCode int // answer to /auth/darkbot; 0 = 200
}

func newFake(t *testing.T) (*fakePear, *Client) {
	f := &fakePear{t: t, queue: []byte(`{"items":[]}`)}
	srv := httptest.NewServer(f)
	t.Cleanup(srv.Close)
	_, port, _ := net.SplitHostPort(srv.Listener.Addr().String())
	p, _ := strconv.Atoi(port)
	c, err := NewClient(p, &memStore{t: &Token{AccessToken: testToken}})
	if err != nil {
		t.Fatal(err)
	}
	c.addWait = time.Second
	return f, c
}

func (f *fakePear) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if !r.Close {
		f.t.Error("requests must not keep the connection open (Pear Desktop resets idle ones)")
	}
	if r.URL.Path == "/auth/"+clientID {
		if f.authCode != 0 {
			w.WriteHeader(f.authCode)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]string{"accessToken": "new-token"})
		return
	}
	if r.Header.Get("Authorization") != "Bearer "+testToken {
		w.WriteHeader(http.StatusUnauthorized)
		return
	}
	switch r.Method + " " + r.URL.Path {
	case "POST /api/v1/search":
		var body struct{ Query, Params string }
		_ = json.NewDecoder(r.Body).Decode(&body)
		_, _ = w.Write(f.search(body.Query, body.Params))
	case "GET /api/v1/queue":
		_, _ = w.Write(f.queue)
	case "POST /api/v1/queue":
		var body struct {
			VideoID        string `json:"videoId"`
			InsertPosition string `json:"insertPosition"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		if body.InsertPosition != "INSERT_AT_END" {
			f.t.Errorf("insert position %q", body.InsertPosition)
		}
		f.added = append(f.added, body.VideoID)
		if f.onAdd != nil {
			go f.onAdd(body.VideoID) // Pear adds after answering
		}
		w.WriteHeader(http.StatusNoContent)
	case "GET /api/v1/song":
		if f.song == nil {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		_, _ = w.Write(f.song)
	case "POST /api/v1/next":
		f.nexts++
		w.WriteHeader(http.StatusNoContent)
	default:
		w.WriteHeader(http.StatusNotFound)
	}
}

func (f *fakePear) setQueue(ids ...string) {
	type pv struct {
		VideoID string `json:"videoId"`
		Title   text   `json:"title"`
	}
	var items []map[string]pv
	for _, id := range ids {
		items = append(items, map[string]pv{"playlistPanelVideoRenderer": {VideoID: id, Title: text{Runs: []run{{Text: id}}}}})
	}
	data, _ := json.Marshal(map[string]any{"items": items})
	f.mu.Lock()
	f.queue = data
	f.mu.Unlock()
}

func TestParseLink(t *testing.T) {
	for _, tc := range []struct {
		in     string
		id     string
		isLink bool
		err    error
	}{
		{"https://music.youtube.com/watch?v=4D7u5KF7SP8&si=abc", "4D7u5KF7SP8", true, nil},
		{"https://www.youtube.com/watch?v=dQw4w9WgXcQ&list=RDdQw4w9WgXcQ", "dQw4w9WgXcQ", true, nil},
		{"https://m.youtube.com/watch?v=dQw4w9WgXcQ", "dQw4w9WgXcQ", true, nil},
		{"https://youtu.be/dQw4w9WgXcQ?si=x", "dQw4w9WgXcQ", true, nil},
		{"music.youtube.com/watch?v=4D7u5KF7SP8 please", "4D7u5KF7SP8", true, nil},
		{"https://music.youtube.com/playlist?list=PL123", "", true, ErrNotSongLink},
		{"https://www.youtube.com/@channel", "", true, ErrNotSongLink},
		{"https://youtu.be/short", "", true, ErrNotSongLink},
		{"https://www.youtube.com/watch?v=", "", true, ErrNotSongLink},
		{"https://open.spotify.com/track/4uLU6hMCjMI75M1A2tKUQC", "", false, nil},
		{"https://notyoutube.com/watch?v=dQw4w9WgXcQ", "", false, nil},
		{"daft punk get lucky", "", false, nil},
		{"", "", false, nil},
	} {
		id, ok, err := ParseLink(tc.in)
		if id != tc.id || ok != tc.isLink || !errors.Is(err, tc.err) {
			t.Errorf("ParseLink(%q) = %q, %v, %v; want %q, %v, %v", tc.in, id, ok, err, tc.id, tc.isLink, tc.err)
		}
	}
}

func TestSearchTakesTheTopSong(t *testing.T) {
	f, c := newFake(t)
	f.search = func(query, params string) []byte {
		if query != "eminem without me" || params != songsFilter {
			t.Errorf("search %q with %q", query, params)
		}
		return fixture(t, "search_songs.json")
	}
	tr, err := c.Search(context.Background(), "eminem without me")
	if err != nil {
		t.Fatal(err)
	}
	want := Track{VideoID: "tqxRidAWER8", Name: "Without Me", Artists: "Eminem", Duration: 4*time.Minute + 51*time.Second, Explicit: true}
	if *tr != want {
		t.Fatalf("got %+v, want %+v", *tr, want)
	}
	if tr.Title() != "Eminem – Without Me" {
		t.Fatalf("title %q", tr.Title())
	}
}

// A music video isn't in the songs search; Lookup then finds it in the full
// search (top result card).
func TestLookupFindsVideos(t *testing.T) {
	f, c := newFake(t)
	var calls []string
	f.search = func(query, params string) []byte {
		calls = append(calls, params)
		if params == songsFilter {
			return fixture(t, "lookup_song_miss.json")
		}
		return fixture(t, "lookup_all.json")
	}
	tr, err := c.Lookup(context.Background(), "dQw4w9WgXcQ")
	if err != nil {
		t.Fatal(err)
	}
	if tr.VideoID != "dQw4w9WgXcQ" || tr.Title() != "Rick Astley – Never Gonna Give You Up" || tr.Duration != 3*time.Minute+34*time.Second {
		t.Fatalf("got %+v", *tr)
	}
	if len(calls) != 2 {
		t.Fatalf("searches %v", calls)
	}
}

func TestLookupFindsSongsFirst(t *testing.T) {
	f, c := newFake(t)
	f.search = func(_, params string) []byte {
		if params != songsFilter {
			t.Error("a song found in the songs search needs no second search")
		}
		return fixture(t, "search_songs.json")
	}
	tr, err := c.Lookup(context.Background(), "ifi6mPWoXxY") // a later result
	if err != nil || tr.VideoID != "ifi6mPWoXxY" {
		t.Fatalf("got %+v, %v", tr, err)
	}
}

func TestLookupNotFound(t *testing.T) {
	f, c := newFake(t)
	f.search = func(string, string) []byte { return fixture(t, "lookup_song_miss.json") }
	if _, err := c.Lookup(context.Background(), "aaaaaaaaaaa"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("err %v", err)
	}
}

func TestQueue(t *testing.T) {
	f, c := newFake(t)
	f.queue = fixture(t, "queue.json")
	q, err := c.Queue(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if q.Current == nil || q.Current.Title() != "Eminem – Without Me" || !q.Current.Explicit {
		t.Fatalf("current %+v", q.Current)
	}
	if len(q.Next) != 3 || q.Next[2].Title() != "Daft Punk, Pharrell Williams & Nile Rodgers – Get Lucky (feat. Pharrell Williams and Nile Rodgers)" || q.Next[2].Duration != 6*time.Minute+10*time.Second {
		t.Fatalf("next %+v", q.Next)
	}
	if len(q.IDs) != 5 || q.IDs[0] != "dQw4w9WgXcQ" {
		t.Fatalf("ids %v", q.IDs)
	}
}

func TestQueueBeforeAnythingPlayed(t *testing.T) {
	f, c := newFake(t)
	f.setQueue("a", "b")
	q, err := c.Queue(context.Background())
	if err != nil || q.Current != nil || len(q.Next) != 2 {
		t.Fatalf("got %+v, %v", q, err)
	}
}

func TestAddWaitsUntilQueued(t *testing.T) {
	f, c := newFake(t)
	f.setQueue("x")
	f.onAdd = func(id string) {
		time.Sleep(2 * addPollInterval)
		f.setQueue("x", id)
	}
	if err := c.Add(context.Background(), "4D7u5KF7SP8"); err != nil {
		t.Fatal(err)
	}
	if len(f.added) != 1 || f.added[0] != "4D7u5KF7SP8" {
		t.Fatalf("added %v", f.added)
	}
}

// Pear Desktop answers 204 also for IDs it can't add; the song never shows
// up in the queue.
func TestAddReportsSilentFailure(t *testing.T) {
	_, c := newFake(t)
	if err := c.Add(context.Background(), "aaaaaaaaaaa"); !errors.Is(err, ErrNotAdded) {
		t.Fatalf("err %v", err)
	}
}

// A song already in the queue (Pear Desktop allows duplicates) counts as
// added only when a second copy shows up.
func TestAddCountsCopies(t *testing.T) {
	f, c := newFake(t)
	f.setQueue("same")
	if err := c.Add(context.Background(), "same"); !errors.Is(err, ErrNotAdded) {
		t.Fatalf("the existing copy must not confirm the add, err %v", err)
	}
	f.onAdd = func(id string) { f.setQueue("same", id) }
	if err := c.Add(context.Background(), "same"); err != nil {
		t.Fatal(err)
	}
}

func TestPlayingAndNext(t *testing.T) {
	f, c := newFake(t)
	ctx := context.Background()
	if on, err := c.Playing(ctx); err != nil || on {
		t.Fatalf("no song: %v %v", on, err)
	}
	f.song = fixture(t, "song.json") // paused
	if on, err := c.Playing(ctx); err != nil || on {
		t.Fatalf("paused: %v %v", on, err)
	}
	f.song = []byte(`{"videoId":"tqxRidAWER8","isPaused":false}`)
	if on, err := c.Playing(ctx); err != nil || !on {
		t.Fatalf("playing: %v %v", on, err)
	}
	if err := c.Next(ctx); err != nil || f.nexts != 1 {
		t.Fatalf("next: %v %d", err, f.nexts)
	}
}

func TestConnect(t *testing.T) {
	f, c := newFake(t)
	st := &memStore{}
	c.store = st
	c.token = nil
	if err := c.Connect(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !c.Linked() || st.t == nil || st.t.AccessToken != "new-token" || st.t.AuthorizedAt.IsZero() {
		t.Fatalf("stored %+v", st.t)
	}
	f.authCode = http.StatusForbidden // the streamer clicked Deny
	if err := c.Connect(context.Background()); !errors.Is(err, ErrDenied) {
		t.Fatalf("err %v", err)
	}
	if err := c.Disconnect(); err != nil || c.Linked() || st.t != nil {
		t.Fatal("disconnect must forget the token")
	}
	if _, err := c.Queue(context.Background()); !errors.Is(err, ErrNotConnected) {
		t.Fatalf("err %v", err)
	}
}

func TestRejectedToken(t *testing.T) {
	_, c := newFake(t)
	c.token = &Token{AccessToken: "old"}
	_, err := c.Queue(context.Background())
	if !errors.Is(err, ErrNotAuthorized) {
		t.Fatalf("err %v", err)
	}
	if h := c.Health(func(err error) string { return err.Error() }); h.LastError == "" {
		t.Fatal("the dashboard must show the problem")
	}
}

func TestNotRunning(t *testing.T) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	_, port, _ := net.SplitHostPort(l.Addr().String())
	_ = l.Close() // nothing listens there now
	p, _ := strconv.Atoi(port)
	c, err := NewClient(p, &memStore{t: &Token{AccessToken: testToken}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.Queue(context.Background()); !errors.Is(err, ErrNotRunning) {
		t.Fatalf("err %v", err)
	}
	if err := c.Connect(context.Background()); !errors.Is(err, ErrNotRunning) {
		t.Fatalf("connect err %v", err)
	}
}

func TestParseDuration(t *testing.T) {
	for in, want := range map[string]time.Duration{
		"3:45": 3*time.Minute + 45*time.Second, "1:02:03": time.Hour + 2*time.Minute + 3*time.Second,
		"2002": 0, "1.8bn views": 0, "": 0,
	} {
		if got := parseDuration(in); got != want {
			t.Errorf("parseDuration(%q) = %v, want %v", in, got, want)
		}
	}
}

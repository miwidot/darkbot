// Package ytmusic queues song requests on YouTube Music through Pear Desktop
// (https://github.com/pear-devs/pear-desktop, formerly th-ch/youtube-music),
// the open-source YouTube Music desktop app.
//
// YouTube Music has no public API for playback or the queue. Pear Desktop
// ships an "API Server" plugin (off by default) that does: search, add to
// queue, skip and what plays now. darkbot only connects to it as a client on
// 127.0.0.1, so darkbot itself still opens no port.
//
// The API is documented by the plugin's source (src/plugins/api-server) and
// served as Swagger at /swagger by the running app. Responses of search and
// queue are YouTube Music's own internal JSON, passed through unchanged;
// the parsing below follows what Pear Desktop 3.12 returns (see testdata).
package ytmusic

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/miwidot/darkbot/internal/config"
	"github.com/miwidot/darkbot/internal/i18n"
)

const (
	// host: darkbot only talks to Pear Desktop on this computer, so the
	// access token never crosses the network unencrypted.
	host = "127.0.0.1"
	// clientID names darkbot in Pear Desktop's list of allowed clients.
	clientID  = "darkbot"
	apiPrefix = "/api/v1"
	// songsFilter is the search filter YouTube Music's web app sends for
	// its "Songs" tab: only songs, no videos, albums or artists.
	songsFilter = "EgWKAQIIAWoKEAkQBRAKEAMQBA=="
	// maxBody caps a response; a search answer is about 250 KB.
	maxBody = 8 << 20
	// maxErrorBody: enough of an error answer for the log.
	maxErrorBody = 512
	// Adding to the queue answers before the song is in it (and also for
	// IDs that don't exist), so darkbot watches the queue until the song
	// shows up. In tests it took under 0.5 s.
	addConfirmTimeout = 5 * time.Second
	addPollInterval   = 250 * time.Millisecond
	// explicitBadge marks songs with explicit lyrics.
	explicitBadge = "MUSIC_EXPLICIT_BADGE"
	// artistIDPrefix: channel (artist) IDs start with "UC"; albums "MPRE".
	artistIDPrefix = "UC"
	// separator between the parts of a subtitle ("Artist • Album • 3:45").
	separator = "•"
	maxPort   = 65535
)

// Errors a streamer can act on.
var (
	ErrNotConnected  = i18n.E("err.ytmOffline")
	ErrNotRunning    = i18n.E("err.ytmNotRunning")
	ErrNotAuthorized = i18n.E("err.ytmNotAuthorized")
	ErrDenied        = i18n.E("err.ytmDenied")
	ErrNotFound      = i18n.E("err.ytmNotFound")
	ErrNotSongLink   = i18n.E("err.ytmNotSongLink")
	ErrNotAdded      = i18n.E("err.ytmNotAdded")
)

// Token is darkbot's access to Pear Desktop.
type Token struct {
	AccessToken  string    `json:"access_token"`
	AuthorizedAt time.Time `json:"authorized_at"`
}

// TokenStore persists the token.
type TokenStore interface {
	Load() (*Token, error) // nil, nil = none stored
	Save(*Token) error
	Delete() error
}

// Health is shown on the dashboard.
type Health struct {
	Linked       bool      `json:"linked"`
	AuthorizedAt time.Time `json:"authorizedAt"`
	LastOK       time.Time `json:"lastOk"` // last answer from Pear Desktop; zero = not since start
	LastError    string    `json:"lastError"`
}

// Client talks to Pear Desktop's API Server.
type Client struct {
	http  *http.Client
	store TokenStore

	mu       sync.Mutex
	port     int
	token    *Token
	lastOK   time.Time
	lastErr  error
	onChange func()

	addWait time.Duration // addConfirmTimeout; shorter in tests

	// addMu: one add at a time. Pear Desktop adds asynchronously, so two
	// adds in a row could land in either order, and the duplicate check
	// must see the previous add.
	addMu sync.Mutex
}

// NewClient loads a stored token. An invalid port means Pear Desktop's
// default.
func NewClient(port int, store TokenStore) (*Client, error) {
	t, err := store.Load()
	if err != nil {
		return nil, err
	}
	c := &Client{http: newHTTPClient(), store: store, token: t, addWait: addConfirmTimeout}
	c.SetPort(port)
	return c, nil
}

// newHTTPClient opens a fresh connection per request. Pear Desktop's
// server (Node.js) closes idle connections after 5 s; a request sent on such
// a connection just as it closes fails with "connection reset", and Go
// retries that only for GET. On 127.0.0.1 a new connection costs nothing.
func newHTTPClient() *http.Client {
	tr := http.DefaultTransport.(*http.Transport).Clone()
	tr.DisableKeepAlives = true
	return &http.Client{Transport: tr}
}

// SetPort changes the port of the API Server (Pear Desktop's setting).
func (c *Client) SetPort(port int) {
	if port <= 0 || port > maxPort {
		port = config.DefaultYTMusicPort
	}
	c.mu.Lock()
	c.port = port
	c.mu.Unlock()
}

// OnChange is called when the login or its health changes.
func (c *Client) OnChange(fn func()) {
	c.mu.Lock()
	c.onChange = fn
	c.mu.Unlock()
}

func (c *Client) changed() {
	c.mu.Lock()
	fn := c.onChange
	c.mu.Unlock()
	if fn != nil {
		fn()
	}
}

// Linked reports whether Pear Desktop allowed darkbot (it may still be
// closed right now).
func (c *Client) Linked() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.token != nil
}

// Health returns the state for the dashboard; localize words the last
// problem for the streamer (the details are in the log).
func (c *Client) Health(localize func(error) string) Health {
	c.mu.Lock()
	defer c.mu.Unlock()
	h := Health{Linked: c.token != nil, LastOK: c.lastOK}
	if c.lastErr != nil {
		h.LastError = localize(c.lastErr)
	}
	if c.token != nil {
		h.AuthorizedAt = c.token.AuthorizedAt
	}
	return h
}

func (c *Client) base() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return "http://" + net.JoinHostPort(host, strconv.Itoa(c.port))
}

// Connect asks Pear Desktop for access. Pear Desktop shows the streamer a
// dialog; this waits until they click Allow or Deny (or ctx ends).
func (c *Client) Connect(ctx context.Context) error {
	var out struct {
		AccessToken string `json:"accessToken"`
	}
	err := c.send(ctx, "", http.MethodPost, "/auth/"+clientID, nil, &out)
	var se *statusError
	if errors.As(err, &se) && se.status == http.StatusForbidden {
		return ErrDenied
	}
	if err != nil {
		return err
	}
	if out.AccessToken == "" {
		return fmt.Errorf("pear desktop: login answer without token")
	}
	t := &Token{AccessToken: out.AccessToken, AuthorizedAt: time.Now()}
	if err := c.store.Save(t); err != nil {
		return err
	}
	c.mu.Lock()
	c.token = t
	c.lastErr = nil
	c.mu.Unlock()
	c.changed()
	return nil
}

// Disconnect forgets the access. Pear Desktop keeps darkbot in its list of
// allowed clients until the streamer removes it there.
func (c *Client) Disconnect() error {
	c.mu.Lock()
	c.token = nil
	c.lastErr = nil
	c.mu.Unlock()
	err := c.store.Delete()
	c.changed()
	return err
}

func errText(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

type statusError struct {
	status int
	path   string
	body   string
}

func (e *statusError) Error() string {
	return fmt.Sprintf("pear desktop %s: status %d: %s", e.path, e.status, e.body)
}

// call sends one API request with darkbot's token.
func (c *Client) call(ctx context.Context, method, path string, body, out any) error {
	c.mu.Lock()
	t := c.token
	c.mu.Unlock()
	if t == nil {
		return ErrNotConnected
	}
	err := c.send(ctx, t.AccessToken, method, apiPrefix+path, body, out)
	var se *statusError
	if errors.As(err, &se) && se.status == http.StatusUnauthorized {
		// Pear Desktop no longer accepts the token (darkbot removed from
		// its allowed clients, or its secret changed).
		err = fmt.Errorf("%w: %v", ErrNotAuthorized, err)
	}
	c.record(err)
	return err
}

// record keeps the outcome for the dashboard; a cancelled or timed out
// request (e.g. darkbot stopped waiting for a queue add) says nothing about
// Pear Desktop.
func (c *Client) record(err error) {
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return
	}
	c.mu.Lock()
	before := errText(c.lastErr)
	c.lastErr = err
	if err == nil {
		c.lastOK = time.Now()
	}
	now := errText(c.lastErr)
	c.mu.Unlock()
	if now != before {
		c.changed()
	}
}

func (c *Client) send(ctx context.Context, token, method, path string, body, out any) error {
	var rd io.Reader = http.NoBody
	if body != nil {
		data, err := json.Marshal(body)
		if err != nil {
			return err
		}
		rd = bytes.NewReader(data)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.base()+path, rd)
	if err != nil {
		return err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		// Nothing listens on the port: Pear Desktop is closed or its API
		// Server plugin is off.
		if op, ok := errors.AsType[*net.OpError](err); ok && op.Op == "dial" {
			return fmt.Errorf("%w: %v", ErrNotRunning, err)
		}
		return fmt.Errorf("pear desktop %s: %w", path, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, maxErrorBody))
		return &statusError{status: resp.StatusCode, path: path, body: strings.TrimSpace(string(msg))}
	}
	if out == nil || resp.StatusCode == http.StatusNoContent {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, maxBody))
		return nil
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxBody+1))
	if err != nil {
		return fmt.Errorf("pear desktop %s: %w", path, err)
	}
	if len(data) > maxBody {
		return fmt.Errorf("pear desktop %s: answer larger than %d bytes", path, maxBody)
	}
	if err := json.Unmarshal(data, out); err != nil {
		return fmt.Errorf("parse pear desktop %s: %w", path, err)
	}
	return nil
}

// Track is one song or video on YouTube Music.
type Track struct {
	VideoID  string
	Name     string
	Artists  string        // as YouTube Music writes them, e.g. "A, B & C"
	Duration time.Duration // 0 = unknown
	Explicit bool
}

// Title is "Artists – Name".
func (t Track) Title() string {
	if t.Artists == "" {
		return t.Name
	}
	return t.Artists + " – " + t.Name
}

// Search returns the best matching song for free text.
func (c *Client) Search(ctx context.Context, query string) (*Track, error) {
	items, err := c.search(ctx, query, songsFilter)
	if err != nil {
		return nil, err
	}
	if len(items) == 0 {
		return nil, ErrNotFound
	}
	return &items[0], nil
}

// Lookup finds a song or video by its ID. There is no endpoint for that, so
// it searches for the ID: the songs search finds songs, the full search
// also music videos and other uploads.
func (c *Client) Lookup(ctx context.Context, videoID string) (*Track, error) {
	for _, filter := range []string{songsFilter, ""} {
		items, err := c.search(ctx, videoID, filter)
		if err != nil {
			return nil, err
		}
		for _, t := range items {
			if t.VideoID == videoID {
				return &t, nil
			}
		}
	}
	return nil, ErrNotFound
}

func (c *Client) search(ctx context.Context, query, filter string) ([]Track, error) {
	body := map[string]string{"query": query}
	if filter != "" {
		body["params"] = filter
	}
	var raw any
	if err := c.call(ctx, http.MethodPost, "/search", body, &raw); err != nil {
		return nil, err
	}
	return searchResults(raw), nil
}

// Queue is what plays now and what comes next.
type Queue struct {
	Current *Track
	Next    []Track
	// IDs: every video in the queue (also ones already played), in order.
	IDs []string
}

// Queue reads Pear Desktop's queue.
func (c *Client) Queue(ctx context.Context) (*Queue, error) {
	var raw queueResponse
	if err := c.call(ctx, http.MethodGet, "/queue", nil, &raw); err != nil {
		return nil, err
	}
	return raw.queue(), nil
}

// Song is what Pear Desktop plays now (GET /api/v1/song, SongInfoSchema).
type Song struct {
	VideoID  string `json:"videoId"`
	Title    string `json:"title"`
	Artist   string `json:"artist"`
	ImageSrc string `json:"imageSrc"`
	// SongDuration and ElapsedSeconds are seconds.
	SongDuration   float64 `json:"songDuration"`
	ElapsedSeconds float64 `json:"elapsedSeconds"`
	IsPaused       *bool   `json:"isPaused"`
}

// Playing reports whether the song plays (not paused).
func (s *Song) Playing() bool { return s.IsPaused != nil && !*s.IsPaused }

// Song reads the song playing now; nil when there is none (204).
func (c *Client) Song(ctx context.Context) (*Song, error) {
	var s Song
	if err := c.call(ctx, http.MethodGet, "/song", nil, &s); err != nil {
		return nil, err
	}
	if s.VideoID == "" { // 204 leaves s empty
		return nil, nil
	}
	return &s, nil
}

// Playing reports whether a song plays right now (not paused).
func (c *Client) Playing(ctx context.Context) (bool, error) {
	s, err := c.Song(ctx)
	if err != nil || s == nil {
		return false, err
	}
	return s.Playing(), nil
}

// Add puts a song at the end of the queue and waits until it is there.
func (c *Client) Add(ctx context.Context, videoID string) error {
	c.addMu.Lock()
	defer c.addMu.Unlock()
	before, err := c.Queue(ctx)
	if err != nil {
		return err
	}
	had := count(before.IDs, videoID)
	body := map[string]string{"videoId": videoID, "insertPosition": "INSERT_AT_END"}
	if err := c.call(ctx, http.MethodPost, "/queue", body, nil); err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, c.addWait)
	defer cancel()
	tick := time.NewTicker(addPollInterval)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return fmt.Errorf("%w: %s not in the queue after %s", ErrNotAdded, videoID, c.addWait)
		case <-tick.C:
		}
		q, err := c.Queue(ctx)
		if err != nil {
			if ctx.Err() != nil {
				continue // the deadline ends the loop above
			}
			return err
		}
		if count(q.IDs, videoID) > had {
			return nil
		}
	}
}

// Next skips to the next song.
func (c *Client) Next(ctx context.Context) error {
	return c.call(ctx, http.MethodPost, "/next", nil, nil)
}

func count(ids []string, id string) int {
	n := 0
	for _, x := range ids {
		if x == id {
			n++
		}
	}
	return n
}

// ---- links ----

var videoIDPattern = regexp.MustCompile(`^[A-Za-z0-9_-]{11}$`)

// ParseLink returns the video ID of a YouTube or YouTube Music link. ok is
// false when text is no YouTube link at all. Links to playlists, channels
// etc. give ErrNotSongLink.
func ParseLink(text string) (id string, ok bool, err error) {
	fields := strings.Fields(text)
	if len(fields) == 0 {
		return "", false, nil
	}
	raw := fields[0]
	if !strings.Contains(raw, "://") {
		raw = "https://" + raw
	}
	u, perr := url.Parse(raw)
	if perr != nil {
		return "", false, nil
	}
	h := strings.ToLower(u.Hostname())
	for _, p := range []string{"www.", "m."} {
		h = strings.TrimPrefix(h, p)
	}
	switch h {
	case "youtube.com", "music.youtube.com":
		if u.Path != "/watch" {
			return "", true, ErrNotSongLink
		}
		id = u.Query().Get("v")
	case "youtu.be":
		id = strings.Trim(u.Path, "/")
	default:
		return "", false, nil
	}
	if !videoIDPattern.MatchString(id) {
		return "", true, ErrNotSongLink
	}
	return id, true, nil
}

// ---- YouTube Music's JSON ----

type run struct {
	Text               string `json:"text"`
	NavigationEndpoint *struct {
		WatchEndpoint *struct {
			VideoID string `json:"videoId"`
		} `json:"watchEndpoint"`
		BrowseEndpoint *struct {
			BrowseID string `json:"browseId"`
		} `json:"browseEndpoint"`
	} `json:"navigationEndpoint"`
}

type text struct {
	Runs []run `json:"runs"`
}

func (t text) String() string {
	var b strings.Builder
	for _, r := range t.Runs {
		b.WriteString(r.Text)
	}
	return strings.TrimSpace(b.String())
}

func (t text) videoID() string {
	for _, r := range t.Runs {
		if r.NavigationEndpoint != nil && r.NavigationEndpoint.WatchEndpoint != nil {
			return r.NavigationEndpoint.WatchEndpoint.VideoID
		}
	}
	return ""
}

type badge struct {
	Inline struct {
		Icon struct {
			IconType string `json:"iconType"`
		} `json:"icon"`
	} `json:"musicInlineBadgeRenderer"`
}

func explicit(badges []badge) bool {
	for _, b := range badges {
		if b.Inline.Icon.IconType == explicitBadge {
			return true
		}
	}
	return false
}

var durationText = regexp.MustCompile(`^\d{1,2}(?::\d{2}){1,2}$`)

// parseDuration reads "3:45" or "1:02:03".
func parseDuration(s string) time.Duration {
	if !durationText.MatchString(s) {
		return 0
	}
	var d time.Duration
	for p := range strings.SplitSeq(s, ":") {
		n, _ := strconv.Atoi(p)
		d = d*60 + time.Duration(n)
	}
	return d * time.Second
}

// subtitle reads "Artist • Album • 3:45" (songs), "Video • Artist •
// 1.8bn views • 3:34" (top result) and similar: the artists are the part
// that links to a channel, the duration the part that looks like one.
func subtitle(t text) (artists string, d time.Duration) {
	var parts [][]run
	cur := []run{}
	for _, r := range t.Runs {
		if strings.TrimSpace(r.Text) == separator {
			parts = append(parts, cur)
			cur = []run{}
			continue
		}
		cur = append(cur, r)
	}
	parts = append(parts, cur)
	for _, p := range parts {
		s := text{Runs: p}.String()
		if v := parseDuration(s); v > 0 {
			d = v
			continue
		}
		if artists != "" {
			continue
		}
		for _, r := range p {
			if r.NavigationEndpoint != nil && r.NavigationEndpoint.BrowseEndpoint != nil &&
				strings.HasPrefix(r.NavigationEndpoint.BrowseEndpoint.BrowseID, artistIDPrefix) {
				artists = s
				break
			}
		}
	}
	return artists, d
}

type listItem struct {
	PlaylistItemData struct {
		VideoID string `json:"videoId"`
	} `json:"playlistItemData"`
	FlexColumns []struct {
		Column struct {
			Text text `json:"text"`
		} `json:"musicResponsiveListItemFlexColumnRenderer"`
	} `json:"flexColumns"`
	Badges []badge `json:"badges"`
}

func (it listItem) track() (Track, bool) {
	if len(it.FlexColumns) == 0 {
		return Track{}, false
	}
	name := it.FlexColumns[0].Column.Text
	t := Track{VideoID: it.PlaylistItemData.VideoID, Name: name.String(), Explicit: explicit(it.Badges)}
	if t.VideoID == "" {
		t.VideoID = name.videoID()
	}
	if len(it.FlexColumns) > 1 {
		t.Artists, t.Duration = subtitle(it.FlexColumns[1].Column.Text)
	}
	return t, t.VideoID != "" && t.Name != ""
}

type card struct {
	Title    text `json:"title"`
	Subtitle text `json:"subtitle"`
}

func (c card) track() (Track, bool) {
	t := Track{VideoID: c.Title.videoID(), Name: c.Title.String()}
	t.Artists, t.Duration = subtitle(c.Subtitle)
	return t, t.VideoID != "" && t.Name != ""
}

// searchResults collects the playable results of a search answer in the
// order YouTube Music shows them (top result first). Albums, artists and
// playlists have no video ID and are left out.
func searchResults(raw any) []Track {
	var out []Track
	add := func(node any, into any, track func() (Track, bool)) {
		data, err := json.Marshal(node)
		if err != nil || json.Unmarshal(data, into) != nil {
			return
		}
		if t, ok := track(); ok {
			out = append(out, t)
		}
	}
	var walk func(any)
	walk = func(n any) {
		switch v := n.(type) {
		case map[string]any:
			if node, ok := v["musicCardShelfRenderer"]; ok {
				var c card
				add(node, &c, func() (Track, bool) { return c.track() })
			}
			if node, ok := v["musicResponsiveListItemRenderer"]; ok {
				var it listItem
				add(node, &it, func() (Track, bool) { return it.track() })
				return
			}
			for _, k := range sortedKeys(v) {
				walk(v[k])
			}
		case []any:
			for _, x := range v {
				walk(x)
			}
		}
	}
	walk(raw)
	return out
}

// sortedKeys: a JSON object has no order, but the results must keep
// YouTube Music's order, which lives in arrays; within one object the only
// key that matters is the one renderer, so any fixed order works.
func sortedKeys(m map[string]any) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	return keys
}

type panelVideo struct {
	VideoID        string  `json:"videoId"`
	Title          text    `json:"title"`
	LongBylineText text    `json:"longBylineText"`
	LengthText     text    `json:"lengthText"`
	Selected       bool    `json:"selected"`
	Badges         []badge `json:"badges"`
}

type queueResponse struct {
	Items []struct {
		Video   *panelVideo `json:"playlistPanelVideoRenderer"`
		Wrapper *struct {
			Primary struct {
				Video *panelVideo `json:"playlistPanelVideoRenderer"`
			} `json:"primaryRenderer"`
		} `json:"playlistPanelVideoWrapperRenderer"`
	} `json:"items"`
}

// queue: the selected item plays now, the ones after it come next. Before
// anything played, nothing is selected and all items come next.
func (r queueResponse) queue() *Queue {
	q := &Queue{}
	var items []Track
	current := -1
	for _, it := range r.Items {
		v := it.Video
		if v == nil && it.Wrapper != nil {
			v = it.Wrapper.Primary.Video
		}
		if v == nil || v.VideoID == "" {
			continue
		}
		t := Track{VideoID: v.VideoID, Name: v.Title.String(), Explicit: explicit(v.Badges), Duration: parseDuration(v.LengthText.String())}
		t.Artists, _ = subtitle(v.LongBylineText)
		if v.Selected {
			current = len(items)
		}
		items = append(items, t)
		q.IDs = append(q.IDs, t.VideoID)
	}
	if current >= 0 {
		q.Current = &items[current]
	}
	q.Next = items[current+1:]
	return q
}

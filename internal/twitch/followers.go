package twitch

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"sync"
	"time"

	"github.com/miwidot/darkbot/internal/secret"
)

// Unfollow detection. Twitch has no unfollow event, so FollowerWatcher
// reads the whole follower list (Get Channel Followers, verified 2026-11-18:
// https://dev.twitch.tv/docs/api/reference/#get-channel-followers, scope
// moderator:read:followers, max 100 per page, newest first by followed_at,
// user_id checks a single account) and compares it with the previous read.
// Accounts that vanished are reported as unfollows; that includes deleted
// or banned accounts, Twitch can't tell them apart.
//
// The list is saved, so the first read after a start also finds what
// happened while darkbot was off: unfollows, and follows Twitch never
// delivered because nothing was listening (EventSub has no catch-up).
const (
	followerPageSize = 100
	// FollowerCheckEvery: how often the list is compared. Unfollows show up
	// at most this late.
	FollowerCheckEvery = 10 * time.Minute
	// MaxTrackedFollowers keeps the load fair: 5,000 followers are 50
	// requests per check, far below Twitch's per-minute budget.
	// Bigger channels don't get unfollow detection.
	MaxTrackedFollowers = 5000
	// followerVerifyMax: a vanished account is checked once more on its own
	// before it counts as an unfollow, because the list can shift while it
	// is read page by page (someone unfollowing mid-read hides one account
	// at a page edge). That only ever hides a few, so past this many
	// vanished accounts the rest are real and reported without a check.
	followerVerifyMax = 20
	followerTimeout   = 2 * time.Minute
)

// follower is one entry of Get Channel Followers.
type follower struct {
	UserID     string    `json:"user_id"`
	UserName   string    `json:"user_name"`
	FollowedAt time.Time `json:"followed_at"`
}

// GetFollowers reads one page of followers.
func (h *Helix) GetFollowers(ctx context.Context, broadcasterID, after string) (list []follower, total int, cursor string, err error) {
	q := url.Values{"broadcaster_id": {broadcasterID}, "first": {strconv.Itoa(followerPageSize)}}
	if after != "" {
		q.Set("after", after)
	}
	return h.getFollowers(ctx, q)
}

// IsFollower checks whether one account follows the broadcaster.
func (h *Helix) IsFollower(ctx context.Context, broadcasterID, userID string) (bool, error) {
	list, _, _, err := h.getFollowers(ctx, url.Values{"broadcaster_id": {broadcasterID}, "user_id": {userID}})
	return len(list) > 0, err
}

func (h *Helix) getFollowers(ctx context.Context, q url.Values) ([]follower, int, string, error) {
	var out struct {
		Data       []follower `json:"data"`
		Total      int        `json:"total"`
		Pagination struct {
			Cursor string `json:"cursor"`
		} `json:"pagination"`
	}
	if err := h.do(ctx, http.MethodGet, "/channels/followers", q, nil, &out); err != nil {
		return nil, 0, "", err
	}
	return out.Data, out.Total, out.Pagination.Cursor, nil
}

// FollowerStatus is shown next to the unfollow setting.
type FollowerStatus struct {
	Tracking  int       `json:"tracking"`  // followers in the last complete read
	LastCheck time.Time `json:"lastCheck"` // zero = not yet
	TooMany   bool      `json:"tooMany"`   // channel is above MaxTrackedFollowers
	Total     int       `json:"total"`
}

// savedFollowers is the follower file.
type savedFollowers struct {
	BroadcasterID string            `json:"broadcaster_id"`
	CheckedAt     time.Time         `json:"checked_at"`
	Followers     map[string]string `json:"followers"` // user ID -> display name
}

// FollowerWatcher reports unfollows, and follows missed while darkbot was
// off. Run it only while the option is on.
type FollowerWatcher struct {
	helix  *Helix
	tokens *TokenManager
	log    Logger
	path   string // saved list; "" = keep it in memory only
	emit   func(ChannelEvent)
	now    func() time.Time

	mu      sync.Mutex
	started time.Time         // follows before this were missed, after it EventSub had them
	loaded  bool              // the saved list was looked at
	known   map[string]string // user ID -> display name
	saved   bool              // known came from the file and wasn't compared yet
	status  FollowerStatus
}

// NewFollowerWatcher creates a watcher that saves the list at path ("" =
// don't). emit receives EvUnfollow events and missed EvFollow events.
func NewFollowerWatcher(h *Helix, t *TokenManager, log Logger, path string, emit func(ChannelEvent)) *FollowerWatcher {
	return &FollowerWatcher{helix: h, tokens: t, log: log, path: path, emit: emit, now: time.Now, started: time.Now()}
}

// Status returns the last check result.
func (w *FollowerWatcher) Status() FollowerStatus {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.status
}

// Run checks now and then every FollowerCheckEvery until ctx ends.
func (w *FollowerWatcher) Run(ctx context.Context) {
	ticker := time.NewTicker(FollowerCheckEvery)
	defer ticker.Stop()
	for {
		w.check(ctx)
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

func (w *FollowerWatcher) check(ctx context.Context) {
	tok := w.tokens.Current()
	if tok == nil {
		return
	}
	if !tok.HasScope(ScopeReadFollowers) {
		w.log.Warnf("Unfollow detection needs a Twitch reconnect (follower permission)")
		return
	}
	w.loadOnce(tok.UserID)
	ctx, cancel := context.WithTimeout(ctx, followerTimeout)
	defer cancel()

	current, ok := w.readAll(ctx, tok.UserID)
	if !ok {
		return
	}
	names := make(map[string]string, len(current))
	for id, f := range current {
		names[id] = f.UserName
	}

	w.mu.Lock()
	prev, missed, started := w.known, w.saved, w.started
	w.mu.Unlock()

	var events []ChannelEvent
	if prev != nil {
		events = w.unfollows(ctx, tok.UserID, prev, names, missed)
		if missed {
			events = append(events, missedFollows(prev, current, started)...)
		}
	}

	w.mu.Lock()
	w.known, w.saved = names, false
	w.status = FollowerStatus{Tracking: len(names), Total: len(names), LastCheck: w.now()}
	w.mu.Unlock()
	w.save(tok.UserID, names)

	if prev == nil {
		w.log.Infof("Unfollow detection: watching %d followers", len(names))
	}
	for _, ev := range events {
		w.emit(ev)
	}
}

// readAll reads the whole list; false when it is incomplete or too big.
func (w *FollowerWatcher) readAll(ctx context.Context, broadcasterID string) (map[string]follower, bool) {
	current := map[string]follower{}
	cursor := ""
	for {
		page, total, next, err := w.helix.GetFollowers(ctx, broadcasterID, cursor)
		if err != nil {
			// An incomplete list would look like mass unfollows: skip this round.
			w.log.Warnf("Unfollow check skipped: %v", err)
			return nil, false
		}
		if total > MaxTrackedFollowers {
			w.mu.Lock()
			first := !w.status.TooMany
			w.status = FollowerStatus{TooMany: true, Total: total, LastCheck: w.now()}
			w.known, w.saved = nil, false
			w.mu.Unlock()
			if first {
				w.log.Warnf("Unfollow detection is off: %d followers is more than %d", total, MaxTrackedFollowers)
			}
			return nil, false
		}
		for _, f := range page {
			current[f.UserID] = f
		}
		if next == "" || len(page) == 0 {
			return current, true
		}
		cursor = next
	}
}

// unfollows returns an event per account in prev that is not in names,
// after checking it on its own (see followerVerifyMax). An account still
// following, or one the check failed for, is added back to names so the
// next round looks at it again.
func (w *FollowerWatcher) unfollows(ctx context.Context, broadcasterID string, prev, names map[string]string, missed bool) []ChannelEvent {
	var out []ChannelEvent
	verified := 0
	for id, name := range prev {
		if _, still := names[id]; still {
			continue
		}
		if verified < followerVerifyMax {
			verified++
			follows, err := w.helix.IsFollower(ctx, broadcasterID, id)
			if err != nil {
				w.log.Warnf("Could not check whether %s still follows, asking again next round: %v", name, err)
				names[id] = name
				continue
			}
			if follows {
				names[id] = name // hidden by the list shifting while it was read
				continue
			}
		}
		out = append(out, ChannelEvent{Kind: EvUnfollow, User: name, UserID: id, Missed: missed, At: w.now()})
	}
	return out
}

// missedFollows returns the follows that happened while darkbot was off:
// new in current and older than this run. Later ones came through EventSub.
func missedFollows(prev map[string]string, current map[string]follower, started time.Time) []ChannelEvent {
	var out []ChannelEvent
	for id, f := range current {
		if _, had := prev[id]; had || f.FollowedAt.IsZero() || !f.FollowedAt.Before(started) {
			continue
		}
		out = append(out, ChannelEvent{Kind: EvFollow, User: f.UserName, UserID: id, Missed: true, At: f.FollowedAt})
	}
	return out
}

// loadOnce reads the saved list of this broadcaster, once per watcher.
func (w *FollowerWatcher) loadOnce(broadcasterID string) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.loaded || w.path == "" {
		return
	}
	w.loaded = true
	data, err := os.ReadFile(w.path)
	if errors.Is(err, os.ErrNotExist) {
		return
	}
	var s savedFollowers
	if err == nil {
		err = json.Unmarshal(data, &s)
	}
	switch {
	case err != nil:
		w.log.Warnf("Ignoring the saved follower list %s, starting a new one: %v", w.path, err)
	case s.BroadcasterID != broadcasterID:
		// Another Twitch account was logged in; its list says nothing here.
	case s.Followers != nil:
		w.known, w.saved = s.Followers, true
	}
}

func (w *FollowerWatcher) save(broadcasterID string, names map[string]string) {
	if w.path == "" {
		return
	}
	data, err := json.Marshal(savedFollowers{BroadcasterID: broadcasterID, CheckedAt: w.now(), Followers: names})
	if err == nil {
		err = secret.WriteFileAtomic(w.path, data)
	}
	if err != nil {
		w.log.Warnf("Could not save the follower list (follows and unfollows while darkbot is off won't show): %v", fmt.Errorf("%s: %w", w.path, err))
	}
}

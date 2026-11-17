package twitch

import (
	"context"
	"net/http"
	"net/url"
	"strconv"
	"sync"
	"time"
)

// Unfollow detection. Twitch has no unfollow event, so FollowerWatcher
// reads the whole follower list (Get Channel Followers, verified 2026-11-17:
// https://dev.twitch.tv/docs/api/reference/#get-channel-followers, scope
// moderator:read:followers, max 100 per page, newest first) and compares it
// with the previous read. Accounts that vanished are reported as unfollows;
// that includes deleted or banned accounts, Twitch can't tell them apart.
const (
	followerPageSize = 100
	// FollowerCheckEvery: how often the list is compared. Unfollows show up
	// at most this late.
	FollowerCheckEvery = 10 * time.Minute
	// MaxTrackedFollowers keeps the load fair: 5,000 followers are 50
	// requests per check, far below Twitch's 800 requests per minute.
	// Bigger channels don't get unfollow detection.
	MaxTrackedFollowers = 5000
	followerTimeout     = 2 * time.Minute
)

// follower is one entry of Get Channel Followers.
type follower struct {
	UserID   string `json:"user_id"`
	UserName string `json:"user_name"`
}

// GetFollowers reads one page of followers.
func (h *Helix) GetFollowers(ctx context.Context, broadcasterID, after string) (list []follower, total int, cursor string, err error) {
	q := url.Values{"broadcaster_id": {broadcasterID}, "first": {strconv.Itoa(followerPageSize)}}
	if after != "" {
		q.Set("after", after)
	}
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

// FollowerWatcher reports unfollows. Run it only while the option is on.
type FollowerWatcher struct {
	helix  *Helix
	tokens *TokenManager
	log    Logger
	emit   func(ChannelEvent)

	mu     sync.Mutex
	known  map[string]string // user ID -> display name
	status FollowerStatus
}

// NewFollowerWatcher creates a watcher; emit receives EvUnfollow events.
func NewFollowerWatcher(h *Helix, t *TokenManager, log Logger, emit func(ChannelEvent)) *FollowerWatcher {
	return &FollowerWatcher{helix: h, tokens: t, log: log, emit: emit}
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
	ctx, cancel := context.WithTimeout(ctx, followerTimeout)
	defer cancel()

	current := map[string]string{}
	cursor := ""
	for {
		page, total, next, err := w.helix.GetFollowers(ctx, tok.UserID, cursor)
		if err != nil {
			// An incomplete list would look like mass unfollows: skip this round.
			w.log.Warnf("Unfollow check skipped: %v", err)
			return
		}
		if total > MaxTrackedFollowers {
			w.mu.Lock()
			first := !w.status.TooMany
			w.status = FollowerStatus{TooMany: true, Total: total, LastCheck: time.Now()}
			w.known = nil
			w.mu.Unlock()
			if first {
				w.log.Warnf("Unfollow detection is off: %d followers is more than %d", total, MaxTrackedFollowers)
			}
			return
		}
		for _, f := range page {
			current[f.UserID] = f.UserName
		}
		if next == "" || len(page) == 0 {
			break
		}
		cursor = next
	}

	w.mu.Lock()
	prev := w.known
	w.known = current
	w.status = FollowerStatus{Tracking: len(current), Total: len(current), LastCheck: time.Now()}
	w.mu.Unlock()

	if prev == nil {
		w.log.Infof("Unfollow detection: watching %d followers", len(current))
		return
	}
	for id, name := range prev {
		if _, still := current[id]; !still {
			w.emit(ChannelEvent{Kind: EvUnfollow, User: name, UserID: id})
		}
	}
}

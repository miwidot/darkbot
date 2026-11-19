package twitch

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"strconv"
)

// Clips (https://dev.twitch.tv/docs/api/reference/clips, checked 2026-11-19):
//   - Create Clip: POST /clips?broadcaster_id=…[&title=…][&duration=…],
//     user token with clips:edit. "duration" is 5 to 60 seconds (precision
//     0.1, default 30); the clip ends at the moment of the call. 202 starts
//     the clip and returns its id and edit_url.
//     400: category not clippable, or the title failed AutoMod.
//     403: clips are off in the channel or limited to followers/subs, or
//     the user is banned/timed out. 404: the broadcaster is not live.
//   - Creating is asynchronous: "call Get Clips using the clip ID … If
//     after 60 seconds Get Clips hasn't returned the clip, assume it
//     failed." Get Clips ignores IDs it doesn't know (no error).
const (
	// ScopeEditClips: create clips of the broadcaster's stream.
	ScopeEditClips = "clips:edit"
	// MinClipSeconds and MaxClipSeconds bound Create Clip's duration.
	MinClipSeconds = 5
	MaxClipSeconds = 60
)

// Errors of Create Clip, by the reasons the reference lists.
var (
	ErrClipOffline      = errors.New("clips need a live stream")
	ErrClipNotClippable = errors.New("this category can't be clipped, or the title failed AutoMod")
	ErrClipNotAllowed   = errors.New("clips are turned off or limited in this channel")
)

// Clip is a clip that exists on Twitch (Get Clips), or a new one whose
// creation has started (Create Clip: only ID and EditURL).
type Clip struct {
	ID      string `json:"id"`
	URL     string `json:"url"`
	EditURL string `json:"edit_url"`
	Title   string `json:"title"`
}

// CreateClip starts a clip of the broadcaster's live stream, ending now and
// seconds long (MinClipSeconds..MaxClipSeconds). An empty title keeps
// Twitch's default (the stream title).
func (h *Helix) CreateClip(ctx context.Context, broadcasterID, title string, seconds int) (Clip, error) {
	if seconds < MinClipSeconds || seconds > MaxClipSeconds {
		return Clip{}, errors.New("clip length out of range")
	}
	q := url.Values{"broadcaster_id": {broadcasterID}, "duration": {strconv.Itoa(seconds)}}
	if title != "" {
		q.Set("title", title)
	}
	var out struct {
		Data []Clip `json:"data"`
	}
	if err := h.do(ctx, http.MethodPost, "/clips", q, nil, &out); err != nil {
		return Clip{}, clipError(err)
	}
	if len(out.Data) == 0 || out.Data[0].ID == "" {
		return Clip{}, errors.New("create clip: empty response")
	}
	return out.Data[0], nil
}

// clipError names the documented failures of Create Clip.
func clipError(err error) error {
	if errors.Is(err, ErrNotFound) {
		return ErrClipOffline
	}
	if errors.Is(err, ErrNotAffiliate) {
		// A 403 whose message names partners: still "not allowed here".
		return errors.Join(ErrClipNotAllowed, err)
	}
	if e, ok := errors.AsType[*APIError](err); ok {
		switch e.Status {
		case http.StatusBadRequest:
			return errors.Join(ErrClipNotClippable, err)
		case http.StatusForbidden:
			return errors.Join(ErrClipNotAllowed, err)
		}
	}
	return err
}

// GetClip returns the clip with that ID; ok is false while Twitch doesn't
// list it (yet).
func (h *Helix) GetClip(ctx context.Context, id string) (Clip, bool, error) {
	var out struct {
		Data []Clip `json:"data"`
	}
	if err := h.do(ctx, http.MethodGet, "/clips", url.Values{"id": {id}}, nil, &out); err != nil {
		return Clip{}, false, err
	}
	for _, c := range out.Data {
		if c.ID == id {
			return c, true, nil
		}
	}
	return Clip{}, false, nil
}

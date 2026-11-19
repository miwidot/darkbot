package obs

// The "Now Playing" overlay: a browser source in the darkbot scene that
// shows a local HTML page (written by darkbot), fed through obs-browser's
// obs-websocket vendor request "emit_event", which dispatches a JavaScript
// event to every browser source. No port, no web server.
//
// Checked 2026-11-19 against obs-browser (github.com/obsproject/obs-browser):
// README "obs-websocket Vendor" (vendor "obs-browser", request "emit_event"
// with "event_name" and "event_data"), obs-browser-plugin.cpp (source id
// "browser_source", properties "is_local_file", "local_file", "width",
// "height", button "refreshnocache"; local files are served through the
// "http://absolute/" scheme so they may load files next to them).

import (
	"errors"
	"fmt"
	"strings"

	"github.com/andreykaipov/goobs"
	obsconfig "github.com/andreykaipov/goobs/api/requests/config"
	"github.com/andreykaipov/goobs/api/requests/general"
	"github.com/andreykaipov/goobs/api/requests/inputs"
	"github.com/andreykaipov/goobs/api/requests/sceneitems"
	"github.com/andreykaipov/goobs/api/requests/scenes"
)

const (
	// NowPlayingInput is the Now Playing overlay's browser source.
	NowPlayingInput = "[darkbot] now playing"
	// NowPlayingEvent is the JavaScript event the Now Playing page listens to.
	NowPlayingEvent = "darkbot-nowplaying"
	// GoalInput / GoalEvent and VoteInput / VoteEvent: the goal overlay
	// and the poll/prediction overlay.
	GoalInput     = "[darkbot] goals"
	GoalEvent     = "darkbot-goals"
	VoteInput     = "[darkbot] poll"
	VoteEvent     = "darkbot-vote"
	browserKind   = "browser_source"
	browserVendor = "obs-browser"
	emitEvent     = "emit_event"
	refreshButton = "refreshnocache"
	// NowPlayingWidth × NowPlayingHeight is the browser source's page size;
	// every design fits and sits in its bottom-left corner.
	NowPlayingWidth  = 800
	NowPlayingHeight = 300
	// overlayMargin: distance of a new source from the canvas's corner.
	// The streamer moves it in OBS; darkbot never resets it.
	overlayMargin = 40
	// OBS alignment flags (obs_align: LEFT 1, RIGHT 2, TOP 4, BOTTOM 8).
	obsAlignLeft   = 1
	obsAlignRight  = 2
	obsAlignTop    = 4
	obsAlignBottom = 8
)

// Corner is where a new overlay source is placed on the canvas.
type Corner int

const (
	BottomLeft Corner = iota
	TopLeft
	TopRight
)

// Overlay is one of darkbot's browser sources: a local page fed through
// obs-browser's emit_event.
type Overlay struct {
	Input         string
	Width, Height int
	Corner        Corner
}

// The overlays darkbot puts into its scene.
var (
	NowPlayingOverlay = Overlay{Input: NowPlayingInput, Width: NowPlayingWidth, Height: NowPlayingHeight, Corner: BottomLeft}
	// GoalOverlay: up to a few goal bars stacked, top left.
	GoalOverlay = Overlay{Input: GoalInput, Width: 640, Height: 320, Corner: TopLeft}
	// VoteOverlay: one poll or prediction card, top right.
	VoteOverlay = Overlay{Input: VoteInput, Width: 560, Height: 420, Corner: TopRight}
)

// ErrNoBrowserSource: this OBS has no browser source (some Linux builds).
var ErrNoBrowserSource = errors.New("OBS has no browser source")

// NowPlayingReady reports whether the Now Playing source is in the darkbot
// scene.
func (o *Client) NowPlayingReady() (bool, error) { return o.OverlayReady(NowPlayingInput) }

// SetupNowPlaying: SetupOverlay for Now Playing.
func (o *Client) SetupNowPlaying(file string, show bool) (bool, error) {
	return o.SetupOverlay(NowPlayingOverlay, file, show)
}

// ShowNowPlaying: ShowOverlay for Now Playing.
func (o *Client) ShowNowPlaying(on bool) error { return o.ShowOverlay(NowPlayingInput, on) }

// RefreshNowPlaying: RefreshOverlay for Now Playing.
func (o *Client) RefreshNowPlaying() error { return o.RefreshOverlay(NowPlayingInput) }

// EmitNowPlaying sends the song to the Now Playing page.
func (o *Client) EmitNowPlaying(data map[string]any) error { return o.Emit(NowPlayingEvent, data) }

// OverlayReady reports whether the overlay's source is in the darkbot scene.
func (o *Client) OverlayReady(input string) (bool, error) {
	c, err := o.client()
	if err != nil {
		return false, err
	}
	if ok, err := sceneExists(c); err != nil || !ok {
		return false, err
	}
	return hasItem(c, SceneName, input)
}

// SetupOverlay puts the overlay's browser source, showing file, into the
// darkbot scene (in its corner), or points an existing one at file. The
// darkbot scene must exist (ErrNotSetUp). created reports a new source.
func (o *Client) SetupOverlay(ov Overlay, file string, show bool) (created bool, err error) {
	c, err := o.client()
	if err != nil {
		return false, err
	}
	if ok, err := sceneExists(c); err != nil {
		return false, err
	} else if !ok {
		return false, ErrNotSetUp
	}
	settings := map[string]any{"is_local_file": true, "local_file": file, "width": ov.Width, "height": ov.Height}
	have, err := inputNames(c)
	if err != nil {
		return false, err
	}
	if !have[ov.Input] {
		if ok, err := hasBrowserSource(c); err != nil {
			return false, err
		} else if !ok {
			return false, ErrNoBrowserSource
		}
		r, err := c.Inputs.CreateInput(inputs.NewCreateInputParams().
			WithSceneName(SceneName).WithInputName(ov.Input).WithInputKind(browserKind).
			WithInputSettings(settings).WithSceneItemEnabled(show))
		if err != nil {
			return false, fmt.Errorf("create OBS source %q: %w", ov.Input, err)
		}
		return true, place(c, r.SceneItemId, ov.Corner)
	}
	if err := setInput(c, ov.Input, settings); err != nil {
		return false, err
	}
	if placed, err := hasItem(c, SceneName, ov.Input); err != nil {
		return false, err
	} else if !placed {
		if _, err := c.SceneItems.CreateSceneItem(sceneitems.NewCreateSceneItemParams().
			WithSceneName(SceneName).WithSourceName(ov.Input).WithSceneItemEnabled(show)); err != nil {
			return false, fmt.Errorf("add %q to the darkbot scene: %w", ov.Input, err)
		}
		return false, nil
	}
	return false, o.ShowOverlay(ov.Input, show)
}

// ShowOverlay turns an overlay's source on or off in the darkbot scene
// (nothing to do when it isn't there).
func (o *Client) ShowOverlay(input string, on bool) error {
	c, err := o.client()
	if err != nil {
		return err
	}
	if ok, err := sceneExists(c); err != nil || !ok {
		return err
	}
	if placed, err := hasItem(c, SceneName, input); err != nil || !placed {
		return err
	}
	ids, err := itemIDs(c, []string{input})
	if err != nil {
		return err
	}
	return enable(c, ids[0], on)
}

// RefreshOverlay reloads an overlay page (after darkbot wrote a new
// version of it).
func (o *Client) RefreshOverlay(input string) error {
	c, err := o.client()
	if err != nil {
		return err
	}
	if _, err := c.Inputs.PressInputPropertiesButton(inputs.NewPressInputPropertiesButtonParams().
		WithInputName(input).WithPropertyName(refreshButton)); err != nil {
		return fmt.Errorf("reload %q: %w", input, err)
	}
	return nil
}

// Emit sends data to the overlay pages as the JavaScript event name (all
// browser sources get it; only the page waiting for that name uses it).
func (o *Client) Emit(name string, data map[string]any) error {
	c, err := o.client()
	if err != nil {
		return err
	}
	if _, err := c.General.CallVendorRequest(general.NewCallVendorRequestParams().
		WithVendorName(browserVendor).WithRequestType(emitEvent).
		WithRequestData(map[string]any{"event_name": name, "event_data": data})); err != nil {
		return fmt.Errorf("send data to the OBS overlay: %w", err)
	}
	return nil
}

func sceneExists(c *goobs.Client) (bool, error) {
	list, err := c.Scenes.GetSceneList(&scenes.GetSceneListParams{})
	if err != nil {
		return false, fmt.Errorf("list OBS scenes: %w", err)
	}
	for _, s := range list.Scenes {
		if s.SceneName == SceneName {
			return true, nil
		}
	}
	return false, nil
}

func hasBrowserSource(c *goobs.Client) (bool, error) {
	r, err := c.Inputs.GetInputKindList(inputs.NewGetInputKindListParams().WithUnversioned(true))
	if err != nil {
		return false, fmt.Errorf("list OBS source types: %w", err)
	}
	for _, k := range r.InputKinds {
		if strings.HasPrefix(k, browserKind) {
			return true, nil
		}
	}
	return false, nil
}

// place puts a new source into a corner of the canvas.
func place(c *goobs.Client, itemID int, corner Corner) error {
	v, err := c.Config.GetVideoSettings(&obsconfig.GetVideoSettingsParams{})
	if err != nil {
		return fmt.Errorf("read the OBS canvas size: %w", err)
	}
	t, err := c.SceneItems.GetSceneItemTransform(sceneitems.NewGetSceneItemTransformParams().
		WithSceneName(SceneName).WithSceneItemId(itemID))
	if err != nil {
		return fmt.Errorf("read the position of a darkbot source: %w", err)
	}
	tr := *t.SceneItemTransform
	switch corner {
	case TopLeft:
		tr.Alignment, tr.PositionX, tr.PositionY = obsAlignTop|obsAlignLeft, overlayMargin, overlayMargin
	case TopRight:
		tr.Alignment, tr.PositionX, tr.PositionY = obsAlignTop|obsAlignRight, v.BaseWidth-overlayMargin, overlayMargin
	default:
		tr.Alignment, tr.PositionX, tr.PositionY = obsAlignBottom|obsAlignLeft, overlayMargin, v.BaseHeight-overlayMargin
	}
	tr.BoundsWidth = max(tr.BoundsWidth, minBounds)
	tr.BoundsHeight = max(tr.BoundsHeight, minBounds)
	if _, err := c.SceneItems.SetSceneItemTransform(sceneitems.NewSetSceneItemTransformParams().
		WithSceneName(SceneName).WithSceneItemId(itemID).WithSceneItemTransform(&tr)); err != nil {
		return fmt.Errorf("place a darkbot source: %w", err)
	}
	return nil
}

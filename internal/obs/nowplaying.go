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
	// NowPlayingInput is the overlay's browser source.
	NowPlayingInput = "[darkbot] now playing"
	// NowPlayingEvent is the JavaScript event the overlay page listens to.
	NowPlayingEvent = "darkbot-nowplaying"
	browserKind     = "browser_source"
	browserVendor   = "obs-browser"
	emitEvent       = "emit_event"
	refreshButton   = "refreshnocache"
	// NowPlayingWidth × NowPlayingHeight is the browser source's page size;
	// every design fits and sits in its bottom-left corner.
	NowPlayingWidth  = 800
	NowPlayingHeight = 300
	// nowPlayingMargin: distance of the new source from the canvas's
	// bottom-left corner. The streamer moves it in OBS; darkbot never
	// resets it.
	nowPlayingMargin = 40
	// OBS alignment flags (obs_align: LEFT 1, BOTTOM 8).
	obsAlignBottomLeft = 1 | 8
)

// ErrNoBrowserSource: this OBS has no browser source (some Linux builds).
var ErrNoBrowserSource = errors.New("OBS has no browser source")

// NowPlayingReady reports whether the overlay's source is in the darkbot
// scene.
func (o *Client) NowPlayingReady() (bool, error) {
	c, err := o.client()
	if err != nil {
		return false, err
	}
	if ok, err := sceneExists(c); err != nil || !ok {
		return false, err
	}
	return hasItem(c, SceneName, NowPlayingInput)
}

// SetupNowPlaying puts the overlay's browser source, showing file, into the
// darkbot scene (bottom-left), or points an existing one at file. The
// darkbot scene must exist (ErrNotSetUp). created reports a new source.
func (o *Client) SetupNowPlaying(file string, show bool) (created bool, err error) {
	c, err := o.client()
	if err != nil {
		return false, err
	}
	if ok, err := sceneExists(c); err != nil {
		return false, err
	} else if !ok {
		return false, ErrNotSetUp
	}
	settings := map[string]any{"is_local_file": true, "local_file": file, "width": NowPlayingWidth, "height": NowPlayingHeight}
	have, err := inputNames(c)
	if err != nil {
		return false, err
	}
	if !have[NowPlayingInput] {
		if ok, err := hasBrowserSource(c); err != nil {
			return false, err
		} else if !ok {
			return false, ErrNoBrowserSource
		}
		r, err := c.Inputs.CreateInput(inputs.NewCreateInputParams().
			WithSceneName(SceneName).WithInputName(NowPlayingInput).WithInputKind(browserKind).
			WithInputSettings(settings).WithSceneItemEnabled(show))
		if err != nil {
			return false, fmt.Errorf("create OBS source %q: %w", NowPlayingInput, err)
		}
		return true, placeBottomLeft(c, r.SceneItemId)
	}
	if err := setInput(c, NowPlayingInput, settings); err != nil {
		return false, err
	}
	if placed, err := hasItem(c, SceneName, NowPlayingInput); err != nil {
		return false, err
	} else if !placed {
		if _, err := c.SceneItems.CreateSceneItem(sceneitems.NewCreateSceneItemParams().
			WithSceneName(SceneName).WithSourceName(NowPlayingInput).WithSceneItemEnabled(show)); err != nil {
			return false, fmt.Errorf("add %q to the darkbot scene: %w", NowPlayingInput, err)
		}
		return false, nil
	}
	return false, o.ShowNowPlaying(show)
}

// ShowNowPlaying turns the overlay's source on or off in the darkbot scene
// (nothing to do when it isn't there).
func (o *Client) ShowNowPlaying(on bool) error {
	c, err := o.client()
	if err != nil {
		return err
	}
	if ok, err := sceneExists(c); err != nil || !ok {
		return err
	}
	if placed, err := hasItem(c, SceneName, NowPlayingInput); err != nil || !placed {
		return err
	}
	ids, err := itemIDs(c, []string{NowPlayingInput})
	if err != nil {
		return err
	}
	return enable(c, ids[0], on)
}

// RefreshNowPlaying reloads the overlay page (after darkbot wrote a new
// version of it).
func (o *Client) RefreshNowPlaying() error {
	c, err := o.client()
	if err != nil {
		return err
	}
	if _, err := c.Inputs.PressInputPropertiesButton(inputs.NewPressInputPropertiesButtonParams().
		WithInputName(NowPlayingInput).WithPropertyName(refreshButton)); err != nil {
		return fmt.Errorf("reload %q: %w", NowPlayingInput, err)
	}
	return nil
}

// EmitNowPlaying sends data to the overlay page (all browser sources get
// the event; only darkbot's page listens to it).
func (o *Client) EmitNowPlaying(data map[string]any) error {
	c, err := o.client()
	if err != nil {
		return err
	}
	if _, err := c.General.CallVendorRequest(general.NewCallVendorRequestParams().
		WithVendorName(browserVendor).WithRequestType(emitEvent).
		WithRequestData(map[string]any{"event_name": NowPlayingEvent, "event_data": data})); err != nil {
		return fmt.Errorf("send the song to OBS: %w", err)
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

// placeBottomLeft puts a new source into the canvas's bottom-left corner.
func placeBottomLeft(c *goobs.Client, itemID int) error {
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
	tr.Alignment = obsAlignBottomLeft
	tr.PositionX = nowPlayingMargin
	tr.PositionY = v.BaseHeight - nowPlayingMargin
	tr.BoundsWidth = max(tr.BoundsWidth, minBounds)
	tr.BoundsHeight = max(tr.BoundsHeight, minBounds)
	if _, err := c.SceneItems.SetSceneItemTransform(sceneitems.NewSetSceneItemTransformParams().
		WithSceneName(SceneName).WithSceneItemId(itemID).WithSceneItemTransform(&tr)); err != nil {
		return fmt.Errorf("place a darkbot source: %w", err)
	}
	return nil
}

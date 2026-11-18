package obs

// Alerts in OBS without a port: darkbot owns one scene, "[darkbot]", that
// holds everything it shows or plays (sound, image/GIF, video, text). The
// streamer's scenes get that scene as a single nested item, so darkbot
// never touches anything else in them and new alert types show up
// everywhere at once.
//
// Requests used (obs-websocket v5 protocol, checked 2026-11-18,
// https://github.com/obsproject/obs-websocket/blob/master/docs/generated/protocol.md):
// GetSceneList, CreateScene, GetSceneItemList, CreateSceneItem,
// SetSceneItemIndex ("An index of 0 is at the bottom of the source list"),
// SetSceneItemEnabled, Get/SetSceneItemTransform, GetInputList,
// GetInputKindList, CreateInput, SetInputSettings, GetVideoSettings,
// GetMediaInputStatus, TriggerMediaInputAction; for removing and renaming:
// RemoveSceneItem, RemoveInput ("Will immediately remove all associated
// scene items"), RemoveScene, SetSceneName, SetInputName.

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/andreykaipov/goobs"
	obsconfig "github.com/andreykaipov/goobs/api/requests/config"
	"github.com/andreykaipov/goobs/api/requests/inputs"
	"github.com/andreykaipov/goobs/api/requests/mediainputs"
	"github.com/andreykaipov/goobs/api/requests/sceneitems"
	"github.com/andreykaipov/goobs/api/requests/scenes"
	"github.com/andreykaipov/goobs/api/typedefs"

	"github.com/miwidot/darkbot/internal/config"
)

// Names in OBS. The brackets set darkbot's parts apart from the
// streamer's own in OBS's lists. The streamer may move and style the
// sources inside the darkbot scene; darkbot finds them by name.
const (
	SceneName  = "[darkbot]"
	imageInput = "[darkbot] image"
	videoInput = "[darkbot] video"
	textInput  = "[darkbot] text"
)

// Names darkbot 0.2.6 used. They are renamed on the next contact with OBS,
// so an existing setup keeps its positions and styles.
const legacySceneName = "darkbot"

var legacyInputs = map[string]string{
	"darkbot-image": imageInput,
	"darkbot-video": videoInput,
	"darkbot-text":  textInput,
}

// ErrNotSetUp: an alert needs the darkbot scene, and it isn't in OBS.
var ErrNotSetUp = errors.New("the darkbot scene is not set up in OBS (Settings, OBS)")

const (
	imageKind = "image_source" // plays GIFs animated
	// textFontSize: readable on a 1080p canvas; restyle it in OBS.
	textFontSize = 72
	// textBelow: the text sits this share of the canvas height below the
	// centre, under the picture.
	textBelow = 0.30
	// obsAlignCenter: OBS alignment flags, 0 = centre on both axes.
	obsAlignCenter = 0
	// minBounds: OBS refuses bounds below 1 even when bounds are off.
	minBounds = 1
	// mediaDurationWait: how long Show waits for OBS to report a video's
	// length after starting it.
	mediaDurationWait = 1500 * time.Millisecond
	mediaPoll         = 100 * time.Millisecond
)

// File types for alerts: images (GIFs animate) and videos (WebM can be
// transparent).
var (
	ImageExtensions = []string{".png", ".jpg", ".jpeg", ".gif", ".webp", ".bmp"}
	VideoExtensions = []string{".webm", ".mp4", ".mov", ".mkv"}
)

// IsVideo tells video alert files from images.
func IsVideo(path string) bool {
	return slices.Contains(VideoExtensions, strings.ToLower(filepath.Ext(path)))
}

// IsVisual reports a supported alert file.
func IsVisual(path string) bool {
	ext := strings.ToLower(filepath.Ext(path))
	return slices.Contains(ImageExtensions, ext) || slices.Contains(VideoExtensions, ext)
}

// SceneStatus says where the darkbot scene is.
type SceneStatus struct {
	Exists bool `json:"exists"`
	// Missing lists the streamer's scenes without the darkbot scene in them.
	Missing []string `json:"missing"`
	Scenes  int      `json:"scenes"` // the streamer's scenes (without darkbot's)
}

// Status reads where the darkbot scene is nested. It first renames parts
// left by older darkbot versions (see renameLegacy).
func (o *Client) Status(soundInput string) (SceneStatus, error) {
	c, err := o.client()
	if err != nil {
		return SceneStatus{}, err
	}
	if err := renameLegacy(c, soundInput); err != nil {
		return SceneStatus{}, err
	}
	list, err := c.Scenes.GetSceneList(&scenes.GetSceneListParams{})
	if err != nil {
		return SceneStatus{}, fmt.Errorf("list OBS scenes: %w", err)
	}
	st := SceneStatus{Missing: []string{}}
	for _, s := range list.Scenes {
		if s.SceneName == SceneName {
			st.Exists = true
			continue
		}
		st.Scenes++
		has, err := hasItem(c, s.SceneName, SceneName)
		if err != nil {
			return SceneStatus{}, err
		}
		if !has {
			st.Missing = append(st.Missing, s.SceneName)
		}
	}
	return st, nil
}

// Setup creates the darkbot scene with its sources and nests it on top of
// every other scene. soundInput is the sound source's name. It is safe to
// run again: existing parts are kept as they are.
func (o *Client) Setup(soundInput string) (added []string, err error) {
	c, err := o.client()
	if err != nil {
		return nil, err
	}
	if err := renameLegacy(c, soundInput); err != nil {
		return nil, err
	}
	if err := ensureScene(c, soundInput); err != nil {
		return nil, err
	}
	list, err := c.Scenes.GetSceneList(&scenes.GetSceneListParams{})
	if err != nil {
		return nil, fmt.Errorf("list OBS scenes: %w", err)
	}
	for _, s := range list.Scenes {
		if s.SceneName == SceneName {
			continue
		}
		has, err := hasItem(c, s.SceneName, SceneName)
		if err != nil {
			return added, err
		}
		if has {
			continue
		}
		if err := nestOnTop(c, s.SceneName); err != nil {
			return added, err
		}
		added = append(added, s.SceneName)
	}
	return added, nil
}

// Remove takes darkbot out of OBS: the darkbot scene out of each of the
// streamer's scenes, darkbot's sources and the darkbot scene itself. The
// sound source goes only under its default name; a custom name may be one
// of the streamer's own sources. It returns the scenes darkbot was taken
// out of.
func (o *Client) Remove(soundInput string) (from []string, err error) {
	c, err := o.client()
	if err != nil {
		return nil, err
	}
	if err := renameLegacy(c, soundInput); err != nil {
		return nil, err
	}
	list, err := c.Scenes.GetSceneList(&scenes.GetSceneListParams{})
	if err != nil {
		return nil, fmt.Errorf("list OBS scenes: %w", err)
	}
	exists := false
	for _, s := range list.Scenes {
		if s.SceneName == SceneName {
			exists = true
			continue
		}
		its, err := items(c, s.SceneName)
		if err != nil {
			return from, err
		}
		for _, it := range its {
			if it.SourceName != SceneName {
				continue
			}
			if _, err := c.SceneItems.RemoveSceneItem(sceneitems.NewRemoveSceneItemParams().
				WithSceneName(s.SceneName).WithSceneItemId(it.SceneItemID)); err != nil {
				return from, fmt.Errorf("take the darkbot scene out of %q: %w", s.SceneName, err)
			}
			if !slices.Contains(from, s.SceneName) {
				from = append(from, s.SceneName)
			}
		}
	}
	own := []string{imageInput, videoInput, textInput}
	if soundInput == config.DefaultOBSInputName {
		own = append(own, soundInput)
	}
	have, err := inputNames(c)
	if err != nil {
		return from, err
	}
	for _, name := range own {
		if !have[name] {
			continue
		}
		if _, err := c.Inputs.RemoveInput(inputs.NewRemoveInputParams().WithInputName(name)); err != nil {
			return from, fmt.Errorf("remove OBS source %q: %w", name, err)
		}
	}
	if exists {
		if _, err := c.Scenes.RemoveScene(scenes.NewRemoveSceneParams().WithSceneName(SceneName)); err != nil {
			return from, fmt.Errorf("remove the OBS scene %q: %w", SceneName, err)
		}
	}
	return from, nil
}

// renameLegacy gives parts made by older darkbot versions today's names:
// the scene "darkbot" (only if it holds darkbot's sources, so a scene the
// streamer happened to call so stays), its sources, and the sound source
// under its old default name when the default is in use. Renaming keeps
// the parts where they are nested and how they are styled.
func renameLegacy(c *goobs.Client, soundInput string) error {
	have, err := inputNames(c)
	if err != nil {
		return err
	}
	renames := maps.Clone(legacyInputs)
	if soundInput == config.DefaultOBSInputName {
		renames[config.LegacyOBSInputName] = soundInput
	}
	for old, name := range renames {
		if !have[old] || have[name] {
			continue
		}
		if _, err := c.Inputs.SetInputName(inputs.NewSetInputNameParams().
			WithInputName(old).WithNewInputName(name)); err != nil {
			return fmt.Errorf("rename OBS source %q to %q: %w", old, name, err)
		}
	}
	list, err := c.Scenes.GetSceneList(&scenes.GetSceneListParams{})
	if err != nil {
		return fmt.Errorf("list OBS scenes: %w", err)
	}
	names := map[string]bool{}
	for _, s := range list.Scenes {
		names[s.SceneName] = true
	}
	if !names[legacySceneName] || names[SceneName] {
		return nil
	}
	ours := false
	for _, n := range []string{imageInput, videoInput, textInput} {
		if ours, err = hasItem(c, legacySceneName, n); err != nil || ours {
			break
		}
	}
	if err != nil || !ours {
		return err
	}
	if _, err := c.Scenes.SetSceneName(scenes.NewSetSceneNameParams().
		WithSceneName(legacySceneName).WithNewSceneName(SceneName)); err != nil {
		return fmt.Errorf("rename the OBS scene %q to %q: %w", legacySceneName, SceneName, err)
	}
	return nil
}

func inputNames(c *goobs.Client) (map[string]bool, error) {
	list, err := c.Inputs.GetInputList(&inputs.GetInputListParams{})
	if err != nil {
		return nil, fmt.Errorf("list OBS sources: %w", err)
	}
	have := map[string]bool{}
	for _, in := range list.Inputs {
		have[in.InputName] = true
	}
	return have, nil
}

func items(c *goobs.Client, scene string) ([]*typedefs.SceneItem, error) {
	r, err := c.SceneItems.GetSceneItemList(sceneitems.NewGetSceneItemListParams().WithSceneName(scene))
	if err != nil {
		return nil, fmt.Errorf("list items of OBS scene %q: %w", scene, err)
	}
	return r.SceneItems, nil
}

func hasItem(c *goobs.Client, scene, source string) (bool, error) {
	list, err := items(c, scene)
	if err != nil {
		return false, err
	}
	for _, it := range list {
		if it.SourceName == source {
			return true, nil
		}
	}
	return false, nil
}

// nestOnTop adds the darkbot scene to scene, above everything else.
func nestOnTop(c *goobs.Client, scene string) error {
	before, err := items(c, scene)
	if err != nil {
		return err
	}
	r, err := c.SceneItems.CreateSceneItem(sceneitems.NewCreateSceneItemParams().
		WithSceneName(scene).WithSourceName(SceneName).WithSceneItemEnabled(true))
	if err != nil {
		return fmt.Errorf("add the darkbot scene to %q: %w", scene, err)
	}
	// The highest index is the top of the list; with the new item there
	// are len(before)+1 items, so the top index is len(before).
	if _, err := c.SceneItems.SetSceneItemIndex(sceneitems.NewSetSceneItemIndexParams().
		WithSceneName(scene).WithSceneItemId(r.SceneItemId).WithSceneItemIndex(len(before))); err != nil {
		return fmt.Errorf("move the darkbot scene to the top of %q: %w", scene, err)
	}
	return nil
}

// ensureScene creates the darkbot scene and its sources where missing.
func ensureScene(c *goobs.Client, soundInput string) error {
	list, err := c.Scenes.GetSceneList(&scenes.GetSceneListParams{})
	if err != nil {
		return fmt.Errorf("list OBS scenes: %w", err)
	}
	if !slices.ContainsFunc(list.Scenes, func(s *typedefs.Scene) bool { return s.SceneName == SceneName }) {
		if _, err := c.Scenes.CreateScene(scenes.NewCreateSceneParams().WithSceneName(SceneName)); err != nil {
			return fmt.Errorf("create the OBS scene %q: %w", SceneName, err)
		}
	}
	have, err := inputNames(c)
	if err != nil {
		return err
	}
	inScene, err := items(c, SceneName)
	if err != nil {
		return err
	}
	placed := map[string]bool{}
	for _, it := range inScene {
		placed[it.SourceName] = true
	}
	textKind, err := textInputKind(c)
	if err != nil {
		return err
	}
	want := []struct {
		name, kind string
		settings   map[string]any
	}{
		{soundInput, mediaSourceKind, soundSettings()},
		{imageInput, imageKind, map[string]any{}},
		{videoInput, mediaSourceKind, map[string]any{"is_local_file": true, "looping": false, "restart_on_activate": false, "close_when_inactive": true}},
		{textInput, textKind, map[string]any{"text": "", "font": map[string]any{"face": "Arial", "size": textFontSize, "style": "Bold"}}},
	}
	for _, w := range want {
		switch {
		case !have[w.name]:
			// Visual sources start hidden; the sound source is always on.
			r, err := c.Inputs.CreateInput(inputs.NewCreateInputParams().
				WithSceneName(SceneName).WithInputName(w.name).WithInputKind(w.kind).
				WithInputSettings(w.settings).WithSceneItemEnabled(w.name == soundInput))
			if err != nil {
				return fmt.Errorf("create OBS source %q: %w", w.name, err)
			}
			if w.name != soundInput {
				if err := center(c, r.SceneItemId, w.name == textInput); err != nil {
					return err
				}
			}
		case !placed[w.name]:
			// e.g. the sound source from older versions, in one of the
			// streamer's scenes: also put it in darkbot's scene.
			if _, err := c.SceneItems.CreateSceneItem(sceneitems.NewCreateSceneItemParams().
				WithSceneName(SceneName).WithSourceName(w.name).WithSceneItemEnabled(w.name == soundInput)); err != nil {
				return fmt.Errorf("add %q to the darkbot scene: %w", w.name, err)
			}
		}
	}
	return nil
}

func soundSettings() map[string]any {
	return map[string]any{"is_local_file": true, "restart_on_activate": false, "close_when_inactive": true}
}

// textInputKind: OBS's text source differs per system (GDI+ on Windows,
// FreeType elsewhere) and carries a version suffix.
func textInputKind(c *goobs.Client) (string, error) {
	r, err := c.Inputs.GetInputKindList(inputs.NewGetInputKindListParams().WithUnversioned(false))
	if err != nil {
		return "", fmt.Errorf("list OBS source types: %w", err)
	}
	for _, prefix := range []string{"text_gdiplus", "text_ft2_source"} {
		var best string
		for _, k := range r.InputKinds {
			if strings.HasPrefix(k, prefix) && k > best { // highest version
				best = k
			}
		}
		if best != "" {
			return best, nil
		}
	}
	return "", errors.New("OBS has no text source type")
}

// center puts a new source in the middle of the canvas (text below it).
// The streamer can move it in OBS afterwards; darkbot never resets it.
func center(c *goobs.Client, itemID int, below bool) error {
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
	tr.Alignment = obsAlignCenter
	tr.PositionX = v.BaseWidth / 2
	tr.PositionY = v.BaseHeight / 2
	if below {
		tr.PositionY += v.BaseHeight * textBelow
	}
	tr.BoundsWidth = max(tr.BoundsWidth, minBounds)
	tr.BoundsHeight = max(tr.BoundsHeight, minBounds)
	if _, err := c.SceneItems.SetSceneItemTransform(sceneitems.NewSetSceneItemTransformParams().
		WithSceneName(SceneName).WithSceneItemId(itemID).WithSceneItemTransform(&tr)); err != nil {
		return fmt.Errorf("place a darkbot source: %w", err)
	}
	return nil
}

// Alert is one visual alert: a picture or video file and/or a text.
type Alert struct {
	File string
	Text string
}

// Show displays an alert in the darkbot scene and returns how to hide it
// and, for a video, how long it plays (0 = unknown or a picture).
func (o *Client) Show(ctx context.Context, a Alert, soundInput string) (hide func() error, length time.Duration, err error) {
	c, err := o.client()
	if err != nil {
		return nil, 0, err
	}
	// After Remove, alerts must not quietly bring the scene back.
	list, err := c.Scenes.GetSceneList(&scenes.GetSceneListParams{})
	if err != nil {
		return nil, 0, fmt.Errorf("list OBS scenes: %w", err)
	}
	if !slices.ContainsFunc(list.Scenes, func(s *typedefs.Scene) bool { return s.SceneName == SceneName }) {
		return nil, 0, ErrNotSetUp
	}
	if err := ensureScene(c, soundInput); err != nil { // repairs deleted sources
		return nil, 0, err
	}
	var show []string
	video := a.File != "" && IsVideo(a.File)
	switch {
	case video:
		if err := setInput(c, videoInput, map[string]any{"is_local_file": true, "local_file": a.File, "looping": false}); err != nil {
			return nil, 0, err
		}
		show = append(show, videoInput)
	case a.File != "":
		if err := setInput(c, imageInput, map[string]any{"file": a.File}); err != nil {
			return nil, 0, err
		}
		show = append(show, imageInput)
	}
	if a.Text != "" {
		if err := setInput(c, textInput, map[string]any{"text": a.Text}); err != nil {
			return nil, 0, err
		}
		show = append(show, textInput)
	}
	ids, err := itemIDs(c, show)
	if err != nil {
		return nil, 0, err
	}
	for _, id := range ids {
		if err := enable(c, id, true); err != nil {
			return nil, 0, err
		}
	}
	if video {
		if _, err := c.MediaInputs.TriggerMediaInputAction(mediainputs.NewTriggerMediaInputActionParams().
			WithInputName(videoInput).WithMediaAction(restartAction)); err != nil {
			return nil, 0, fmt.Errorf("start the alert video: %w", err)
		}
		length = videoLength(ctx, c)
	}
	// Hide through the connection current at that time: OBS may have
	// reconnected while the alert was showing.
	return func() error { return o.hide(show) }, length, nil
}

// alertInputs are the sources an alert shows.
var alertInputs = []string{imageInput, videoInput, textInput}

// HideAlerts hides every alert source, e.g. after (re)connecting, when an
// alert of a dropped connection may still be on screen. Without the
// darkbot scene there is nothing to hide.
func (o *Client) HideAlerts() error {
	c, err := o.client()
	if err != nil {
		return err
	}
	list, err := items(c, SceneName)
	if err != nil {
		return nil // no darkbot scene (not set up or removed)
	}
	var present []string
	for _, n := range alertInputs {
		if slices.ContainsFunc(list, func(it *typedefs.SceneItem) bool { return it.SourceName == n }) {
			present = append(present, n)
		}
	}
	return o.hide(present)
}

func (o *Client) hide(names []string) error {
	c, err := o.client()
	if err != nil {
		return err
	}
	ids, err := itemIDs(c, names)
	if err != nil {
		return err
	}
	var errs []error
	for _, id := range ids {
		errs = append(errs, enable(c, id, false))
	}
	return errors.Join(errs...)
}

func setInput(c *goobs.Client, name string, settings map[string]any) error {
	if _, err := c.Inputs.SetInputSettings(inputs.NewSetInputSettingsParams().
		WithInputName(name).WithInputSettings(settings).WithOverlay(true)); err != nil {
		return fmt.Errorf("set OBS source %q: %w", name, err)
	}
	return nil
}

func itemIDs(c *goobs.Client, names []string) ([]int, error) {
	list, err := items(c, SceneName)
	if err != nil {
		return nil, err
	}
	var ids []int
	for _, n := range names {
		i := slices.IndexFunc(list, func(it *typedefs.SceneItem) bool { return it.SourceName == n })
		if i < 0 {
			return nil, fmt.Errorf("OBS source %q is not in the darkbot scene", n)
		}
		ids = append(ids, list[i].SceneItemID)
	}
	return ids, nil
}

func enable(c *goobs.Client, id int, on bool) error {
	_, err := c.SceneItems.SetSceneItemEnabled(sceneitems.NewSetSceneItemEnabledParams().
		WithSceneName(SceneName).WithSceneItemId(id).WithSceneItemEnabled(on))
	if err != nil {
		return fmt.Errorf("show/hide a darkbot source: %w", err)
	}
	return nil
}

// videoLength asks OBS for the alert video's length once it plays.
func videoLength(ctx context.Context, c *goobs.Client) time.Duration {
	deadline := time.Now().Add(mediaDurationWait)
	for time.Now().Before(deadline) {
		st, err := c.MediaInputs.GetMediaInputStatus(mediainputs.NewGetMediaInputStatusParams().WithInputName(videoInput))
		if err == nil && st.MediaDuration > 0 {
			return time.Duration(st.MediaDuration) * time.Millisecond
		}
		select {
		case <-ctx.Done():
			return 0
		case <-time.After(mediaPoll):
		}
	}
	return 0
}

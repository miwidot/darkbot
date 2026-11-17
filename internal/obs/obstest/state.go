package obstest

import (
	"encoding/json"
	"slices"
	"sync"
)

// obs-websocket request status codes the fake uses.
const (
	statusOK            = 100
	statusNotFound      = 600
	statusAlreadyExists = 601
	statusUnknown       = 204 // UnknownRequestType
)

// Canvas size of the fake OBS.
const canvasWidth, canvasHeight = 1920, 1080

// Item is a scene item: a source placed in a scene.
type Item struct {
	ID      int
	Source  string
	Enabled bool
	X, Y    float64
}

// Input is a source with its kind and settings.
type Input struct {
	Kind     string
	Settings map[string]any
}

// State is a tiny OBS: ordered scenes with ordered items (index 0 =
// bottom), inputs, and a log of request types.
type State struct {
	mu       sync.Mutex
	Scenes   []string
	Items    map[string][]Item
	Inputs   map[string]*Input
	Requests []string
	nextID   int
}

// NewState has one scene, "Gameplay", with a camera.
func NewState() *State {
	s := &State{Items: map[string][]Item{}, Inputs: map[string]*Input{}}
	s.Scenes = []string{"Gameplay"}
	s.Inputs["Camera"] = &Input{Kind: "av_capture_input", Settings: map[string]any{}}
	s.Items["Gameplay"] = []Item{{ID: s.id(), Source: "Camera", Enabled: true}}
	return s
}

func (s *State) id() int { s.nextID++; return s.nextID }

// AddScene adds a scene, like the streamer clicking + in OBS.
func (s *State) AddScene(name string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.Scenes = append(s.Scenes, name)
	s.Items[name] = nil
}

// Snapshot returns a copy of the items of a scene.
func (s *State) Snapshot(scene string) []Item {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.Items[scene])
}

// Input returns a copy of an input's settings (nil = missing).
func (s *State) Input(name string) map[string]any {
	s.mu.Lock()
	defer s.mu.Unlock()
	in, ok := s.Inputs[name]
	if !ok {
		return nil
	}
	out := map[string]any{"_kind": in.Kind}
	for k, v := range in.Settings {
		out[k] = v
	}
	return out
}

// Count returns how often a request type was sent.
func (s *State) Count(req string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := 0
	for _, r := range s.Requests {
		if r == req {
			n++
		}
	}
	return n
}

type req struct {
	SceneName          string         `json:"sceneName"`
	SourceName         string         `json:"sourceName"`
	InputName          string         `json:"inputName"`
	InputKind          string         `json:"inputKind"`
	InputSettings      map[string]any `json:"inputSettings"`
	SceneItemEnabled   *bool          `json:"sceneItemEnabled"`
	SceneItemID        int            `json:"sceneItemId"`
	SceneItemIndex     int            `json:"sceneItemIndex"`
	SceneItemTransform map[string]any `json:"sceneItemTransform"`
}

func (s *State) handle(typ string, raw json.RawMessage) (map[string]any, int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.Requests = append(s.Requests, typ)
	var r req
	_ = json.Unmarshal(raw, &r)
	switch typ {
	case "GetVersion":
		return map[string]any{"obsVersion": "33.0.1", "obsWebSocketVersion": "5.7.6"}, statusOK
	case "GetSceneList":
		list := []map[string]any{}
		for i, n := range s.Scenes {
			list = append(list, map[string]any{"sceneName": n, "sceneIndex": i})
		}
		return map[string]any{"scenes": list}, statusOK
	case "CreateScene":
		if slices.Contains(s.Scenes, r.SceneName) {
			return nil, statusAlreadyExists
		}
		s.Scenes = append(s.Scenes, r.SceneName)
		s.Items[r.SceneName] = nil
		return map[string]any{"sceneUuid": r.SceneName}, statusOK
	case "GetSceneItemList":
		if !slices.Contains(s.Scenes, r.SceneName) {
			return nil, statusNotFound
		}
		list := []map[string]any{}
		for i, it := range s.Items[r.SceneName] {
			list = append(list, map[string]any{"sceneItemId": it.ID, "sceneItemIndex": i, "sourceName": it.Source, "sceneItemEnabled": it.Enabled})
		}
		return map[string]any{"sceneItems": list}, statusOK
	case "CreateSceneItem":
		_, isInput := s.Inputs[r.SourceName]
		if !slices.Contains(s.Scenes, r.SceneName) || (!isInput && !slices.Contains(s.Scenes, r.SourceName)) {
			return nil, statusNotFound
		}
		// The protocol doesn't say where a new item goes. The fake puts it at
		// the bottom, the worst case, so tests prove darkbot moves it up.
		it := Item{ID: s.id(), Source: r.SourceName, Enabled: r.SceneItemEnabled == nil || *r.SceneItemEnabled}
		s.Items[r.SceneName] = slices.Insert(s.Items[r.SceneName], 0, it)
		return map[string]any{"sceneItemId": it.ID}, statusOK
	case "SetSceneItemIndex":
		list := s.Items[r.SceneName]
		i := slices.IndexFunc(list, func(it Item) bool { return it.ID == r.SceneItemID })
		if i < 0 || r.SceneItemIndex >= len(list) {
			return nil, statusNotFound
		}
		it := list[i]
		list = slices.Delete(list, i, i+1)
		s.Items[r.SceneName] = slices.Insert(list, r.SceneItemIndex, it)
		return map[string]any{}, statusOK
	case "SetSceneItemEnabled":
		return s.editItem(r, func(it *Item) { it.Enabled = r.SceneItemEnabled != nil && *r.SceneItemEnabled })
	case "GetSceneItemTransform":
		return map[string]any{"sceneItemTransform": map[string]any{"scaleX": 1, "scaleY": 1, "boundsType": "OBS_BOUNDS_NONE", "boundsWidth": 0, "boundsHeight": 0}}, statusOK
	case "SetSceneItemTransform":
		if w, _ := r.SceneItemTransform["boundsWidth"].(float64); w < 1 {
			return nil, 400 // real OBS: boundsWidth must be >= 1
		}
		return s.editItem(r, func(it *Item) {
			it.X, _ = r.SceneItemTransform["positionX"].(float64)
			it.Y, _ = r.SceneItemTransform["positionY"].(float64)
		})
	case "GetInputList":
		list := []map[string]any{}
		for n, in := range s.Inputs {
			list = append(list, map[string]any{"inputName": n, "inputKind": in.Kind})
		}
		return map[string]any{"inputs": list}, statusOK
	case "GetInputKindList":
		return map[string]any{"inputKinds": []string{"image_source", "ffmpeg_source", "text_gdiplus_v2", "text_gdiplus_v3", "color_source_v3"}}, statusOK
	case "CreateInput":
		if _, ok := s.Inputs[r.InputName]; ok {
			return nil, statusAlreadyExists
		}
		s.Inputs[r.InputName] = &Input{Kind: r.InputKind, Settings: r.InputSettings}
		it := Item{ID: s.id(), Source: r.InputName, Enabled: r.SceneItemEnabled == nil || *r.SceneItemEnabled}
		s.Items[r.SceneName] = append(s.Items[r.SceneName], it)
		return map[string]any{"sceneItemId": it.ID, "inputUuid": r.InputName}, statusOK
	case "SetInputSettings":
		in, ok := s.Inputs[r.InputName]
		if !ok {
			return nil, statusNotFound
		}
		if in.Settings == nil {
			in.Settings = map[string]any{}
		}
		for k, v := range r.InputSettings {
			in.Settings[k] = v
		}
		return map[string]any{}, statusOK
	case "GetVideoSettings":
		return map[string]any{"baseWidth": canvasWidth, "baseHeight": canvasHeight, "outputWidth": canvasWidth, "outputHeight": canvasHeight}, statusOK
	case "TriggerMediaInputAction":
		return map[string]any{}, statusOK
	case "GetMediaInputStatus":
		return map[string]any{"mediaState": "OBS_MEDIA_STATE_PLAYING", "mediaDuration": 3000, "mediaCursor": 0}, statusOK
	}
	return nil, statusUnknown
}

func (s *State) editItem(r req, fn func(*Item)) (map[string]any, int) {
	list := s.Items[r.SceneName]
	for i := range list {
		if list[i].ID == r.SceneItemID {
			fn(&list[i])
			return map[string]any{}, statusOK
		}
	}
	return nil, statusNotFound
}

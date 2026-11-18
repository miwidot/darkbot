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
	// Emitted holds the requestData of each obs-browser emit_event call.
	Emitted []map[string]any
	// NoBrowser: this OBS has no browser source (SetNoBrowser).
	NoBrowser bool
}

// SetNoBrowser makes this OBS one without a browser source.
func (s *State) SetNoBrowser() {
	s.mu.Lock()
	s.NoBrowser = true
	s.mu.Unlock()
}

// EmittedEvents returns a copy of the emit_event calls.
func (s *State) EmittedEvents() []map[string]any {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.Emitted)
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
	NewSceneName       string         `json:"newSceneName"`
	NewInputName       string         `json:"newInputName"`
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
		kinds := []string{"image_source", "ffmpeg_source", "text_gdiplus_v2", "text_gdiplus_v3", "color_source_v3"}
		if !s.NoBrowser {
			kinds = append(kinds, "browser_source")
		}
		return map[string]any{"inputKinds": kinds}, statusOK
	case "CallVendorRequest":
		var v struct {
			VendorName  string         `json:"vendorName"`
			RequestType string         `json:"requestType"`
			RequestData map[string]any `json:"requestData"`
		}
		_ = json.Unmarshal(raw, &v)
		if v.VendorName != "obs-browser" || v.RequestType != "emit_event" {
			return nil, statusNotFound
		}
		s.Emitted = append(s.Emitted, v.RequestData)
		return map[string]any{"vendorName": v.VendorName, "requestType": v.RequestType, "responseData": map[string]any{}}, statusOK
	case "PressInputPropertiesButton":
		if _, ok := s.Inputs[r.InputName]; !ok {
			return nil, statusNotFound
		}
		return map[string]any{}, statusOK
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
	case "SetSceneName":
		if !slices.Contains(s.Scenes, r.SceneName) {
			return nil, statusNotFound
		}
		if s.sourceExists(r.NewSceneName) {
			return nil, statusAlreadyExists
		}
		s.Scenes[slices.Index(s.Scenes, r.SceneName)] = r.NewSceneName
		s.Items[r.NewSceneName] = s.Items[r.SceneName]
		delete(s.Items, r.SceneName)
		s.renameSource(r.SceneName, r.NewSceneName)
		return map[string]any{}, statusOK
	case "SetInputName":
		in, ok := s.Inputs[r.InputName]
		if !ok {
			return nil, statusNotFound
		}
		if s.sourceExists(r.NewInputName) {
			return nil, statusAlreadyExists
		}
		s.Inputs[r.NewInputName] = in
		delete(s.Inputs, r.InputName)
		s.renameSource(r.InputName, r.NewInputName)
		return map[string]any{}, statusOK
	case "RemoveSceneItem":
		list := s.Items[r.SceneName]
		i := slices.IndexFunc(list, func(it Item) bool { return it.ID == r.SceneItemID })
		if i < 0 {
			return nil, statusNotFound
		}
		s.Items[r.SceneName] = slices.Delete(list, i, i+1)
		return map[string]any{}, statusOK
	case "RemoveInput":
		// "Will immediately remove all associated scene items."
		if _, ok := s.Inputs[r.InputName]; !ok {
			return nil, statusNotFound
		}
		delete(s.Inputs, r.InputName)
		s.dropSource(r.InputName)
		return map[string]any{}, statusOK
	case "RemoveScene":
		i := slices.Index(s.Scenes, r.SceneName)
		if i < 0 {
			return nil, statusNotFound
		}
		s.Scenes = slices.Delete(s.Scenes, i, i+1)
		delete(s.Items, r.SceneName)
		s.dropSource(r.SceneName) // where it was nested
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

// Scene and input names share one namespace in OBS (both are sources).
func (s *State) sourceExists(name string) bool {
	_, isInput := s.Inputs[name]
	return isInput || slices.Contains(s.Scenes, name)
}

// renameSource updates every item showing the source; OBS items refer to
// the source itself, so a rename shows everywhere.
func (s *State) renameSource(old, name string) {
	for _, list := range s.Items {
		for i := range list {
			if list[i].Source == old {
				list[i].Source = name
			}
		}
	}
}

// dropSource removes every item showing the source.
func (s *State) dropSource(name string) {
	for scene, list := range s.Items {
		s.Items[scene] = slices.DeleteFunc(list, func(it Item) bool { return it.Source == name })
	}
}

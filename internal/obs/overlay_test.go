package obs

import (
	"context"
	"errors"
	"slices"
	"testing"
	"time"

	"github.com/andreykaipov/goobs/api/requests/inputs"
	"github.com/andreykaipov/goobs/api/requests/sceneitems"
	"github.com/andreykaipov/goobs/api/requests/scenes"

	"github.com/miwidot/darkbot/internal/config"
	"github.com/miwidot/darkbot/internal/obs/obstest"
)

// testSound is the default sound source name, which darkbot owns.
const testSound = config.DefaultOBSInputName

func connected(t *testing.T) (*Client, *obstest.Server) {
	t.Helper()
	f := obstest.New(t, false)
	o := New(nil)
	host, port := f.HostPort(t)
	if _, err := o.Connect(host, port, ""); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(o.Disconnect)
	return o, f
}

func sources(items []obstest.Item) []string {
	var out []string
	for _, it := range items {
		out = append(out, it.Source)
	}
	return out
}

func TestSetupCreatesSceneAndNestsItOnTop(t *testing.T) {
	o, f := connected(t)
	st, err := o.Status(testSound)
	if err != nil || st.Exists || !slices.Equal(st.Missing, []string{"Gameplay"}) {
		t.Fatalf("before: %+v %v", st, err)
	}
	added, err := o.Setup(testSound)
	if err != nil || !slices.Equal(added, []string{"Gameplay"}) {
		t.Fatalf("setup: %v %v", added, err)
	}
	own := f.State.Snapshot(SceneName)
	if got := sources(own); !slices.Equal(got, []string{testSound, imageInput, videoInput, textInput}) {
		t.Fatalf("darkbot scene holds %v", got)
	}
	for _, it := range own {
		if it.Enabled != (it.Source == testSound) {
			t.Errorf("%s enabled=%v: pictures start hidden, the sound source is always on", it.Source, it.Enabled)
		}
	}
	if img := own[1]; img.X != 960 || img.Y != 540 {
		t.Errorf("picture at %v,%v, want the canvas centre", img.X, img.Y)
	}
	if txt := own[3]; txt.X != 960 || txt.Y <= 540 {
		t.Errorf("text at %v,%v, want under the picture", txt.X, txt.Y)
	}
	if k := f.State.Input(textInput)["_kind"]; k != "text_gdiplus_v3" {
		t.Errorf("text kind %v, want the newest available", k)
	}
	game := sources(f.State.Snapshot("Gameplay"))
	if !slices.Equal(game, []string{"Camera", SceneName}) {
		t.Fatalf("Gameplay holds %v, want the darkbot scene on top and the camera untouched", game)
	}
	if st, _ := o.Status(testSound); !st.Exists || len(st.Missing) != 0 || st.Scenes != 1 {
		t.Fatalf("after: %+v", st)
	}
}

func TestSetupTwiceChangesNothing(t *testing.T) {
	o, f := connected(t)
	if _, err := o.Setup(testSound); err != nil {
		t.Fatal(err)
	}
	before := len(f.State.Snapshot(SceneName)) + len(f.State.Snapshot("Gameplay"))
	added, err := o.Setup(testSound)
	if err != nil || len(added) != 0 {
		t.Fatalf("second setup: %v %v", added, err)
	}
	if after := len(f.State.Snapshot(SceneName)) + len(f.State.Snapshot("Gameplay")); after != before {
		t.Fatalf("items %d -> %d", before, after)
	}
}

func TestNewSceneIsReportedAndAdded(t *testing.T) {
	o, f := connected(t)
	if _, err := o.Setup(testSound); err != nil {
		t.Fatal(err)
	}
	f.State.AddScene("Just Chatting")
	if st, _ := o.Status(testSound); !slices.Equal(st.Missing, []string{"Just Chatting"}) {
		t.Fatalf("missing %v", st.Missing)
	}
	added, err := o.Setup(testSound)
	if err != nil || !slices.Equal(added, []string{"Just Chatting"}) {
		t.Fatalf("added %v %v", added, err)
	}
}

// The sound source from older versions sits in a streamer scene: setup also
// puts it in the darkbot scene and leaves the old item alone.
func TestSetupAdoptsOldSoundSource(t *testing.T) {
	o, f := connected(t)
	c, _ := o.client()
	if _, err := c.Inputs.CreateInput(newSoundInput("Gameplay")); err != nil {
		t.Fatal(err)
	}
	if _, err := o.Setup(testSound); err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(sources(f.State.Snapshot(SceneName)), testSound) {
		t.Fatal("the sound source must be in the darkbot scene")
	}
	if !slices.Contains(sources(f.State.Snapshot("Gameplay")), testSound) {
		t.Fatal("the old item must stay")
	}
}

func TestShowPictureAndText(t *testing.T) {
	o, f := connected(t)
	if _, err := o.Setup(testSound); err != nil {
		t.Fatal(err)
	}
	hide, length, err := o.Show(context.Background(), Alert{File: "/alerts/follow.gif", Text: "Anna folgt!"}, testSound)
	if err != nil || length != 0 {
		t.Fatalf("show: %v %v", length, err)
	}
	if f.State.Input(imageInput)["file"] != "/alerts/follow.gif" || f.State.Input(textInput)["text"] != "Anna folgt!" {
		t.Fatalf("image %v text %v", f.State.Input(imageInput), f.State.Input(textInput))
	}
	shown := func() map[string]bool {
		m := map[string]bool{}
		for _, it := range f.State.Snapshot(SceneName) {
			m[it.Source] = it.Enabled
		}
		return m
	}
	if s := shown(); !s[imageInput] || !s[textInput] || s[videoInput] {
		t.Fatalf("visible %v", s)
	}
	if err := hide(); err != nil {
		t.Fatal(err)
	}
	if s := shown(); s[imageInput] || s[textInput] {
		t.Fatalf("still visible after hide: %v", s)
	}
}

// OBS reconnects while an alert shows: hiding goes through the new
// connection, and a connect hides alerts a lost connection left visible.
func TestHideAfterReconnect(t *testing.T) {
	o, f := connected(t)
	if _, err := o.Setup(testSound); err != nil {
		t.Fatal(err)
	}
	enabled := func(src string) bool {
		for _, it := range f.State.Snapshot(SceneName) {
			if it.Source == src {
				return it.Enabled
			}
		}
		return false
	}
	reconnect := func() {
		host, port := f.HostPort(t)
		if _, err := o.Connect(host, port, ""); err != nil {
			t.Fatal(err)
		}
	}
	hide, _, err := o.Show(context.Background(), Alert{Text: "Anna folgt!"}, testSound)
	if err != nil {
		t.Fatal(err)
	}
	reconnect()
	if err := hide(); err != nil || enabled(textInput) {
		t.Fatalf("hide after reconnect: %v, visible %v", err, enabled(textInput))
	}

	if _, _, err := o.Show(context.Background(), Alert{Text: "Ben folgt!"}, testSound); err != nil {
		t.Fatal(err)
	}
	reconnect() // the old hide is lost with the connection
	if err := o.HideAlerts(); err != nil || enabled(textInput) {
		t.Fatalf("HideAlerts: %v, visible %v", err, enabled(textInput))
	}
}

// Without the darkbot scene there is nothing to hide.
func TestHideAlertsWithoutScene(t *testing.T) {
	o, _ := connected(t)
	if err := o.HideAlerts(); err != nil {
		t.Fatal(err)
	}
}

func TestShowVideoReportsLength(t *testing.T) {
	o, f := connected(t)
	if _, err := o.Setup(testSound); err != nil {
		t.Fatal(err)
	}
	_, length, err := o.Show(context.Background(), Alert{File: "/alerts/raid.webm"}, testSound)
	if err != nil || length != 3*time.Second {
		t.Fatalf("length %v %v", length, err)
	}
	if f.State.Input(videoInput)["local_file"] != "/alerts/raid.webm" || f.State.Count("TriggerMediaInputAction") != 1 {
		t.Fatalf("video %v", f.State.Input(videoInput))
	}
}

func TestShowWithoutOBS(t *testing.T) {
	o := New(nil)
	if _, _, err := o.Show(context.Background(), Alert{Text: "x"}, testSound); err == nil {
		t.Fatal("no OBS: must fail")
	}
}

// Without the darkbot scene (never set up, or removed) an alert is skipped
// and does not bring the scene back.
func TestShowNeedsSetup(t *testing.T) {
	o, f := connected(t)
	if _, _, err := o.Show(context.Background(), Alert{Text: "x"}, testSound); !errors.Is(err, ErrNotSetUp) {
		t.Fatalf("want ErrNotSetUp, got %v", err)
	}
	if f.State.Input(textInput) != nil || f.State.Count("CreateScene") != 0 {
		t.Fatal("an alert must not create anything in OBS")
	}
	if !o.Connected() {
		t.Fatal("a missing scene is no reason to drop OBS")
	}
}

// Remove takes out everything darkbot added and nothing else.
func TestRemove(t *testing.T) {
	o, f := connected(t)
	f.State.AddScene("Just Chatting")
	if _, err := o.Setup(testSound); err != nil {
		t.Fatal(err)
	}
	from, err := o.Remove(testSound)
	if err != nil || !slices.Equal(from, []string{"Gameplay", "Just Chatting"}) {
		t.Fatalf("removed from %v, %v", from, err)
	}
	if got := sources(f.State.Snapshot("Gameplay")); !slices.Equal(got, []string{"Camera"}) {
		t.Fatalf("Gameplay holds %v, want only the camera", got)
	}
	for _, n := range []string{testSound, imageInput, videoInput, textInput} {
		if f.State.Input(n) != nil {
			t.Errorf("source %q is still in OBS", n)
		}
	}
	if f.State.Input("Camera") == nil {
		t.Fatal("the streamer's camera must stay")
	}
	st, err := o.Status(testSound)
	if err != nil || st.Exists || !slices.Equal(st.Missing, []string{"Gameplay", "Just Chatting"}) {
		t.Fatalf("after remove: %+v %v", st, err)
	}
	// Removing again is harmless; setting up again works.
	if from, err := o.Remove(testSound); err != nil || len(from) != 0 {
		t.Fatalf("second remove: %v %v", from, err)
	}
	if added, err := o.Setup(testSound); err != nil || len(added) != 2 {
		t.Fatalf("setup after remove: %v %v", added, err)
	}
}

// A custom sound source name may be the streamer's own source: Remove
// leaves it in OBS.
func TestRemoveKeepsCustomSoundSource(t *testing.T) {
	o, f := connected(t)
	const own = "My Alerts"
	if _, err := o.Setup(own); err != nil {
		t.Fatal(err)
	}
	if _, err := o.Remove(own); err != nil {
		t.Fatal(err)
	}
	if f.State.Input(own) == nil {
		t.Fatal("a custom-named sound source must stay")
	}
}

// A setup made by darkbot 0.2.6 (scene "darkbot", sources "darkbot-…") is
// renamed to the bracket names, keeping its nesting and settings.
func TestLegacyNamesAreRenamed(t *testing.T) {
	o, f := connected(t)
	c, _ := o.client()
	if _, err := c.Scenes.CreateScene(scenes.NewCreateSceneParams().WithSceneName(legacySceneName)); err != nil {
		t.Fatal(err)
	}
	for _, n := range []string{"darkbot-image", "darkbot-video", "darkbot-text", config.LegacyOBSInputName} {
		if _, err := c.Inputs.CreateInput(inputs.NewCreateInputParams().WithSceneName(legacySceneName).
			WithInputName(n).WithInputKind(mediaSourceKind).WithInputSettings(map[string]any{"styled": n})); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := c.SceneItems.CreateSceneItem(sceneitems.NewCreateSceneItemParams().
		WithSceneName("Gameplay").WithSourceName(legacySceneName)); err != nil {
		t.Fatal(err)
	}
	st, err := o.Status(testSound)
	if err != nil || !st.Exists || len(st.Missing) != 0 {
		t.Fatalf("status: %+v %v", st, err)
	}
	if got := sources(f.State.Snapshot("Gameplay")); !slices.Contains(got, SceneName) {
		t.Fatalf("Gameplay holds %v, want the renamed scene still nested", got)
	}
	want := []string{imageInput, videoInput, textInput, testSound}
	if got := sources(f.State.Snapshot(SceneName)); !slices.Equal(got, want) {
		t.Fatalf("[darkbot] holds %v, want %v", got, want)
	}
	if f.State.Input(imageInput)["styled"] != "darkbot-image" {
		t.Fatal("renaming must keep the settings")
	}
	if added, err := o.Setup(testSound); err != nil || len(added) != 0 {
		t.Fatalf("setup after rename must add nothing: %v %v", added, err)
	}
}

// A streamer's own scene that happens to be called "darkbot" is not taken.
func TestForeignDarkbotSceneStays(t *testing.T) {
	o, f := connected(t)
	f.State.AddScene(legacySceneName)
	if _, err := o.Status(testSound); err != nil {
		t.Fatal(err)
	}
	if f.State.Count("SetSceneName") != 0 {
		t.Fatal("a scene without darkbot's sources must keep its name")
	}
}

func newSoundInput(scene string) *inputs.CreateInputParams {
	return inputs.NewCreateInputParams().WithSceneName(scene).WithInputName(testSound).WithInputKind(mediaSourceKind).WithInputSettings(soundSettings())
}

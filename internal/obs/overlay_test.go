package obs

import (
	"context"
	"slices"
	"testing"
	"time"

	"github.com/andreykaipov/goobs/api/requests/inputs"

	"github.com/miwidot/darkbot/internal/obs/obstest"
)

const testSound = "darkbot-sound"

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
	st, err := o.Status()
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
	if st, _ := o.Status(); !st.Exists || len(st.Missing) != 0 || st.Scenes != 1 {
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
	if st, _ := o.Status(); !slices.Equal(st.Missing, []string{"Just Chatting"}) {
		t.Fatalf("missing %v", st.Missing)
	}
	added, err := o.Setup(testSound)
	if err != nil || !slices.Equal(added, []string{"Just Chatting"}) {
		t.Fatalf("added %v %v", added, err)
	}
}

// darkbot-sound from older versions sits in a streamer scene: setup also
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
	hide()
	if s := shown(); s[imageInput] || s[textInput] {
		t.Fatalf("still visible after hide: %v", s)
	}
}

func TestShowVideoReportsLength(t *testing.T) {
	o, f := connected(t)
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

func newSoundInput(scene string) *inputs.CreateInputParams {
	return inputs.NewCreateInputParams().WithSceneName(scene).WithInputName(testSound).WithInputKind(mediaSourceKind).WithInputSettings(soundSettings())
}

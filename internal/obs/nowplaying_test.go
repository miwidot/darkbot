package obs

import (
	"errors"
	"slices"
	"testing"
)

func TestNowPlayingSource(t *testing.T) {
	o, f := connected(t)
	if _, err := o.SetupNowPlaying("/a/nowplaying.html", true); !errors.Is(err, ErrNotSetUp) {
		t.Fatalf("needs the darkbot scene first: %v", err)
	}
	if ready, err := o.NowPlayingReady(); ready || err != nil {
		t.Fatalf("ready before setup: %v %v", ready, err)
	}
	if err := o.ShowNowPlaying(true); err != nil {
		t.Fatalf("show without a source is a no-op: %v", err)
	}
	if _, err := o.Setup(testSound); err != nil {
		t.Fatal(err)
	}
	created, err := o.SetupNowPlaying("/a/nowplaying.html", true)
	if err != nil || !created {
		t.Fatalf("create: %v %v", created, err)
	}
	in := f.State.Input(NowPlayingInput)
	if in["_kind"] != browserKind || in["local_file"] != "/a/nowplaying.html" || in["is_local_file"] != true {
		t.Fatalf("settings %v", in)
	}
	item := func() (it struct {
		on   bool
		x, y float64
	}) {
		for _, i := range f.State.Snapshot(SceneName) {
			if i.Source == NowPlayingInput {
				it.on, it.x, it.y = i.Enabled, i.X, i.Y
			}
		}
		return
	}
	if it := item(); !it.on || it.x != overlayMargin || it.y != 1080-overlayMargin {
		t.Fatalf("placed %+v: bottom-left, shown", it)
	}
	if ready, _ := o.NowPlayingReady(); !ready {
		t.Fatal("ready after setup")
	}
	// Again (e.g. darkbot's files moved): same source, new path, hidden.
	created, err = o.SetupNowPlaying("/b/nowplaying.html", false)
	if err != nil || created || f.State.Input(NowPlayingInput)["local_file"] != "/b/nowplaying.html" || item().on {
		t.Fatalf("update: %v %v %v", created, err, f.State.Input(NowPlayingInput))
	}
	if err := o.HideAlerts(); err != nil {
		t.Fatal(err)
	}
	if err := o.ShowNowPlaying(true); err != nil || !item().on {
		t.Fatalf("show: %v", err)
	}
	if err := o.HideAlerts(); err != nil || !item().on {
		t.Fatal("hiding alerts must leave the song overlay alone")
	}
	if err := o.EmitNowPlaying(map[string]any{"title": "Around the World"}); err != nil {
		t.Fatal(err)
	}
	if ev := f.State.EmittedEvents(); len(ev) != 1 || ev[0]["event_name"] != NowPlayingEvent ||
		ev[0]["event_data"].(map[string]any)["title"] != "Around the World" {
		t.Fatalf("emitted %v", ev)
	}
	if err := o.RefreshNowPlaying(); err != nil || f.State.Count("PressInputPropertiesButton") != 1 {
		t.Fatalf("refresh: %v", err)
	}
	if _, err := o.Remove(testSound); err != nil {
		t.Fatal(err)
	}
	if f.State.Input(NowPlayingInput) != nil {
		t.Fatal("removing darkbot from OBS removes the overlay too")
	}
}

func TestNowPlayingWithoutBrowserSource(t *testing.T) {
	o, f := connected(t)
	f.State.SetNoBrowser()
	if _, err := o.Setup(testSound); err != nil {
		t.Fatal(err)
	}
	if _, err := o.SetupNowPlaying("/a/nowplaying.html", true); !errors.Is(err, ErrNoBrowserSource) {
		t.Fatalf("err %v", err)
	}
	if slices.Contains(sources(f.State.Snapshot(SceneName)), NowPlayingInput) {
		t.Fatal("nothing created")
	}
}

// The goal and poll overlays go into the other corners; Remove takes them
// out with darkbot's other sources.
func TestOverlayCorners(t *testing.T) {
	o, f := connected(t)
	if _, err := o.Setup(testSound); err != nil {
		t.Fatal(err)
	}
	for _, ov := range []Overlay{GoalOverlay, VoteOverlay} {
		if created, err := o.SetupOverlay(ov, "/a/"+ov.Input+".html", true); err != nil || !created {
			t.Fatalf("%s: %v %v", ov.Input, created, err)
		}
	}
	pos := map[string][2]float64{}
	for _, i := range f.State.Snapshot(SceneName) {
		pos[i.Source] = [2]float64{i.X, i.Y}
	}
	if pos[GoalInput] != [2]float64{overlayMargin, overlayMargin} {
		t.Errorf("goals at %v, want top left", pos[GoalInput])
	}
	if pos[VoteInput] != [2]float64{1920 - overlayMargin, overlayMargin} {
		t.Errorf("poll at %v, want top right", pos[VoteInput])
	}
	if err := o.Emit(GoalEvent, map[string]any{"goals": []any{}}); err != nil {
		t.Fatal(err)
	}
	if _, err := o.Remove(testSound); err != nil {
		t.Fatal(err)
	}
	if f.State.Input(GoalInput) != nil || f.State.Input(VoteInput) != nil {
		t.Fatal("Remove left the overlays in OBS")
	}
}

package main

import (
	"testing"
	"time"

	"github.com/miwidot/darkbot/internal/twitch"
)

func TestOverlayGoals(t *testing.T) {
	var o overlays
	now := time.Date(2026, 11, 19, 20, 0, 0, 0, time.UTC)
	goal := func(kind string, cur int) twitch.ChannelEvent {
		return twitch.ChannelEvent{Kind: kind, Goal: &twitch.Goal{ID: "g1", Type: twitch.GoalFollow, Current: cur, Target: 100}}
	}
	if g, _, gone := o.apply(goal(twitch.EvGoalBegin, 10), false, now); !g || !gone.IsZero() {
		t.Fatal("a running goal stays")
	}
	o.apply(goal(twitch.EvGoalProgress, 60), false, now)
	goals, _ := o.snapshot()
	if len(goals) != 1 || goals[0].Current != 60 {
		t.Fatalf("progress not applied: %+v", goals)
	}
	_, _, gone := o.apply(goal(twitch.EvGoalAchieved, 100), false, now)
	if !gone.Equal(now.Add(goalEndLinger)) {
		t.Fatalf("reached goal leaves at %v", gone)
	}
	if g, _ := o.prune(now.Add(goalEndLinger - time.Second)); g {
		t.Fatal("pruned too early")
	}
	if g, _ := o.prune(gone); !g || len(o.goals) != 0 {
		t.Fatal("not pruned when due")
	}
}

func TestOverlaySeedGoals(t *testing.T) {
	var o overlays
	now := time.Now()
	// A sample from "Test" is kept while it shows; Twitch's list replaces
	// the running goals.
	o.apply(twitch.ChannelEvent{Kind: twitch.EvGoalBegin, Goal: &twitch.Goal{ID: "sample-goal"}}, true, now)
	o.apply(twitch.ChannelEvent{Kind: twitch.EvGoalBegin, Goal: &twitch.Goal{ID: "old"}}, false, now)
	if !o.seedGoals([]twitch.Goal{{ID: "g2", Current: 3, Target: 9}}) {
		t.Fatal("change not reported")
	}
	goals, _ := o.snapshot()
	if len(goals) != 2 || goals[0].ID != "sample-goal" || goals[1].ID != "g2" {
		t.Fatalf("%+v", goals)
	}
	if o.seedGoals([]twitch.Goal{{ID: "g2", Current: 3, Target: 9}}) {
		t.Fatal("same list reported as a change")
	}
}

func TestOverlayVote(t *testing.T) {
	var o overlays
	now := time.Now()
	v := &twitch.Vote{ID: "p1", State: twitch.VoteActive, Options: []twitch.VoteOption{{ID: "a"}}}
	if _, changed, gone := o.apply(twitch.ChannelEvent{Kind: twitch.EvPollBegin, Vote: v}, false, now); !changed || !gone.IsZero() {
		t.Fatal("a running poll stays")
	}
	end := *v
	end.State = twitch.VoteEnded
	_, _, gone := o.apply(twitch.ChannelEvent{Kind: twitch.EvPollEnd, Vote: &end}, false, now)
	if !gone.Equal(now.Add(voteEndLinger)) {
		t.Fatalf("result leaves at %v", gone)
	}
	if _, cleared := o.prune(gone); !cleared {
		t.Fatal("result not cleared")
	}
	if _, v := o.snapshot(); v != nil {
		t.Fatal("vote still shown")
	}
	// Samples leave quickly even while "active".
	_, _, gone = o.apply(twitch.ChannelEvent{Kind: twitch.EvPollBegin, Vote: v}, true, now)
	if !gone.Equal(now.Add(testLinger)) {
		t.Fatalf("sample leaves at %v", gone)
	}
}

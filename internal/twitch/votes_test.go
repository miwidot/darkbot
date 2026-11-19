package twitch

import (
	"encoding/json"
	"testing"
	"time"
)

// Payloads are the reference's own examples (checked 2026-11-19).
const (
	pollEndExample = `{"id":"1243456","broadcaster_user_id":"1337","broadcaster_user_login":"cool_user","broadcaster_user_name":"Cool_User","title":"Aren't shoes just really hard socks?","choices":[{"id":"123","title":"Blue","bits_votes":50,"channel_points_votes":70,"votes":120},{"id":"124","title":"Yellow","bits_votes":100,"channel_points_votes":40,"votes":140},{"id":"125","title":"Green","bits_votes":10,"channel_points_votes":70,"votes":80}],"bits_voting":{"is_enabled":true,"amount_per_vote":10},"channel_points_voting":{"is_enabled":true,"amount_per_vote":10},"status":"completed","started_at":"2020-07-15T17:16:03.17106713Z","ended_at":"2020-07-15T17:16:11.17106713Z"}`
	// Note user_id as a number in top_predictors: darkbot doesn't read
	// top_predictors, so the example must still parse.
	predictionEndExample   = `{"id":"1243456","broadcaster_user_id":"1337","broadcaster_user_login":"cool_user","broadcaster_user_name":"Cool_User","title":"Aren't shoes just really hard socks?","winning_outcome_id":"12345","outcomes":[{"id":"12345","title":"Yeah!","color":"blue","users":2,"channel_points":15000,"top_predictors":[{"user_name":"Cool_User","user_login":"cool_user","user_id":"1234","channel_points_won":10000,"channel_points_used":500}]},{"id":"22435","title":"No!","users":2,"channel_points":200,"color":"pink","top_predictors":[{"user_name":"Cooler_User","user_login":"cooler_user","user_id":12345,"channel_points_won":null,"channel_points_used":100}]}],"status":"resolved","started_at":"2020-07-15T17:16:03.17106713Z","ended_at":"2020-07-15T17:16:11.17106713Z"}`
	predictionBeginExample = `{"id":"1243456","broadcaster_user_id":"1337","broadcaster_user_login":"cool_user","broadcaster_user_name":"Cool_User","title":"Aren't shoes just really hard socks?","outcomes":[{"id":"1243456","title":"Yeah!","color":"blue"},{"id":"2243456","title":"No!","color":"pink"}],"started_at":"2020-07-15T17:16:03.17106713Z","locks_at":"2020-07-15T17:21:03.17106713Z"}`
	goalEndExample         = `{"id":"12345-abc-678-defgh","broadcaster_user_id":"141981764","broadcaster_user_name":"TwitchDev","broadcaster_user_login":"twitchdev","type":"subscription","description":"Help me get partner!","is_achieved":false,"current_amount":180,"target_amount":220,"started_at":"2021-07-15T17:16:03.17106713Z","ended_at":"2020-07-16T17:16:03.17106713Z"}`
)

func TestPollEnd(t *testing.T) {
	ev, err := parseChannelEvent("channel.poll.end", json.RawMessage(pollEndExample))
	if err != nil {
		t.Fatal(err)
	}
	if ev.Kind != EvPollEnd || ev.Title != "Aren't shoes just really hard socks?" || ev.Winner != "Yellow" || ev.Amount != 140 {
		t.Fatalf("%+v", ev)
	}
	v := ev.Vote
	if v == nil || v.State != VoteEnded || v.Winner != "124" || len(v.Options) != 3 || v.Options[0].Votes != 120 {
		t.Fatalf("vote %+v", v)
	}
	// A terminated poll has no winner.
	ev, _ = parseChannelEvent("channel.poll.end", json.RawMessage(`{"id":"1","title":"t","choices":[{"id":"a","title":"A","votes":3}],"status":"terminated"}`))
	if ev.Vote.State != VoteCanceled || ev.Winner != "" {
		t.Fatalf("terminated: %+v %+v", ev, ev.Vote)
	}
}

func TestPrediction(t *testing.T) {
	ev, err := parseChannelEvent("channel.prediction.end", json.RawMessage(predictionEndExample))
	if err != nil {
		t.Fatal(err)
	}
	if ev.Kind != EvPredictionEnd || ev.Winner != "Yeah!" || ev.Amount != 15000 || ev.Vote.State != VoteEnded || ev.Vote.Options[1].Color != "pink" || ev.Vote.Options[0].Users != 2 {
		t.Fatalf("%+v %+v", ev, ev.Vote)
	}
	ev, err = parseChannelEvent("channel.prediction.begin", json.RawMessage(predictionBeginExample))
	if err != nil || ev.Kind != EvPredictionBegin || ev.Vote.State != VoteActive || !ev.Vote.Prediction ||
		!ev.Vote.Until.Equal(time.Date(2020, 7, 15, 17, 21, 3, 171067130, time.UTC)) {
		t.Fatalf("begin: %+v %v", ev.Vote, err)
	}
	ev, _ = parseChannelEvent("channel.prediction.lock", json.RawMessage(predictionBeginExample))
	if ev.Kind != EvPredictionLock || ev.Vote.State != VoteLocked {
		t.Fatalf("lock: %+v", ev.Vote)
	}
	ev, _ = parseChannelEvent("channel.prediction.end", json.RawMessage(`{"id":"1","title":"t","outcomes":[],"status":"canceled"}`))
	if ev.Vote.State != VoteCanceled || ev.Winner != "" {
		t.Fatalf("canceled: %+v", ev.Vote)
	}
}

func TestGoals(t *testing.T) {
	ev, err := parseChannelEvent("channel.goal.end", json.RawMessage(goalEndExample))
	if err != nil || ev.Kind != EvGoalEnd || ev.Goal.Current != 180 || ev.Goal.Target != 220 || ev.Title != "Help me get partner!" {
		t.Fatalf("not achieved: %+v %v", ev, err)
	}
	if !IsOverlayOnly(ev.Kind) {
		t.Fatal("a goal ended without reaching it is no alert")
	}
	ev, _ = parseChannelEvent("channel.goal.end", json.RawMessage(`{"id":"g","type":"follow","description":"","is_achieved":true,"current_amount":10,"target_amount":10}`))
	if ev.Kind != EvGoalAchieved || IsOverlayOnly(ev.Kind) || ev.GoalType != GoalFollow {
		t.Fatalf("achieved: %+v", ev)
	}
	ev, _ = parseChannelEvent("channel.goal.progress", json.RawMessage(`{"id":"g","type":"new_bit","current_amount":5,"target_amount":10}`))
	if ev.Kind != EvGoalProgress || !IsOverlayOnly(ev.Kind) {
		t.Fatalf("progress: %+v", ev)
	}
}

func TestLeader(t *testing.T) {
	v := Vote{Options: []VoteOption{{ID: "a", Votes: 2}, {ID: "b", Votes: 5}, {ID: "c", Votes: 5}}}
	if l, ok := v.Leader(); !ok || l.ID != "b" {
		t.Fatalf("%+v", l)
	}
	if _, ok := (Vote{}).Leader(); ok {
		t.Fatal("no options, no leader")
	}
}

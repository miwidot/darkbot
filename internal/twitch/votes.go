package twitch

import (
	"encoding/json"
	"time"
)

// Goals, polls and predictions (EventSub v1, checked 2026-11-19 against
// https://dev.twitch.tv/docs/eventsub/reference/):
//   - channel.goal.begin/progress/end (channel:read:goals): type, description
//     ("a maximum of 40 characters"), current_amount, target_amount;
//     is_achieved only in end.
//   - channel.poll.begin/progress/end (channel:read:polls): title, choices[]
//     with title and votes ("Total number of votes ... across all methods"),
//     ends_at; end has status completed, archived or terminated.
//   - channel.prediction.begin/progress/lock/end (channel:read:predictions):
//     title, outcomes[] with title, color (blue/pink), users, channel_points;
//     locks_at; end has winning_outcome_id and status resolved or canceled.
//
// Alerts come from begin, achieved/end and lock; the progress messages
// only feed the OBS overlays and never reach the feed.

// Alert kinds of this file.
const (
	EvGoalBegin       = "goal_begin"
	EvGoalAchieved    = "goal_achieved"
	EvPollBegin       = "poll_begin"
	EvPollEnd         = "poll_end"
	EvPredictionBegin = "prediction_begin"
	EvPredictionLock  = "prediction_lock"
	EvPredictionEnd   = "prediction_end"
)

// Overlay-only kinds: progress, and a goal that ended without being
// reached. Not in EventKinds; the app hands them to the overlays only.
const (
	EvGoalProgress       = "goal_progress"
	EvGoalEnd            = "goal_end"
	EvPollProgress       = "poll_progress"
	EvPredictionProgress = "prediction_progress"
)

// Vote states.
const (
	VoteActive = "active"
	VoteLocked = "locked" // prediction: no more bets
	VoteEnded  = "ended"
	// VoteCanceled: a poll terminated/archived or a prediction canceled
	// (no winner).
	VoteCanceled = "canceled"
)

// Vote is a running poll or prediction, for the overlay.
type Vote struct {
	ID         string       `json:"id"`
	Prediction bool         `json:"prediction"`
	Title      string       `json:"title"`
	Options    []VoteOption `json:"options"`
	State      string       `json:"state"`
	// Until: when a poll ends or a prediction locks (zero = unknown).
	Until time.Time `json:"until"`
	// Winner is the winning option's ID once it is decided ("" before).
	Winner string `json:"winner"`
}

// VoteOption is one poll choice or prediction outcome.
type VoteOption struct {
	ID    string `json:"id"`
	Title string `json:"title"`
	// Votes: poll votes, or the channel points bet on a prediction outcome.
	Votes int `json:"votes"`
	// Users: viewers who bet on a prediction outcome (0 for polls).
	Users int    `json:"users"`
	Color string `json:"color,omitempty"` // prediction: blue or pink
}

// Leader returns the option with the most votes (the first on a tie);
// ok is false without options.
func (v Vote) Leader() (VoteOption, bool) {
	if len(v.Options) == 0 {
		return VoteOption{}, false
	}
	best := v.Options[0]
	for _, o := range v.Options[1:] {
		if o.Votes > best.Votes {
			best = o
		}
	}
	return best, true
}

// option returns the option with that ID.
func (v Vote) option(id string) (VoteOption, bool) {
	for _, o := range v.Options {
		if o.ID == id {
			return o, true
		}
	}
	return VoteOption{}, false
}

// Poll and prediction end statuses (see above).
const (
	pollCompleted      = "completed"
	predictionResolved = "resolved"
)

func parsePoll(kind string) func(json.RawMessage) (ChannelEvent, error) {
	return func(raw json.RawMessage) (ChannelEvent, error) {
		v, err := decode[struct {
			ID      string `json:"id"`
			Title   string `json:"title"`
			Choices []struct {
				ID    string `json:"id"`
				Title string `json:"title"`
				Votes int    `json:"votes"`
			} `json:"choices"`
			Status string    `json:"status"`
			EndsAt time.Time `json:"ends_at"`
		}](raw)
		if err != nil {
			return ChannelEvent{}, err
		}
		vote := &Vote{ID: v.ID, Title: v.Title, State: VoteActive, Until: v.EndsAt}
		for _, c := range v.Choices {
			vote.Options = append(vote.Options, VoteOption{ID: c.ID, Title: c.Title, Votes: c.Votes})
		}
		ev := ChannelEvent{Kind: kind, Title: v.Title, Vote: vote}
		if kind == EvPollEnd {
			vote.State = VoteCanceled
			if v.Status == pollCompleted {
				vote.State = VoteEnded
				if lead, ok := vote.Leader(); ok {
					vote.Winner, ev.Winner, ev.Amount = lead.ID, lead.Title, lead.Votes
				}
			}
		}
		return ev, nil
	}
}

func parsePrediction(kind string) func(json.RawMessage) (ChannelEvent, error) {
	return func(raw json.RawMessage) (ChannelEvent, error) {
		v, err := decode[struct {
			ID       string `json:"id"`
			Title    string `json:"title"`
			Outcomes []struct {
				ID     string `json:"id"`
				Title  string `json:"title"`
				Color  string `json:"color"`
				Users  int    `json:"users"`
				Points int    `json:"channel_points"`
			} `json:"outcomes"`
			LocksAt time.Time `json:"locks_at"`
			Winner  string    `json:"winning_outcome_id"`
			Status  string    `json:"status"`
		}](raw)
		if err != nil {
			return ChannelEvent{}, err
		}
		vote := &Vote{ID: v.ID, Prediction: true, Title: v.Title, State: VoteActive, Until: v.LocksAt}
		for _, o := range v.Outcomes {
			vote.Options = append(vote.Options, VoteOption{ID: o.ID, Title: o.Title, Votes: o.Points, Users: o.Users, Color: o.Color})
		}
		ev := ChannelEvent{Kind: kind, Title: v.Title, Vote: vote}
		switch kind {
		case EvPredictionLock:
			vote.State = VoteLocked
		case EvPredictionEnd:
			vote.State = VoteCanceled
			if v.Status == predictionResolved {
				vote.State, vote.Winner = VoteEnded, v.Winner
				if w, ok := vote.option(v.Winner); ok {
					ev.Winner, ev.Amount = w.Title, w.Votes
				}
			}
		}
		return ev, nil
	}
}

func parseGoal(kind string) func(json.RawMessage) (ChannelEvent, error) {
	return func(raw json.RawMessage) (ChannelEvent, error) {
		v, err := decode[struct {
			ID          string `json:"id"`
			Type        string `json:"type"`
			Description string `json:"description"`
			Achieved    bool   `json:"is_achieved"`
			Current     int    `json:"current_amount"`
			Target      int    `json:"target_amount"`
		}](raw)
		if err != nil {
			return ChannelEvent{}, err
		}
		g := &Goal{ID: v.ID, Type: normalizeGoalType(v.Type), Description: v.Description, Current: v.Current, Target: v.Target}
		if kind == EvGoalEnd && v.Achieved {
			kind = EvGoalAchieved
		}
		return ChannelEvent{Kind: kind, Title: v.Description, Amount: v.Target, Goal: g, GoalType: g.Type}, nil
	}
}

// voteSpecs are the subscriptions of this file (appended to eventSpecs).
var voteSpecs = []eventSpec{
	{Type: "channel.goal.begin", Version: "1", Scope: ScopeReadGoals, condition: byBroadcaster, parse: parseGoal(EvGoalBegin)},
	{Type: "channel.goal.progress", Version: "1", Scope: ScopeReadGoals, condition: byBroadcaster, parse: parseGoal(EvGoalProgress)},
	{Type: "channel.goal.end", Version: "1", Scope: ScopeReadGoals, condition: byBroadcaster, parse: parseGoal(EvGoalEnd)},
	{Type: "channel.poll.begin", Version: "1", Scope: ScopeReadPolls, condition: byBroadcaster, parse: parsePoll(EvPollBegin)},
	{Type: "channel.poll.progress", Version: "1", Scope: ScopeReadPolls, condition: byBroadcaster, parse: parsePoll(EvPollProgress)},
	{Type: "channel.poll.end", Version: "1", Scope: ScopeReadPolls, condition: byBroadcaster, parse: parsePoll(EvPollEnd)},
	{Type: "channel.prediction.begin", Version: "1", Scope: ScopeReadPredictions, condition: byBroadcaster, parse: parsePrediction(EvPredictionBegin)},
	{Type: "channel.prediction.progress", Version: "1", Scope: ScopeReadPredictions, condition: byBroadcaster, parse: parsePrediction(EvPredictionProgress)},
	{Type: "channel.prediction.lock", Version: "1", Scope: ScopeReadPredictions, condition: byBroadcaster, parse: parsePrediction(EvPredictionLock)},
	{Type: "channel.prediction.end", Version: "1", Scope: ScopeReadPredictions, condition: byBroadcaster, parse: parsePrediction(EvPredictionEnd)},
}

func init() { eventSpecs = append(eventSpecs, voteSpecs...) }

// IsOverlayOnly reports kinds that only feed the overlays (no feed line,
// no alert).
func IsOverlayOnly(kind string) bool {
	switch kind {
	case EvGoalProgress, EvGoalEnd, EvPollProgress, EvPredictionProgress:
		return true
	}
	return false
}

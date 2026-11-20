package main

import (
	"testing"

	"github.com/miwidot/darkbot/internal/twitch"
)

func TestCountChatLeavesOutBroadcasterAndBots(t *testing.T) {
	a := newChatApp(t)
	a.countChat(twitch.ChatMessage{ChatterID: "1", ChatterLogin: "me", ChatterName: "Me", IsBroadcaster: true})
	a.countChat(twitch.ChatMessage{ChatterID: "2", ChatterLogin: "streamelements", ChatterName: "StreamElements"})
	a.countChat(twitch.ChatMessage{ChatterID: "3", ChatterLogin: "anna", ChatterName: "Anna"})
	a.countChat(twitch.ChatMessage{ChatterID: "4", ChatterLogin: "mybot", ChatterName: "MyBot"})
	if err := a.SaveIgnoredChatters([]string{"@MyBot"}); err != nil {
		t.Fatal(err)
	}
	if got := a.cfg.Get().IgnoredChatters; len(got) != 1 || got[0] != "mybot" {
		t.Fatalf("stored %v", got)
	}
	in := a.GetInsights(7)
	if in.Session.Messages != 2 || in.Session.Chatters != 1 || in.Session.TopChatters[0].Name != "Anna" {
		t.Fatalf("session %+v", in.Session)
	}
	if err := a.SaveIgnoredChatters([]string{"not valid!"}); err == nil {
		t.Fatal("an invalid name must be refused")
	}
}

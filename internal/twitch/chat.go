package twitch

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"unicode/utf8"
)

// Chat via EventSub + Helix (verified 2026-11-17):
//   - Receive: https://dev.twitch.tv/docs/eventsub/reference/chat
//     channel.chat.message v1, condition broadcaster_user_id + user_id (the
//     user to read chat as; we read as the broadcaster), scope user:read:chat.
//   - Send: https://dev.twitch.tv/docs/api/reference/chat (Send Chat Message)
//     POST /chat/messages, scope user:write:chat, sender_id must be the token
//     user, message max 500 characters. A 200 can still mean "dropped"
//     (is_sent=false with drop_reason, e.g. automod or slow mode).
//   - Rate limit: 100 messages per 30 s for the broadcaster
//     (https://dev.twitch.tv/docs/chat/).
const (
	chatMessageType = "channel.chat.message"
	// MaxChatMessage is Twitch's limit for one chat message, in characters.
	MaxChatMessage = 500
	// badgeModerator is the badge set_id Twitch uses for moderators (as in
	// the payload example of the EventSub reference). The docs list no other
	// moderator-like set_id (checked 2026-11-17), so only this one counts.
	badgeModerator = "moderator"
	// badgeVIP and badgeSubscriber are the set_ids of the VIP and
	// subscriber badges (Get Global Chat Badges example and the chat
	// message payload example, checked 2026-11-18). Twitch says to read
	// badges from each message, not to store them.
	badgeVIP        = "vip"
	badgeSubscriber = "subscriber"
	// badgeFounder: Twitch's badge for a channel's first subscribers,
	// shown instead of the subscriber badge. It is not in the docs'
	// examples; it is one of Twitch's global badge sets (Get Global Chat
	// Badges) and founders are subscribers, so it counts as one.
	badgeFounder = "founder"
)

// ChatMessage is one chat message from the broadcaster's channel.
type ChatMessage struct {
	ID            string
	BroadcasterID string
	ChatterID     string
	ChatterLogin  string
	ChatterName   string
	Text          string
	IsBroadcaster bool
	IsModerator   bool
	IsVIP         bool
	IsSubscriber  bool
}

type chatEvent struct {
	BroadcasterUserID string `json:"broadcaster_user_id"`
	ChatterUserID     string `json:"chatter_user_id"`
	ChatterUserLogin  string `json:"chatter_user_login"`
	ChatterUserName   string `json:"chatter_user_name"`
	MessageID         string `json:"message_id"`
	Message           struct {
		Text string `json:"text"`
	} `json:"message"`
	Badges []struct {
		SetID string `json:"set_id"`
	} `json:"badges"`
	// SourceBroadcasterUserID is set when the message was sent in another
	// channel of a Shared Chat session.
	SourceBroadcasterUserID *string `json:"source_broadcaster_user_id"`
}

// parseChatEvent turns the notification event into a ChatMessage. ok is
// false for messages that came from another channel through Shared Chat:
// viewers of a partner channel must not control this streamer's music.
func parseChatEvent(raw json.RawMessage) (ChatMessage, bool, error) {
	var ev chatEvent
	if err := json.Unmarshal(raw, &ev); err != nil {
		return ChatMessage{}, false, err
	}
	if src := ev.SourceBroadcasterUserID; src != nil && *src != "" && *src != ev.BroadcasterUserID {
		return ChatMessage{}, false, nil
	}
	m := ChatMessage{
		ID:            ev.MessageID,
		BroadcasterID: ev.BroadcasterUserID,
		ChatterID:     ev.ChatterUserID,
		ChatterLogin:  ev.ChatterUserLogin,
		ChatterName:   ev.ChatterUserName,
		Text:          ev.Message.Text,
		IsBroadcaster: ev.ChatterUserID != "" && ev.ChatterUserID == ev.BroadcasterUserID,
	}
	for _, b := range ev.Badges {
		switch b.SetID {
		case badgeModerator:
			m.IsModerator = true
		case badgeVIP:
			m.IsVIP = true
		case badgeSubscriber, badgeFounder:
			m.IsSubscriber = true
		}
	}
	return m, true, nil
}

// SubscribeChat creates the channel.chat.message subscription on the given
// EventSub WebSocket session, reading chat as the broadcaster.
func (h *Helix) SubscribeChat(ctx context.Context, sessionID, broadcasterID string) error {
	return h.subscribe(ctx, sessionID, chatMessageType, "1", map[string]string{
		"broadcaster_user_id": broadcasterID,
		"user_id":             broadcasterID,
	})
}

// SendChatMessage posts text in the broadcaster's chat as the broadcaster.
// replyTo (optional) makes it a threaded reply. Text longer than Twitch
// allows is shortened instead of rejected.
func (h *Helix) SendChatMessage(ctx context.Context, broadcasterID, text, replyTo string) error {
	body := map[string]any{
		"broadcaster_id": broadcasterID,
		"sender_id":      broadcasterID,
		"message":        TruncateChat(text),
	}
	if replyTo != "" {
		body["reply_parent_message_id"] = replyTo
	}
	var out struct {
		Data []struct {
			IsSent     bool `json:"is_sent"`
			DropReason *struct {
				Code    string `json:"code"`
				Message string `json:"message"`
			} `json:"drop_reason"`
		} `json:"data"`
	}
	if err := h.do(ctx, http.MethodPost, "/chat/messages", nil, body, &out); err != nil {
		return err
	}
	if len(out.Data) > 0 && !out.Data[0].IsSent {
		if d := out.Data[0].DropReason; d != nil {
			return fmt.Errorf("chat message dropped by Twitch: %s (%s)", d.Message, d.Code)
		}
		return fmt.Errorf("chat message dropped by Twitch")
	}
	return nil
}

// TruncateChat shortens text to MaxChatMessage characters (runes), ending
// with "…" when cut.
func TruncateChat(text string) string {
	if utf8.RuneCountInString(text) <= MaxChatMessage {
		return text
	}
	r := []rune(text)
	return string(r[:MaxChatMessage-1]) + "…"
}

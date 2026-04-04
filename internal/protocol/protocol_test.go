package protocol

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/laney/modeloff/internal/domain"
)

func TestFromMessage(t *testing.T) {
	at := time.Date(2025, 6, 15, 12, 0, 0, 0, time.UTC)

	got := FromMessage(domain.Message{
		Channel: "#general",
		From:    "alice",
		Body:    "hello",
		SentAt:  at,
	})

	require.Equal(t, IRCMessage{
		Kind:   KindPrivMsg,
		From:   "alice",
		Target: "#general",
		Body:   "hello",
		At:     at,
	}, got)
}

func TestFromJoinEvent(t *testing.T) {
	at := time.Date(2025, 6, 15, 12, 0, 0, 0, time.UTC)

	got := FromJoinEvent(domain.JoinEvent{
		Channel: "#general",
		Nick:    "alice",
		At:      at,
	})

	require.Equal(t, IRCMessage{
		Kind:   KindJoin,
		From:   "alice",
		Target: "#general",
		At:     at,
	}, got)
}

func TestFromPartEvent(t *testing.T) {
	at := time.Date(2025, 6, 15, 12, 0, 0, 0, time.UTC)

	got := FromPartEvent(domain.PartEvent{
		Channel: "#general",
		Nick:    "alice",
		At:      at,
	})

	require.Equal(t, IRCMessage{
		Kind:   KindPart,
		From:   "alice",
		Target: "#general",
		At:     at,
	}, got)
}

func TestFromTopicChangeEvent(t *testing.T) {
	at := time.Date(2025, 6, 15, 12, 0, 0, 0, time.UTC)

	got := FromTopicChangeEvent(domain.TopicChangeEvent{
		Channel: "#general",
		Topic:   "Discussion",
		By:      "alice",
		At:      at,
	})

	require.Equal(t, IRCMessage{
		Kind:   KindTopic,
		From:   "alice",
		Target: "#general",
		Body:   "Discussion",
		At:     at,
	}, got)
}

func TestFromNickChangeEvent(t *testing.T) {
	at := time.Date(2025, 6, 15, 12, 0, 0, 0, time.UTC)

	got := FromNickChangeEvent(domain.NickChangeEvent{
		OldNick: "alice",
		NewNick: "ally",
		At:      at,
	})

	require.Equal(t, IRCMessage{
		Kind:   KindNick,
		From:   "alice",
		Target: "ally",
		At:     at,
	}, got)
}

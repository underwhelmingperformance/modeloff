package domain

import "testing"

// Compile-time assertions that every event type satisfies Event.
var _ Event = MessageEvent{}
var _ Event = JoinEvent{}
var _ Event = PartEvent{}
var _ Event = NickChangeEvent{}
var _ Event = TopicChangeEvent{}
var _ Event = ModelInvitedEvent{}
var _ Event = ModelKickedEvent{}
var _ Event = ModelReplyEvent{}
var _ Event = DMOpenedEvent{}
var _ Event = ConfigChangedEvent{}
var _ Event = ErrorEvent{}

func TestEventMarker(t *testing.T) {
	events := []Event{
		MessageEvent{},
		JoinEvent{},
		PartEvent{},
		NickChangeEvent{},
		TopicChangeEvent{},
		ModelInvitedEvent{},
		ModelKickedEvent{},
		ModelReplyEvent{},
		DMOpenedEvent{},
		ConfigChangedEvent{},
		ErrorEvent{},
	}

	for _, e := range events {
		e.eventMarker()
	}

	if len(events) != 11 {
		t.Fatalf("expected 11 event types, got %d", len(events))
	}
}

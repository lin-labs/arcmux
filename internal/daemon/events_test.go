package daemon

import (
	"testing"
	"time"
)

func TestEventBus_PublishSubscribe(t *testing.T) {
	bus := NewEventBus()
	defer bus.Close()

	ch, id := bus.Subscribe("")
	defer bus.Unsubscribe(id)

	event := Event{
		SessionID: "s-1",
		Type:      "state_changed",
		State:     "working",
		Timestamp: time.Now(),
	}

	bus.Publish(event)

	select {
	case received := <-ch:
		if received.SessionID != "s-1" {
			t.Errorf("SessionID = %q, want %q", received.SessionID, "s-1")
		}
		if received.Type != "state_changed" {
			t.Errorf("Type = %q, want %q", received.Type, "state_changed")
		}
	case <-time.After(1 * time.Second):
		t.Fatal("timeout waiting for event")
	}
}

func TestEventBus_SessionFilter(t *testing.T) {
	bus := NewEventBus()
	defer bus.Close()

	// Subscribe to specific session
	ch, id := bus.Subscribe("s-1")
	defer bus.Unsubscribe(id)

	// Publish event for different session
	bus.Publish(Event{SessionID: "s-2", Type: "state_changed"})

	// Publish event for subscribed session
	bus.Publish(Event{SessionID: "s-1", Type: "stuck_detected"})

	select {
	case received := <-ch:
		if received.SessionID != "s-1" {
			t.Errorf("got event for wrong session: %q", received.SessionID)
		}
		if received.Type != "stuck_detected" {
			t.Errorf("Type = %q, want %q", received.Type, "stuck_detected")
		}
	case <-time.After(1 * time.Second):
		t.Fatal("timeout waiting for event")
	}
}

func TestEventBus_Unsubscribe(t *testing.T) {
	bus := NewEventBus()
	defer bus.Close()

	ch, id := bus.Subscribe("")
	bus.Unsubscribe(id)

	// Channel should be closed
	_, ok := <-ch
	if ok {
		t.Error("channel should be closed after unsubscribe")
	}
}

func TestEventBus_Close(t *testing.T) {
	bus := NewEventBus()

	ch, _ := bus.Subscribe("")
	bus.Close()

	// Channel should be closed
	_, ok := <-ch
	if ok {
		t.Error("channel should be closed after bus close")
	}

	// Publish after close should not panic
	bus.Publish(Event{SessionID: "s-1", Type: "test"})
}

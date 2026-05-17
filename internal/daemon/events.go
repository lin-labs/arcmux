package daemon

import (
	"sync"
	"time"
)

// Event is a daemon-level event pushed to subscribers.
type Event struct {
	SessionID string
	Type      string
	State     string
	Message   string
	Timestamp time.Time
	Data      map[string]string
}

// EventBus manages event subscriptions.
type EventBus struct {
	mu      sync.RWMutex
	subs    map[int]*subscription
	nextID  int
	closed  bool
}

type subscription struct {
	ch        chan Event
	sessionID string // empty = all sessions
}

// NewEventBus creates an event bus.
func NewEventBus() *EventBus {
	return &EventBus{
		subs: make(map[int]*subscription),
	}
}

// Subscribe returns a channel that receives events.
// If sessionID is non-empty, only events for that session are delivered.
func (b *EventBus) Subscribe(sessionID string) (<-chan Event, int) {
	b.mu.Lock()
	defer b.mu.Unlock()

	id := b.nextID
	b.nextID++

	ch := make(chan Event, 64)
	b.subs[id] = &subscription{
		ch:        ch,
		sessionID: sessionID,
	}
	return ch, id
}

// Unsubscribe removes a subscription and closes its channel.
func (b *EventBus) Unsubscribe(id int) {
	b.mu.Lock()
	defer b.mu.Unlock()

	if sub, ok := b.subs[id]; ok {
		close(sub.ch)
		delete(b.subs, id)
	}
}

// Publish sends an event to all matching subscribers.
func (b *EventBus) Publish(event Event) {
	b.mu.RLock()
	defer b.mu.RUnlock()

	if b.closed {
		return
	}

	for _, sub := range b.subs {
		if sub.sessionID != "" && sub.sessionID != event.SessionID {
			continue
		}
		select {
		case sub.ch <- event:
		default:
			// Drop if subscriber can't keep up
		}
	}
}

// Close shuts down all subscriptions.
func (b *EventBus) Close() {
	b.mu.Lock()
	defer b.mu.Unlock()

	b.closed = true
	for id, sub := range b.subs {
		close(sub.ch)
		delete(b.subs, id)
	}
}

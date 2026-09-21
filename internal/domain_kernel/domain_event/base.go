// Package domain_event provides the base types every bounded context's domain_events build on.
package domain_event

import (
	"context"
	"time"

	"github.com/google/uuid"
)

// DomainEvent is the contract every domain event satisfies.
type DomainEvent interface {
	Name() string
	ToJson() (string, error)
	OccurredAt() time.Time
	EventID() string
}

// BaseDomainEvent is embedded by concrete events so they only implement Name() and ToJson().
type BaseDomainEvent struct {
	ID       string    `json:"eventId"`
	Occurred time.Time `json:"occurredAt"`
}

func NewBaseDomainEvent() BaseDomainEvent {
	return BaseDomainEvent{ID: uuid.NewString(), Occurred: time.Now()}
}

func (b BaseDomainEvent) EventID() string       { return b.ID }
func (b BaseDomainEvent) OccurredAt() time.Time { return b.Occurred }

// EventRecorder is embedded by aggregate roots to accumulate events raised by domain decisions.
//
// It is deliberately not goroutine-safe: an aggregate should never be mutated from
// multiple goroutines, and adding a lock here would only mask that design error.
type EventRecorder struct {
	pending []DomainEvent
}

// AddDomainEvent records an event. Called by the entity after a state change succeeds.
func (r *EventRecorder) AddDomainEvent(e DomainEvent) {
	r.pending = append(r.pending, e)
}

// GetAllPendingEvents drains the recorded events. Draining on read is what guarantees
// an event is never published twice.
func (r *EventRecorder) GetAllPendingEvents() []DomainEvent {
	if len(r.pending) == 0 {
		return nil
	}
	out := r.pending
	r.pending = nil
	return out
}

func (r *EventRecorder) HasPendingEvents() bool { return len(r.pending) > 0 }

// Publisher dispatches domain events. Implemented by AmqpBus in every real deployment;
// domain_services only ever see this interface, which is what lets them be tested
// without a broker.
type Publisher interface {
	Publish(ctx context.Context, events ...DomainEvent) error
}

// Handler consumes a single event.
//
// Delivery is at-least-once: redelivery after a failed attempt, after a reconnect, and
// after a consumer is killed mid-handling all happen in normal operation. Every handler
// must therefore be idempotent — handling the same event twice must not produce the
// side effect twice.
type Handler func(ctx context.Context, e DomainEvent) error

// NoopPublisher discards events; used in tests and in deployments with no broker wired up.
type NoopPublisher struct{}

func (NoopPublisher) Publish(context.Context, ...DomainEvent) error { return nil }

// Package outbox delivers ledger events at least once and deduplicates them on
// the consumer side.
package outbox

import (
	"fmt"
	"sync"

	"github.com/gwakko/cex-lab/ledger"
)

// Source is an ordered, append-only event log (the ledger's outbox).
type Source interface {
	EventsSince(offset int) []ledger.Event
}

// Handler processes one event.
type Handler func(ledger.Event) error

// Relay drains a Source into a Handler. The offset advances only after a
// successful delivery, so a handler error or a crash before the offset is
// saved leads to redelivery, never to loss.
type Relay struct {
	src     Source
	deliver Handler
	offset  int
}

// NewRelay starts relaying from offset (e.g. a persisted checkpoint).
func NewRelay(src Source, deliver Handler, offset int) *Relay {
	return &Relay{src: src, deliver: deliver, offset: offset}
}

// Drain delivers all pending events in order and stops at the first error.
func (r *Relay) Drain() (delivered int, err error) {
	for _, e := range r.src.EventsSince(r.offset) {
		if err := r.deliver(e); err != nil {
			return delivered, fmt.Errorf("outbox: deliver %s: %w", e.ID, err)
		}
		r.offset++
		delivered++
	}
	return delivered, nil
}

// Offset is the checkpoint to persist.
func (r *Relay) Offset() int { return r.offset }

// Inbox makes an at-least-once stream effectively-once by remembering which
// event IDs were handled. In a real service the "seen" mark and the handler's
// side effect must commit in one database transaction; here both are in memory.
type Inbox struct {
	mu     sync.Mutex
	seen   map[string]bool
	handle Handler
}

// NewInbox wraps h with deduplication by event ID.
func NewInbox(h Handler) *Inbox {
	return &Inbox{seen: map[string]bool{}, handle: h}
}

// Handle runs the handler once per event ID.
func (i *Inbox) Handle(e ledger.Event) error {
	i.mu.Lock()
	defer i.mu.Unlock()
	if i.seen[e.ID] {
		return nil
	}
	if err := i.handle(e); err != nil {
		return err
	}
	i.seen[e.ID] = true
	return nil
}

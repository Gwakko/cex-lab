// Package outbox delivers ledger events at least once and deduplicates them on
// the consumer side.
package outbox

import (
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/gwakko/cex-lab/ledger"
)

// Source is an ordered, append-only event log (the ledger's outbox).
type Source interface {
	EventsSince(offset int) []ledger.Event
}

// Handler processes one event.
type Handler func(ledger.Event) error

// Metrics receives relay and inbox measurements.
type Metrics interface {
	Published(result string) // "ok" or "error"
	Backlog(events int, oldestAge time.Duration)
	InboxDuplicate()
}

type nopMetrics struct{}

func (nopMetrics) Published(string)           {}
func (nopMetrics) Backlog(int, time.Duration) {}
func (nopMetrics) InboxDuplicate()            {}

type config struct {
	metrics Metrics
	log     *slog.Logger
	now     func() time.Time
}

// Option configures a Relay or an Inbox.
type Option func(*config)

// WithMetrics reports measurements to m.
func WithMetrics(m Metrics) Option { return func(c *config) { c.metrics = m } }

// WithLogger logs deliveries and duplicates.
func WithLogger(l *slog.Logger) Option { return func(c *config) { c.log = l } }

// WithClock sets the clock used for backlog age (tests).
func WithClock(now func() time.Time) Option { return func(c *config) { c.now = now } }

func newConfig(opts []Option) config {
	c := config{metrics: nopMetrics{}, log: slog.New(slog.DiscardHandler), now: time.Now}
	for _, o := range opts {
		o(&c)
	}
	return c
}

// Relay drains a Source into a Handler. The offset advances only after a
// successful delivery, so a handler error or a crash before the offset is
// saved leads to redelivery, never to loss.
type Relay struct {
	src     Source
	deliver Handler
	offset  int
	cfg     config
}

// NewRelay starts relaying from offset (e.g. a persisted checkpoint).
func NewRelay(src Source, deliver Handler, offset int, opts ...Option) *Relay {
	return &Relay{src: src, deliver: deliver, offset: offset, cfg: newConfig(opts)}
}

// Drain delivers all pending events in order and stops at the first error.
// It reports the remaining backlog either way.
func (r *Relay) Drain() (delivered int, err error) {
	defer r.reportBacklog()
	for _, e := range r.src.EventsSince(r.offset) {
		if err := r.deliver(e); err != nil {
			r.cfg.metrics.Published("error")
			r.cfg.log.Warn("outbox delivery failed", "event_id", e.ID, "trace_id", e.TraceID, "error", err)
			return delivered, fmt.Errorf("outbox: deliver %s: %w", e.ID, err)
		}
		r.cfg.metrics.Published("ok")
		r.cfg.log.Info("outbox event published", "event_id", e.ID, "type", e.Type, "idempotency_key", e.Key, "trace_id", e.TraceID)
		r.offset++
		delivered++
	}
	return delivered, nil
}

func (r *Relay) reportBacklog() {
	pending := r.src.EventsSince(r.offset)
	var age time.Duration
	if len(pending) > 0 {
		age = r.cfg.now().Sub(pending[0].Time)
	}
	r.cfg.metrics.Backlog(len(pending), age)
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
	cfg    config
}

// NewInbox wraps h with deduplication by event ID.
func NewInbox(h Handler, opts ...Option) *Inbox {
	return &Inbox{seen: map[string]bool{}, handle: h, cfg: newConfig(opts)}
}

// Handle runs the handler once per event ID.
func (i *Inbox) Handle(e ledger.Event) error {
	i.mu.Lock()
	defer i.mu.Unlock()
	if i.seen[e.ID] {
		i.cfg.metrics.InboxDuplicate()
		i.cfg.log.Info("inbox duplicate skipped", "event_id", e.ID, "trace_id", e.TraceID)
		return nil
	}
	if err := i.handle(e); err != nil {
		return err
	}
	i.seen[e.ID] = true
	return nil
}

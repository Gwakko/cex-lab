package instrument

import (
	"time"

	"github.com/gwakko/cex-lab/telemetry"
)

// Outbox implements outbox.Metrics.
type Outbox struct {
	backlog    *telemetry.Gauge
	oldest     *telemetry.Gauge
	published  *telemetry.Counter
	duplicates *telemetry.Counter
}

// NewOutbox registers the outbox and inbox metrics.
func NewOutbox(r *telemetry.Registry) *Outbox {
	return &Outbox{
		backlog:    r.Gauge("outbox_backlog", "Committed events not yet published."),
		oldest:     r.Gauge("outbox_oldest_age_seconds", "Age of the oldest unpublished event (0 when empty)."),
		published:  r.Counter("outbox_relay_publish_total", "Relay delivery attempts by result.", "result"),
		duplicates: r.Counter("inbox_duplicates_total", "Redelivered events skipped by the consumer inbox."),
	}
}

func (m *Outbox) Published(result string) { m.published.Inc(result) }
func (m *Outbox) InboxDuplicate()         { m.duplicates.Inc() }
func (m *Outbox) Backlog(events int, oldest time.Duration) {
	m.backlog.Set(float64(events))
	m.oldest.Set(oldest.Seconds())
}

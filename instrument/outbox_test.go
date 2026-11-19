package instrument

import (
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/gwakko/cex-lab/internal/events"
	"github.com/gwakko/cex-lab/internal/ledger"
	"github.com/gwakko/cex-lab/internal/ledger/adapters/memstore"
	"github.com/gwakko/cex-lab/internal/platform/outbox"
	"github.com/gwakko/cex-lab/internal/platform/telemetry"
)

func TestOutboxMetrics(t *testing.T) {
	reg := telemetry.NewRegistry()
	m := NewOutbox(reg)
	l := memstore.NewLedger()
	for i := range 4 {
		l.Apply(ledger.Deposit{Key: fmt.Sprint(i), User: "a", Asset: "BTC", Amount: 1})
	}
	oldest := l.EventsSince(0)[2].Time
	clock := func() time.Time { return oldest.Add(90 * time.Second) }

	inbox := outbox.NewInbox(func(events.Event) error { return nil }, outbox.WithMetrics(m))
	calls := 0
	flaky := func(e events.Event) error {
		if calls++; calls == 3 {
			return errors.New("broker down")
		}
		return inbox.Handle(e)
	}
	r := outbox.NewRelay(l, flaky, 0, outbox.WithMetrics(m), outbox.WithClock(clock))
	r.Drain()
	wantMetric(t, reg, 2, "outbox_relay_publish_total", "ok")
	wantMetric(t, reg, 1, "outbox_relay_publish_total", "error")
	wantMetric(t, reg, 2, "outbox_backlog")
	wantMetric(t, reg, 90, "outbox_oldest_age_seconds")

	r.Drain()
	wantMetric(t, reg, 0, "outbox_backlog")
	wantMetric(t, reg, 0, "outbox_oldest_age_seconds")

	outbox.NewRelay(l, inbox.Handle, 0, outbox.WithMetrics(m)).Drain() // crash: checkpoint lost
	wantMetric(t, reg, 4, "inbox_duplicates_total")
}

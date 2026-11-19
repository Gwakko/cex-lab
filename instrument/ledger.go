// Package instrument adapts the domain packages' metric interfaces to a
// telemetry.Registry. Domain packages never import it: they depend only on
// their own small interfaces with no-op defaults.
package instrument

import (
	"time"

	"github.com/gwakko/cex-lab/internal/platform/telemetry"
)

// Ledger implements ledger.Metrics.
type Ledger struct {
	commands  *telemetry.Counter
	conflicts *telemetry.Counter
	entries   *telemetry.Counter
}

// NewLedger registers the ledger metrics.
func NewLedger(r *telemetry.Registry) *Ledger {
	return &Ledger{
		commands:  r.Counter("ledger_commands_total", "Ledger commands by type and result (applied, idempotent_replay, rejected, error).", "type", "result"),
		conflicts: r.Counter("ledger_idempotency_conflicts_total", "Idempotency keys reused with a different payload."),
		entries:   r.Counter("ledger_entries_written_total", "Ledger entries committed."),
	}
}

func (m *Ledger) CommandDone(op, result string) { m.commands.Inc(op, result) }
func (m *Ledger) IdempotencyConflict()          { m.conflicts.Inc() }
func (m *Ledger) EntriesWritten(n int)          { m.entries.Add(float64(n)) }

// Journal implements journal.Observer for one named journal.
type Journal struct {
	name      string
	fsync     *telemetry.Histogram
	bytes     *telemetry.Gauge
	truncated *telemetry.Counter
}

// NewJournal registers journal metrics labelled with name (for example
// "ledger" or "engine_BTC-USDT"; one per file, so cardinality stays bounded).
func NewJournal(r *telemetry.Registry, name string) *Journal {
	return &Journal{
		name:      name,
		fsync:     r.Histogram("journal_fsync_duration_seconds", "Duration of one journal write plus fsync.", telemetry.LatencyBuckets, "journal"),
		bytes:     r.Gauge("journal_bytes", "Journal file size in bytes.", "journal"),
		truncated: r.Counter("journal_truncated_records_total", "Torn trailing records dropped on recovery.", "journal"),
	}
}

func (m *Journal) Synced(d time.Duration, size int64) {
	m.fsync.Observe(d.Seconds(), m.name)
	m.bytes.Set(float64(size), m.name)
}

func (m *Journal) Truncated(records int) { m.truncated.Add(float64(records), m.name) }

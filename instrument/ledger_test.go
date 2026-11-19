package instrument

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gwakko/cex-lab/internal/platform/telemetry"
	"github.com/gwakko/cex-lab/internal/platform/trace"
	"github.com/gwakko/cex-lab/ledger"
)

func metric(t *testing.T, r *telemetry.Registry, name string, labels ...string) float64 {
	t.Helper()
	v, _ := r.Get(name, labels...)
	return v
}

func wantMetric(t *testing.T, r *telemetry.Registry, want float64, name string, labels ...string) {
	t.Helper()
	if got := metric(t, r, name, labels...); got != want {
		t.Errorf("%s%v = %v, want %v", name, labels, got, want)
	}
}

// logLines decodes JSON log output.
func logLines(t *testing.T, buf *bytes.Buffer) []map[string]any {
	t.Helper()
	var out []map[string]any
	for _, line := range strings.Split(strings.TrimSpace(buf.String()), "\n") {
		if line == "" {
			continue
		}
		var m map[string]any
		if err := json.Unmarshal([]byte(line), &m); err != nil {
			t.Fatalf("log line is not JSON: %q", line)
		}
		out = append(out, m)
	}
	return out
}

func TestLedgerMetricsAndLogs(t *testing.T) {
	reg := telemetry.NewRegistry()
	var logs bytes.Buffer
	l := ledger.New(ledger.WithMetrics(NewLedger(reg)), ledger.WithLogger(slog.New(slog.NewJSONHandler(&logs, nil))))
	ctx := trace.WithID(context.Background(), "trace-abc")

	dep := ledger.Deposit{Key: "dep-1", User: "alice", Asset: "BTC", Amount: 100}
	if _, _, err := l.ApplyContext(ctx, dep); err != nil {
		t.Fatal(err)
	}
	l.ApplyContext(ctx, dep) // idempotent retry
	l.Apply(ledger.Deposit{Key: "dep-1", User: "alice", Asset: "BTC", Amount: 5})
	l.Apply(ledger.Hold{Key: "h-1", User: "alice", Asset: "BTC", Amount: 1_000})

	wantMetric(t, reg, 1, "ledger_commands_total", "deposit", ledger.ResultApplied)
	wantMetric(t, reg, 1, "ledger_commands_total", "deposit", ledger.ResultReplay)
	wantMetric(t, reg, 1, "ledger_commands_total", "deposit", ledger.ResultRejected)
	wantMetric(t, reg, 1, "ledger_commands_total", "hold", ledger.ResultRejected)
	wantMetric(t, reg, 1, "ledger_idempotency_conflicts_total")
	wantMetric(t, reg, 2, "ledger_entries_written_total")

	lines := logLines(t, &logs)
	commit := lines[0]
	if commit["msg"] != "ledger tx committed" || commit["trace_id"] != "trace-abc" ||
		commit["idempotency_key"] != "dep-1" || commit["event_id"] != "evt-1" {
		t.Fatalf("commit log %v", commit)
	}
	if ev := l.EventsSince(0)[0]; ev.TraceID != "trace-abc" || ev.Time.IsZero() {
		t.Fatalf("event must carry trace id and time: %+v", ev)
	}
}

func TestInvariantErrorNamesTheInvariant(t *testing.T) {
	err := ledger.Violation("non_negative_balance", "x is %d", -1)
	var ie *ledger.InvariantError
	if !errors.As(err, &ie) || ie.Invariant != "non_negative_balance" {
		t.Fatalf("%v", err)
	}
}

func TestJournalMetrics(t *testing.T) {
	reg := telemetry.NewRegistry()
	path := filepath.Join(t.TempDir(), "ledger.journal")
	fs, err := ledger.OpenFileStore(path, NewJournal(reg, "ledger"))
	if err != nil {
		t.Fatal(err)
	}
	l, _ := ledger.Open(fs)
	l.Apply(ledger.Deposit{Key: "d1", User: "a", Asset: "BTC", Amount: 1})
	l.Apply(ledger.Deposit{Key: "d2", User: "a", Asset: "BTC", Amount: 1})
	fs.Close()

	st, _ := os.Stat(path)
	wantMetric(t, reg, 2, "journal_fsync_duration_seconds", "ledger")
	wantMetric(t, reg, float64(st.Size()), "journal_bytes", "ledger")

	f, _ := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0)
	f.WriteString(`{"Seq":3,"Ke`) // torn write
	f.Close()
	fs, _ = ledger.OpenFileStore(path, NewJournal(reg, "ledger"))
	defer fs.Close()
	if _, err := ledger.Open(fs); err != nil {
		t.Fatal(err)
	}
	wantMetric(t, reg, 1, "journal_truncated_records_total", "ledger")
	if fs.Err() != nil {
		t.Fatalf("healthy journal reports %v", fs.Err())
	}
}

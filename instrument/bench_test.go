package instrument

import (
	"strconv"
	"testing"

	"github.com/gwakko/cex-lab/internal/ledger"
	"github.com/gwakko/cex-lab/internal/ledger/adapters/memstore"
	"github.com/gwakko/cex-lab/internal/platform/telemetry"
)

// BenchmarkLedgerApply compares the no-op recorder with the real registry.
func BenchmarkLedgerApply(b *testing.B) {
	for _, bc := range []struct {
		name string
		opts []ledger.Option
	}{
		{"nop", nil},
		{"instrumented", []ledger.Option{ledger.WithMetrics(NewLedger(telemetry.NewRegistry()))}},
	} {
		b.Run(bc.name, func(b *testing.B) {
			l := memstore.NewLedger(bc.opts...)
			i := 0
			for b.Loop() {
				i++
				l.Apply(ledger.Deposit{Key: strconv.Itoa(i), User: "u", Asset: "BTC", Amount: 1})
			}
		})
	}
}

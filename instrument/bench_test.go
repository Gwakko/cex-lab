package instrument

import (
	"strconv"
	"testing"

	"github.com/gwakko/cex-lab/ledger"
	"github.com/gwakko/cex-lab/telemetry"
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
			l := ledger.New(bc.opts...)
			i := 0
			for b.Loop() {
				i++
				l.Apply(ledger.Deposit{Key: strconv.Itoa(i), User: "u", Asset: "BTC", Amount: 1})
			}
		})
	}
}

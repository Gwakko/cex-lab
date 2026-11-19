package instrument_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/gwakko/cex-lab/internal/app/instrument"
	"github.com/gwakko/cex-lab/internal/custody"
	"github.com/gwakko/cex-lab/internal/custody/adapters/fakechain"
	"github.com/gwakko/cex-lab/internal/custody/adapters/memcursor"
	"github.com/gwakko/cex-lab/internal/ledger"
	"github.com/gwakko/cex-lab/internal/ledger/adapters/memstore"
	"github.com/gwakko/cex-lab/internal/platform/telemetry"
)

func newWatcher(t *testing.T, reg *telemetry.Registry, fc *fakechain.Chain, l *ledger.Ledger, confirmations uint64) *custody.Watcher {
	t.Helper()
	credit := func(ctx context.Context, d custody.Deposit) error {
		_, _, err := l.ApplyContext(ctx, ledger.Deposit{Key: "chain:" + d.Key(), User: "alice", Asset: ledger.Asset(d.Asset), Amount: d.Amount})
		return err
	}
	w, err := custody.NewWatcher(fc, &memcursor.Store{}, credit, custody.Config{
		Confirmations: confirmations, Watched: []string{"addr"}, StartBlock: 1, Metrics: instrument.NewChain(reg),
	})
	if err != nil {
		t.Fatal(err)
	}
	return w
}

func tr(tx string, amount int64) custody.Transfer {
	return custody.Transfer{TxHash: tx, To: "addr", Asset: "ETH", Amount: amount}
}

func TestChainMetricsReorgAndDuplicates(t *testing.T) {
	reg := telemetry.NewRegistry()
	fc := fakechain.New()
	w := newWatcher(t, reg, fc, memstore.NewLedger(), 3)
	ctx := context.Background()

	fc.Mine(tr("0x1", 10), tr("0x1", 10)) // block 1, duplicated log
	fc.Mine(tr("0x2", 20))                // block 2
	w.Poll(ctx)
	wantMetric(t, reg, 2, "chain_head_block")
	wantMetric(t, reg, 2, "chain_deposits_pending")
	wantMetric(t, reg, 0, "chain_watcher_lag_blocks")
	wantMetric(t, reg, 1, "chain_duplicate_deliveries_total")

	fc.Reorg(1, nil, []custody.Transfer{tr("0x2", 20)}) // 0x2 moves from block 2 to block 3
	w.Poll(ctx)
	wantMetric(t, reg, 1, "chain_reorgs_total")
	wantMetric(t, reg, 1, "chain_reorg_depth_blocks") // one observation...
	var page strings.Builder
	reg.WriteText(&page)
	if !strings.Contains(page.String(), `chain_reorg_depth_blocks_bucket{le="1"} 1`) {
		t.Fatalf("depth-1 reorg not in le=1 bucket:\n%s", page.String())
	}
	wantMetric(t, reg, 1, "chain_deposits_credited_total", "ETH") // 0x1 now has 3 confirmations
	wantMetric(t, reg, 1, "chain_confirmed_block")

	fc.MineEmpty(2)
	w.Poll(ctx)
	wantMetric(t, reg, 2, "chain_deposits_credited_total", "ETH")
	wantMetric(t, reg, 0, "chain_deposits_pending")
	wantMetric(t, reg, 0, "chain_reorg_below_confirmation_threshold_total")
}

func TestChainMetricsReorgBelowThreshold(t *testing.T) {
	reg := telemetry.NewRegistry()
	fc := fakechain.New()
	w := newWatcher(t, reg, fc, memstore.NewLedger(), 2)
	ctx := context.Background()
	fc.Mine(tr("0xbad", 5))
	fc.Mine()
	w.Poll(ctx)
	wantMetric(t, reg, 1, "chain_deposits_credited_total", "ETH")

	fc.Reorg(2, nil, nil, nil)
	if err := w.Poll(ctx); !errors.Is(err, custody.ErrConfirmedDepositReorged) {
		t.Fatalf("got %v", err)
	}
	w.Poll(ctx) // halted: must not count the same reorg again
	wantMetric(t, reg, 1, "chain_reorg_below_confirmation_threshold_total")
	wantMetric(t, reg, 1, "chain_reorgs_total")
	wantMetric(t, reg, 1, "chain_reorg_depth_blocks")
}

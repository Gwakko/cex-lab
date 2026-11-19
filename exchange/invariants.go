package exchange

import (
	"context"
	"fmt"

	"github.com/gwakko/cex-lab/ledger"
)

// Check asserts the cross-component invariants while holding every market
// lock, so it sees a quiescent state:
//   - ledger.Verify: entries balance per asset, balances match entries, no
//     negative user/system balance, in-flight == pending withdrawals;
//   - no market book is crossed;
//   - every locked balance equals the reserve of that user's resting orders.
func (x *Exchange) Check(ctx context.Context) error {
	for _, s := range x.symbols {
		m := x.markets[s]
		m.mu.Lock()
		defer m.mu.Unlock()
	}
	if err := x.ledger.Verify(); err != nil {
		return err
	}

	expected := map[ledger.Account]int64{}
	for _, s := range x.symbols {
		m := x.markets[s]
		snap, err := m.eng.Snapshot(ctx)
		if err != nil {
			return err
		}
		if snap.Crossed() {
			return fmt.Errorf("exchange: %s book crossed: bid %d >= ask %d", s, snap.Bids[0].Price, snap.Asks[0].Price)
		}
		for _, o := range append(snap.Bids, snap.Asks...) {
			asset, amount := m.restingReserve(o)
			expected[ledger.UserAccount(o.User, asset, ledger.Locked)] += amount
		}
	}

	balances := x.ledger.Balances()
	for acc, b := range balances {
		if acc.Kind == ledger.Locked && b != expected[acc] {
			return fmt.Errorf("exchange: %s = %d, resting orders reserve %d", acc, b, expected[acc])
		}
	}
	for acc, want := range expected {
		if balances[acc] != want {
			return fmt.Errorf("exchange: %s = %d, resting orders reserve %d", acc, balances[acc], want)
		}
	}
	return nil
}

package exchange

import (
	"context"
	"errors"
	"time"

	"github.com/gwakko/cex-lab/internal/ledger"
)

// Check asserts the cross-component invariants while holding every market
// lock, so it sees a quiescent state:
//   - ledger.Verify: entries balance per asset, balances match entries, no
//     negative user/system balance, in-flight == pending withdrawals;
//   - no market book is crossed;
//   - every locked balance equals the reserve of that user's resting orders.
//
// Violations are *ledger.InvariantError. Every run is reported to Metrics and
// remembered for LastCheckError (readiness).
func (x *Exchange) Check(ctx context.Context) error {
	err := x.check(ctx)
	violated := ""
	var ie *ledger.InvariantError
	switch {
	case errors.As(err, &ie):
		violated = ie.Invariant
	case err != nil:
		violated = "check_failed"
	}
	x.metrics.InvariantChecked(time.Now(), violated)
	if err != nil {
		x.log.Error("invariant violated", "invariant", violated, "error", err)
	}
	x.checkMu.Lock()
	x.lastCheck = err
	x.checkMu.Unlock()
	return err
}

// LastCheckError is the result of the most recent Check (nil before the first).
func (x *Exchange) LastCheckError() error {
	x.checkMu.Lock()
	defer x.checkMu.Unlock()
	return x.lastCheck
}

func (x *Exchange) check(ctx context.Context) error {
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
			return ledger.Violation("book_not_crossed", "%s bid %d >= ask %d", s, snap.Bids[0].Price, snap.Asks[0].Price)
		}
		for _, o := range append(snap.Bids, snap.Asks...) {
			asset, amount := m.restingReserve(o)
			expected[ledger.UserAccount(o.User, asset, ledger.Locked)] += amount
		}
	}

	balances := x.ledger.Balances()
	for acc, b := range balances {
		if acc.Kind == ledger.Locked && b != expected[acc] {
			return ledger.Violation("locked_equals_reserves", "%s = %d, resting orders reserve %d", acc, b, expected[acc])
		}
	}
	for acc, want := range expected {
		if balances[acc] != want {
			return ledger.Violation("locked_equals_reserves", "%s = %d, resting orders reserve %d", acc, balances[acc], want)
		}
	}
	return nil
}

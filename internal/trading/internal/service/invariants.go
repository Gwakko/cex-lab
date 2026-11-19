package service

import (
	"context"
	"errors"
	"time"
)

// Check asserts the cross-component invariants while holding every market
// lock, so it sees a quiescent state:
//   - LedgerPort.Verify: the ledger's own checks (entries balance per asset,
//     balances match entries, no negative balance, in-flight == pending);
//   - no market book is crossed;
//   - every locked balance equals the reserve of that user's resting orders.
//
// Violations implement InvariantName() string. Every run is reported to
// Metrics and remembered for LastCheckError (readiness).
func (x *Exchange) Check(ctx context.Context) error {
	err := x.check(ctx)
	violated := ""
	var named interface{ InvariantName() string }
	switch {
	case errors.As(err, &named):
		violated = named.InvariantName()
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

	expected := map[Holding]int64{}
	for _, s := range x.symbols {
		m := x.markets[s]
		snap, err := m.eng.Snapshot(ctx)
		if err != nil {
			return err
		}
		if snap.Crossed() {
			return violation("book_not_crossed", "%s bid %d >= ask %d", s, snap.Bids[0].Price, snap.Asks[0].Price)
		}
		for _, o := range append(snap.Bids, snap.Asks...) {
			asset, amount := m.restingReserve(o)
			expected[Holding{User: o.User, Asset: asset}] += amount
		}
	}

	balances := x.ledger.LockedBalances()
	for acc, b := range balances {
		if b != expected[acc] {
			return violation("locked_equals_reserves", "%s = %d, resting orders reserve %d", acc, b, expected[acc])
		}
	}
	for acc, want := range expected {
		if balances[acc] != want {
			return violation("locked_equals_reserves", "%s = %d, resting orders reserve %d", acc, balances[acc], want)
		}
	}
	return nil
}

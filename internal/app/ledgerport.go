package app

import (
	"context"
	"errors"

	"github.com/gwakko/cex-lab/internal/ledger"
	"github.com/gwakko/cex-lab/internal/trading"
)

// ledgerPort adapts the ledger module to trading.LedgerPort.
type ledgerPort struct{ l *ledger.Ledger }

// NewLedgerPort exposes a ledger to the trading module through its port.
func NewLedgerPort(l *ledger.Ledger) trading.LedgerPort { return ledgerPort{l} }

func (p ledgerPort) Hold(ctx context.Context, key, user, asset string, amount int64) (bool, error) {
	_, replayed, err := p.l.ApplyContext(ctx, ledger.Hold{Key: key, User: user, Asset: ledger.Asset(asset), Amount: amount})
	return replayed, translate(err)
}

func (p ledgerPort) Release(ctx context.Context, key, user, asset string, amount int64) error {
	_, _, err := p.l.ApplyContext(ctx, ledger.Release{Key: key, User: user, Asset: ledger.Asset(asset), Amount: amount})
	return translate(err)
}

func (p ledgerPort) Settle(ctx context.Context, s trading.Settlement) error {
	_, _, err := p.l.ApplyContext(ctx, ledger.Settle{
		Key: s.Key, Buyer: s.Buyer, Seller: s.Seller,
		Base: ledger.Asset(s.Base), Quote: ledger.Asset(s.Quote),
		BaseQty: s.BaseQty, QuoteQty: s.QuoteQty,
		BuyerFee: s.BuyerFee, SellerFee: s.SellerFee, BuyerRefund: s.BuyerRefund,
	})
	return translate(err)
}

func (p ledgerPort) Verify() error { return p.l.Verify() }

func (p ledgerPort) LockedBalances() map[trading.Holding]int64 {
	out := map[trading.Holding]int64{}
	for acc, b := range p.l.Balances() {
		if acc.Kind == ledger.Locked {
			out[trading.Holding{User: acc.Owner, Asset: string(acc.Asset)}] = b
		}
	}
	return out
}

// translate keeps the ledger error (and its message) and adds trading's
// sentinel, so both errors.Is(err, ledger.ErrInsufficientFunds) and
// errors.Is(err, trading.ErrInsufficientFunds) hold.
func translate(err error) error {
	if errors.Is(err, ledger.ErrInsufficientFunds) {
		return insufficientFunds{err}
	}
	return err
}

type insufficientFunds struct{ err error }

func (e insufficientFunds) Error() string   { return e.err.Error() }
func (e insufficientFunds) Unwrap() []error { return []error{e.err, trading.ErrInsufficientFunds} }

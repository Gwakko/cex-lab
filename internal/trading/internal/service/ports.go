package service

import (
	"context"
	"fmt"
)

// LedgerPort is what trading needs from a ledger. Trading defines it and the
// composition root adapts the ledger module to it, so trading never imports
// the ledger. Errors for a missing reserve must wrap ErrInsufficientFunds.
type LedgerPort interface {
	// Hold moves amount from available to locked. replayed reports that the
	// key was already committed (no new effect).
	Hold(ctx context.Context, key, user, asset string, amount int64) (replayed bool, err error)
	Release(ctx context.Context, key, user, asset string, amount int64) error
	Settle(ctx context.Context, s Settlement) error
	// Verify checks the ledger's own invariants. Violations should implement
	// InvariantName() string.
	Verify() error
	// LockedBalances returns every locked balance the ledger knows, including zeros.
	LockedBalances() map[Holding]int64
}

// Settlement books one trade atomically: the buyer pays QuoteQty (plus
// BuyerRefund returned to available) from locked quote, the seller delivers
// BaseQty from locked base, each side pays its fee in the asset it receives.
type Settlement struct {
	Key         string
	Buyer       string
	Seller      string
	Base        string
	Quote       string
	BaseQty     int64
	QuoteQty    int64
	BuyerFee    int64
	SellerFee   int64
	BuyerRefund int64
}

// Holding identifies a user's locked balance of one asset.
type Holding struct {
	User  string
	Asset string
}

func (h Holding) String() string { return h.User + "/" + h.Asset + "/locked" }

// InvariantError names the invariant that failed.
type InvariantError struct {
	Invariant string
	Detail    string
}

func (e *InvariantError) InvariantName() string { return e.Invariant }

func (e *InvariantError) Error() string {
	return "invariant " + e.Invariant + " violated: " + e.Detail
}

func violation(invariant, format string, args ...any) error {
	return &InvariantError{Invariant: invariant, Detail: fmt.Sprintf(format, args...)}
}

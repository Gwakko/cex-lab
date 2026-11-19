// Package ledger is the ledger module: a double-entry, append-only ledger with
// integer amounts, idempotent commands and a transactional outbox.
//
// This file is the module's public API. The implementation lives in
// internal/core and is reachable only through these names.
package ledger

import (
	"log/slog"

	"github.com/gwakko/cex-lab/internal/ledger/internal/core"
)

// Ledger is the module entry point. Its methods are Apply, ApplyContext,
// Balance, Balances, Supply, Withdrawal, EventsSince and Verify.
type Ledger = core.Ledger

// Open rebuilds a ledger by replaying every transaction in store.
func Open(store Store, opts ...Option) (*Ledger, error) { return core.Open(store, opts...) }

// Option configures a Ledger.
type Option = core.Option

// WithMetrics reports command outcomes to m.
func WithMetrics(m Metrics) Option { return core.WithMetrics(m) }

// WithLogger logs commits, replays and rejections.
func WithLogger(l *slog.Logger) Option { return core.WithLogger(l) }

// Accounts and amounts.
type (
	Asset   = core.Asset
	Kind    = core.Kind
	Account = core.Account
	Entry   = core.Entry
)

const (
	Available           = core.Available
	Locked              = core.Locked
	Fees                = core.Fees
	External            = core.External
	WithdrawalsInFlight = core.WithdrawalsInFlight
	System              = core.System
)

// UserAccount returns a user's account of the given kind.
func UserAccount(user string, asset Asset, kind Kind) Account {
	return core.UserAccount(user, asset, kind)
}

// SystemAccount returns an exchange-level account.
func SystemAccount(asset Asset, kind Kind) Account { return core.SystemAccount(asset, kind) }

// Commands. Each carries an idempotency key.
type (
	Command         = core.Command
	Deposit         = core.Deposit
	Hold            = core.Hold
	Release         = core.Release
	Settle          = core.Settle
	WithdrawRequest = core.WithdrawRequest
	WithdrawConfirm = core.WithdrawConfirm
	WithdrawFail    = core.WithdrawFail
)

// Results.
type (
	Tx              = core.Tx
	Withdrawal      = core.Withdrawal
	WithdrawalState = core.WithdrawalState
	InvariantError  = core.InvariantError
)

const (
	WithdrawalPending   = core.WithdrawalPending
	WithdrawalConfirmed = core.WithdrawalConfirmed
	WithdrawalFailed    = core.WithdrawalFailed

	ResultApplied  = core.ResultApplied
	ResultReplay   = core.ResultReplay
	ResultRejected = core.ResultRejected
	ResultError    = core.ResultError

	// EventSource is the Source of every event the ledger emits.
	EventSource = core.EventSource
)

// Violation builds an *InvariantError.
func Violation(invariant, format string, args ...any) error {
	return core.Violation(invariant, format, args...)
}

// Errors.
var (
	ErrInvalidCommand      = core.ErrInvalidCommand
	ErrInsufficientFunds   = core.ErrInsufficientFunds
	ErrOverflow            = core.ErrOverflow
	ErrUnknownWithdrawal   = core.ErrUnknownWithdrawal
	ErrWithdrawalState     = core.ErrWithdrawalState
	ErrIdempotencyConflict = core.ErrIdempotencyConflict
)

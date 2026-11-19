package core

import (
	"errors"
	"fmt"
	"math"
)

var (
	ErrInvalidCommand    = errors.New("ledger: invalid command")
	ErrInsufficientFunds = errors.New("ledger: insufficient funds")
	ErrOverflow          = errors.New("ledger: amount overflow")
	ErrUnknownWithdrawal = errors.New("ledger: unknown withdrawal")
	ErrWithdrawalState   = errors.New("ledger: withdrawal is not pending")
)

// Command is a request to change balances. Each one carries an idempotency key.
type Command interface {
	IdempotencyKey() string
	op() string
	plan(l *Ledger) (plan, error)
}

type plan struct {
	entries    []Entry
	withdrawal *Withdrawal
}

// legs builds entries, dropping zero amounts.
type legs []Entry

func (ls legs) add(a Account, amount int64) legs {
	if amount == 0 {
		return ls
	}
	return append(ls, Entry{a, amount})
}

// Deposit credits a user with money that arrived from outside.
type Deposit struct {
	Key    string
	User   string
	Asset  Asset
	Amount int64
}

func (c Deposit) IdempotencyKey() string { return c.Key }
func (Deposit) op() string               { return "deposit" }

func (c Deposit) plan(l *Ledger) (plan, error) {
	if err := checkUserAmount(c.User, c.Asset, c.Amount); err != nil {
		return plan{}, err
	}
	ext := SystemAccount(c.Asset, External)
	// External is the most negative account; bounding it bounds every sum.
	if l.balances[ext] < math.MinInt64+c.Amount {
		return plan{}, ErrOverflow
	}
	return plan{entries: legs{}.
		add(ext, -c.Amount).
		add(UserAccount(c.User, c.Asset, Available), c.Amount)}, nil
}

// Hold moves funds from available to locked (order reserve).
type Hold struct {
	Key    string
	User   string
	Asset  Asset
	Amount int64
}

func (c Hold) IdempotencyKey() string { return c.Key }
func (Hold) op() string               { return "hold" }

func (c Hold) plan(l *Ledger) (plan, error) {
	return l.move(c.User, c.Asset, c.Amount, Available, Locked)
}

// Release moves funds from locked back to available.
type Release struct {
	Key    string
	User   string
	Asset  Asset
	Amount int64
}

func (c Release) IdempotencyKey() string { return c.Key }
func (Release) op() string               { return "release" }

func (c Release) plan(l *Ledger) (plan, error) {
	return l.move(c.User, c.Asset, c.Amount, Locked, Available)
}

func (l *Ledger) move(user string, asset Asset, amount int64, from, to Kind) (plan, error) {
	if err := checkUserAmount(user, asset, amount); err != nil {
		return plan{}, err
	}
	src := UserAccount(user, asset, from)
	if l.balances[src] < amount {
		return plan{}, fmt.Errorf("%w: %s has %d, need %d", ErrInsufficientFunds, src, l.balances[src], amount)
	}
	return plan{entries: legs{}.add(src, -amount).add(UserAccount(user, asset, to), amount)}, nil
}

// Settle books one trade atomically. The buyer pays QuoteQty (+ BuyerRefund
// returned to available, e.g. price improvement) from locked quote; the seller
// delivers BaseQty from locked base. Each side pays its fee in the asset it
// receives.
type Settle struct {
	Key         string
	Buyer       string
	Seller      string
	Base        Asset
	Quote       Asset
	BaseQty     int64
	QuoteQty    int64
	BuyerFee    int64
	SellerFee   int64
	BuyerRefund int64
}

func (c Settle) IdempotencyKey() string { return c.Key }
func (Settle) op() string               { return "settle" }

func (c Settle) plan(l *Ledger) (plan, error) {
	if err := c.validate(); err != nil {
		return plan{}, err
	}
	buyerLocked := UserAccount(c.Buyer, c.Quote, Locked)
	sellerLocked := UserAccount(c.Seller, c.Base, Locked)
	if l.balances[buyerLocked] < c.QuoteQty || l.balances[buyerLocked]-c.QuoteQty < c.BuyerRefund {
		return plan{}, fmt.Errorf("%w: %s", ErrInsufficientFunds, buyerLocked)
	}
	if l.balances[sellerLocked] < c.BaseQty {
		return plan{}, fmt.Errorf("%w: %s", ErrInsufficientFunds, sellerLocked)
	}
	return plan{entries: legs{}.
		add(buyerLocked, -(c.QuoteQty+c.BuyerRefund)).
		add(UserAccount(c.Buyer, c.Quote, Available), c.BuyerRefund).
		add(UserAccount(c.Buyer, c.Base, Available), c.BaseQty-c.BuyerFee).
		add(SystemAccount(c.Base, Fees), c.BuyerFee).
		add(sellerLocked, -c.BaseQty).
		add(UserAccount(c.Seller, c.Quote, Available), c.QuoteQty-c.SellerFee).
		add(SystemAccount(c.Quote, Fees), c.SellerFee)}, nil
}

func (c Settle) validate() error {
	switch {
	case !validUser(c.Buyer) || !validUser(c.Seller):
		return fmt.Errorf("%w: settle needs buyer and seller", ErrInvalidCommand)
	case c.Base == "" || c.Quote == "" || c.Base == c.Quote:
		return fmt.Errorf("%w: settle needs two distinct assets", ErrInvalidCommand)
	case c.BaseQty <= 0 || c.QuoteQty <= 0:
		return fmt.Errorf("%w: settle quantities must be positive", ErrInvalidCommand)
	case c.BuyerFee < 0 || c.BuyerFee > c.BaseQty || c.SellerFee < 0 || c.SellerFee > c.QuoteQty:
		return fmt.Errorf("%w: fee out of range", ErrInvalidCommand)
	case c.BuyerRefund < 0:
		return fmt.Errorf("%w: negative refund", ErrInvalidCommand)
	}
	return nil
}

// WithdrawalState is the lifecycle of a withdrawal.
type WithdrawalState string

const (
	WithdrawalPending   WithdrawalState = "pending"
	WithdrawalConfirmed WithdrawalState = "confirmed"
	WithdrawalFailed    WithdrawalState = "failed"
)

// Withdrawal is a request to send funds out of the exchange.
type Withdrawal struct {
	ID     string
	User   string
	Asset  Asset
	Amount int64
	State  WithdrawalState
}

// WithdrawRequest moves funds from available to withdrawals in flight.
type WithdrawRequest struct {
	Key    string
	ID     string
	User   string
	Asset  Asset
	Amount int64
}

func (c WithdrawRequest) IdempotencyKey() string { return c.Key }
func (WithdrawRequest) op() string               { return "withdraw_request" }

func (c WithdrawRequest) plan(l *Ledger) (plan, error) {
	if c.ID == "" {
		return plan{}, fmt.Errorf("%w: withdrawal id required", ErrInvalidCommand)
	}
	if _, exists := l.withdrawals[c.ID]; exists {
		return plan{}, fmt.Errorf("%w: withdrawal %s already exists", ErrInvalidCommand, c.ID)
	}
	p, err := l.move(c.User, c.Asset, c.Amount, Available, Locked)
	if err != nil {
		return plan{}, err
	}
	p.entries[1].Account = SystemAccount(c.Asset, WithdrawalsInFlight)
	p.withdrawal = &Withdrawal{ID: c.ID, User: c.User, Asset: c.Asset, Amount: c.Amount, State: WithdrawalPending}
	return p, nil
}

// WithdrawConfirm records that the funds left the exchange.
type WithdrawConfirm struct {
	Key string
	ID  string
}

func (c WithdrawConfirm) IdempotencyKey() string { return c.Key }
func (WithdrawConfirm) op() string               { return "withdraw_confirm" }

func (c WithdrawConfirm) plan(l *Ledger) (plan, error) {
	return l.finishWithdrawal(c.ID, WithdrawalConfirmed)
}

// WithdrawFail returns in-flight funds to the user's available balance.
type WithdrawFail struct {
	Key string
	ID  string
}

func (c WithdrawFail) IdempotencyKey() string { return c.Key }
func (WithdrawFail) op() string               { return "withdraw_fail" }

func (c WithdrawFail) plan(l *Ledger) (plan, error) {
	return l.finishWithdrawal(c.ID, WithdrawalFailed)
}

func (l *Ledger) finishWithdrawal(id string, to WithdrawalState) (plan, error) {
	w, ok := l.withdrawals[id]
	if !ok {
		return plan{}, fmt.Errorf("%w: %s", ErrUnknownWithdrawal, id)
	}
	if w.State != WithdrawalPending {
		return plan{}, fmt.Errorf("%w: %s is %s", ErrWithdrawalState, id, w.State)
	}
	dest := SystemAccount(w.Asset, External)
	if to == WithdrawalFailed {
		dest = UserAccount(w.User, w.Asset, Available)
	}
	w.State = to
	return plan{
		entries:    legs{}.add(SystemAccount(w.Asset, WithdrawalsInFlight), -w.Amount).add(dest, w.Amount),
		withdrawal: &w,
	}, nil
}

func validUser(u string) bool { return u != "" && u != System }

func checkUserAmount(user string, asset Asset, amount int64) error {
	switch {
	case !validUser(user):
		return fmt.Errorf("%w: invalid user %q", ErrInvalidCommand, user)
	case asset == "":
		return fmt.Errorf("%w: asset required", ErrInvalidCommand)
	case amount <= 0:
		return fmt.Errorf("%w: amount must be positive, got %d", ErrInvalidCommand, amount)
	}
	return nil
}

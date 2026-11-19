package ledger

import (
	"fmt"
	"sync"
)

// Tx is one committed command: its entries and the event it emits.
type Tx struct {
	Seq        uint64
	Key        string
	Op         string
	Entries    []Entry
	Withdrawal *Withdrawal `json:",omitempty"`
}

// Ledger applies commands one at a time under a mutex.
type Ledger struct {
	mu          sync.Mutex
	txs         []Tx
	balances    map[Account]int64
	withdrawals map[string]Withdrawal
}

// New returns an empty ledger.
func New() *Ledger {
	return &Ledger{
		balances:    map[Account]int64{},
		withdrawals: map[string]Withdrawal{},
	}
}

// Apply validates cmd against current balances and commits it.
func (l *Ledger) Apply(cmd Command) (Tx, error) {
	l.mu.Lock()
	defer l.mu.Unlock()

	p, err := cmd.plan(l)
	if err != nil {
		return Tx{}, err
	}
	tx := Tx{
		Seq:        uint64(len(l.txs)) + 1,
		Key:        cmd.IdempotencyKey(),
		Op:         cmd.op(),
		Entries:    p.entries,
		Withdrawal: p.withdrawal,
	}
	if err := checkBalanced(tx.Entries); err != nil {
		return Tx{}, err
	}
	l.apply(tx)
	return tx, nil
}

func (l *Ledger) apply(tx Tx) {
	for _, e := range tx.Entries {
		l.balances[e.Account] += e.Amount
	}
	if tx.Withdrawal != nil {
		l.withdrawals[tx.Withdrawal.ID] = *tx.Withdrawal
	}
	l.txs = append(l.txs, tx)
}

func checkBalanced(entries []Entry) error {
	sums := map[Asset]int64{}
	for _, e := range entries {
		if e.Amount == 0 {
			return fmt.Errorf("ledger: zero-amount entry for %s", e.Account)
		}
		sums[e.Account.Asset] += e.Amount
	}
	for asset, s := range sums {
		if s != 0 {
			return fmt.Errorf("ledger: unbalanced transaction, %s sums to %d", asset, s)
		}
	}
	return nil
}

// Balance returns the derived balance of an account.
func (l *Ledger) Balance(a Account) int64 {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.balances[a]
}

// Balances returns a copy of every touched account's balance.
func (l *Ledger) Balances() map[Account]int64 {
	l.mu.Lock()
	defer l.mu.Unlock()
	out := make(map[Account]int64, len(l.balances))
	for a, b := range l.balances {
		out[a] = b
	}
	return out
}

// Supply is the amount of an asset deposited and not yet withdrawn.
func (l *Ledger) Supply(asset Asset) int64 {
	return -l.Balance(SystemAccount(asset, External))
}

// Withdrawal returns the current state of a withdrawal.
func (l *Ledger) Withdrawal(id string) (Withdrawal, bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	w, ok := l.withdrawals[id]
	return w, ok
}

// Verify recomputes balances from the entries and checks the ledger
// invariants: derived balances match, every asset sums to zero, no account
// except External is negative, and in-flight balances equal pending
// withdrawals.
func (l *Ledger) Verify() error {
	l.mu.Lock()
	defer l.mu.Unlock()

	recomputed := map[Account]int64{}
	sums := map[Asset]int64{}
	for _, tx := range l.txs {
		for _, e := range tx.Entries {
			recomputed[e.Account] += e.Amount
			sums[e.Account.Asset] += e.Amount
		}
	}
	for asset, s := range sums {
		if s != 0 {
			return fmt.Errorf("ledger: %s entries sum to %d", asset, s)
		}
	}
	for a, b := range l.balances {
		if recomputed[a] != b {
			return fmt.Errorf("ledger: %s balance %d, entries say %d", a, b, recomputed[a])
		}
		if b < 0 && !a.mayBeNegative() {
			return fmt.Errorf("ledger: %s is negative (%d)", a, b)
		}
	}
	inFlight := map[Asset]int64{}
	for _, w := range l.withdrawals {
		if w.State == WithdrawalPending {
			inFlight[w.Asset] += w.Amount
		}
	}
	for a, b := range l.balances {
		if a.Owner == System && a.Kind == WithdrawalsInFlight && b != inFlight[a.Asset] {
			return fmt.Errorf("ledger: %s is %d, pending withdrawals total %d", a, b, inFlight[a.Asset])
		}
	}
	return nil
}

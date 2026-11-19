package ledger

import (
	"errors"
	"fmt"
	"sync"
)

// ErrIdempotencyConflict means a key was reused for a different command.
var ErrIdempotencyConflict = errors.New("ledger: idempotency key reused with a different payload")

// Event is an outbox record. It is stored in the same transaction as the
// entries it describes, so a committed change always has its event.
type Event struct {
	ID    string
	Type  string
	Key   string
	TxSeq uint64
}

// Tx is one committed command: its entries and the event it emits.
type Tx struct {
	Seq         uint64
	Key         string
	Fingerprint string
	Op          string
	Entries     []Entry
	Withdrawal  *Withdrawal `json:",omitempty"`
	Events      []Event
}

// Ledger applies commands one at a time under a mutex. Its in-memory state is
// a projection of the Store and can always be rebuilt by replay.
type Ledger struct {
	mu          sync.Mutex
	store       Store
	txs         []Tx
	byKey       map[string]int
	balances    map[Account]int64
	withdrawals map[string]Withdrawal
	events      []Event
}

// New returns an empty ledger backed by a MemStore.
func New() *Ledger {
	l, _ := Open(&MemStore{}) // an empty MemStore cannot fail to load
	return l
}

// Open rebuilds a ledger by replaying every transaction in store.
func Open(store Store) (*Ledger, error) {
	l := &Ledger{
		store:       store,
		byKey:       map[string]int{},
		balances:    map[Account]int64{},
		withdrawals: map[string]Withdrawal{},
	}
	txs, err := store.Load()
	if err != nil {
		return nil, err
	}
	for _, tx := range txs {
		if err := l.checkReplayed(tx); err != nil {
			return nil, fmt.Errorf("ledger: replay tx %d: %w", tx.Seq, err)
		}
		l.apply(tx)
	}
	return l, nil
}

func (l *Ledger) checkReplayed(tx Tx) error {
	if want := uint64(len(l.txs)) + 1; tx.Seq != want {
		return fmt.Errorf("sequence gap: want %d", want)
	}
	if _, dup := l.byKey[tx.Key]; dup {
		return fmt.Errorf("duplicate key %q", tx.Key)
	}
	return checkBalanced(tx.Entries)
}

// Apply validates cmd against current balances and commits it.
//
// A key that was already committed returns the stored transaction with
// replayed=true and changes nothing; the same key with a different payload
// fails with ErrIdempotencyConflict. Rejected commands are not recorded, so a
// retry after "insufficient funds" is evaluated again.
func (l *Ledger) Apply(cmd Command) (tx Tx, replayed bool, err error) {
	l.mu.Lock()
	defer l.mu.Unlock()

	key := cmd.IdempotencyKey()
	if key == "" {
		return Tx{}, false, fmt.Errorf("%w: empty idempotency key", ErrInvalidCommand)
	}
	fp := fingerprint(cmd)
	if i, ok := l.byKey[key]; ok {
		if l.txs[i].Fingerprint != fp {
			return Tx{}, false, fmt.Errorf("%w: %s", ErrIdempotencyConflict, key)
		}
		return l.txs[i], true, nil
	}

	p, err := cmd.plan(l)
	if err != nil {
		return Tx{}, false, err
	}
	seq := uint64(len(l.txs)) + 1
	tx = Tx{
		Seq:         seq,
		Key:         key,
		Fingerprint: fp,
		Op:          cmd.op(),
		Entries:     p.entries,
		Withdrawal:  p.withdrawal,
		Events:      []Event{{ID: fmt.Sprintf("evt-%d", seq), Type: cmd.op(), Key: key, TxSeq: seq}},
	}
	if err := checkBalanced(tx.Entries); err != nil {
		return Tx{}, false, err
	}
	if err := l.store.Append(tx); err != nil {
		return Tx{}, false, fmt.Errorf("ledger: persist: %w", err)
	}
	l.apply(tx)
	return tx, false, nil
}

// fingerprint identifies a command's type and payload. Commands are flat
// structs, so %+v is a stable canonical form.
func fingerprint(cmd Command) string {
	return fmt.Sprintf("%T%+v", cmd, cmd)
}

func (l *Ledger) apply(tx Tx) {
	for _, e := range tx.Entries {
		l.balances[e.Account] += e.Amount
	}
	if tx.Withdrawal != nil {
		l.withdrawals[tx.Withdrawal.ID] = *tx.Withdrawal
	}
	l.byKey[tx.Key] = len(l.txs)
	l.txs = append(l.txs, tx)
	l.events = append(l.events, tx.Events...)
}

// EventsSince returns outbox events from position offset onwards.
func (l *Ledger) EventsSince(offset int) []Event {
	l.mu.Lock()
	defer l.mu.Unlock()
	if offset >= len(l.events) {
		return nil
	}
	return append([]Event(nil), l.events[offset:]...)
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

package ledger

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/gwakko/cex-lab/internal/events"
	"github.com/gwakko/cex-lab/internal/platform/trace"
)

// ErrIdempotencyConflict means a key was reused for a different command.
var ErrIdempotencyConflict = errors.New("ledger: idempotency key reused with a different payload")

// Tx is one committed command: its entries and the event it emits.
type Tx struct {
	Seq         uint64
	Key         string
	Fingerprint string
	Op          string
	TraceID     string `json:",omitempty"`
	Entries     []Entry
	Withdrawal  *Withdrawal `json:",omitempty"`
	Events      []events.Event
}

// Command results reported to Metrics.
const (
	ResultApplied  = "applied"
	ResultReplay   = "idempotent_replay"
	ResultRejected = "rejected"
	ResultError    = "error"
)

// Metrics receives ledger measurements. Labels are bounded: op and result.
type Metrics interface {
	CommandDone(op, result string)
	IdempotencyConflict()
	EntriesWritten(n int)
}

type nopMetrics struct{}

func (nopMetrics) CommandDone(string, string) {}
func (nopMetrics) IdempotencyConflict()       {}
func (nopMetrics) EntriesWritten(int)         {}

// Option configures a Ledger.
type Option func(*Ledger)

// WithMetrics reports command outcomes to m.
func WithMetrics(m Metrics) Option { return func(l *Ledger) { l.metrics = m } }

// WithLogger logs commits, replays and rejections to log.
func WithLogger(log *slog.Logger) Option { return func(l *Ledger) { l.log = log } }

// Ledger applies commands one at a time under a mutex. Its in-memory state is
// a projection of the Store and can always be rebuilt by replay.
type Ledger struct {
	mu          sync.Mutex
	store       Store
	txs         []Tx
	byKey       map[string]int
	balances    map[Account]int64
	withdrawals map[string]Withdrawal
	events      []events.Event
	metrics     Metrics
	log         *slog.Logger
}

// New returns an empty ledger backed by a MemStore.
func New(opts ...Option) *Ledger {
	l, _ := Open(&MemStore{}, opts...) // an empty MemStore cannot fail to load
	return l
}

// Open rebuilds a ledger by replaying every transaction in store.
func Open(store Store, opts ...Option) (*Ledger, error) {
	l := &Ledger{
		store:       store,
		byKey:       map[string]int{},
		balances:    map[Account]int64{},
		withdrawals: map[string]Withdrawal{},
		metrics:     nopMetrics{},
		log:         slog.New(slog.DiscardHandler),
	}
	for _, o := range opts {
		o(l)
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

// Apply is ApplyContext without a request context.
func (l *Ledger) Apply(cmd Command) (tx Tx, replayed bool, err error) {
	return l.ApplyContext(context.Background(), cmd)
}

// ApplyContext validates cmd against current balances and commits it. The
// trace id in ctx is stored on the transaction and its event.
//
// A key that was already committed returns the stored transaction with
// replayed=true and changes nothing; the same key with a different payload
// fails with ErrIdempotencyConflict. Rejected commands are not recorded, so a
// retry after "insufficient funds" is evaluated again.
func (l *Ledger) ApplyContext(ctx context.Context, cmd Command) (tx Tx, replayed bool, err error) {
	l.mu.Lock()
	defer l.mu.Unlock()

	op, key, traceID := cmd.op(), cmd.IdempotencyKey(), trace.ID(ctx)
	log := l.log.With("op", op, "idempotency_key", key, "trace_id", traceID)
	result := ResultApplied
	defer func() { l.metrics.CommandDone(op, result) }()

	if key == "" {
		result = ResultRejected
		return Tx{}, false, fmt.Errorf("%w: empty idempotency key", ErrInvalidCommand)
	}
	fp := fingerprint(cmd)
	if i, ok := l.byKey[key]; ok {
		if l.txs[i].Fingerprint != fp {
			result = ResultRejected
			l.metrics.IdempotencyConflict()
			log.Warn("idempotency conflict", "original_seq", l.txs[i].Seq)
			return Tx{}, false, fmt.Errorf("%w: %s", ErrIdempotencyConflict, key)
		}
		result = ResultReplay
		log.Info("idempotent replay", "seq", l.txs[i].Seq)
		return l.txs[i], true, nil
	}

	p, err := cmd.plan(l)
	if err != nil {
		result = ResultRejected
		log.Info("ledger command rejected", "error", err)
		return Tx{}, false, err
	}
	seq := uint64(len(l.txs)) + 1
	ev := events.Event{
		ID: fmt.Sprintf("evt-%d", seq), Source: EventSource, Type: op, Version: events.SchemaV1,
		Key: key, Seq: seq, TraceID: traceID, Time: time.Now().UTC(),
	}
	tx = Tx{
		Seq:         seq,
		Key:         key,
		Fingerprint: fp,
		Op:          op,
		TraceID:     traceID,
		Entries:     p.entries,
		Withdrawal:  p.withdrawal,
		Events:      []events.Event{ev},
	}
	if err := checkBalanced(tx.Entries); err != nil {
		result = ResultError
		log.Error("unbalanced transaction refused", "error", err)
		return Tx{}, false, err
	}
	if err := l.store.Append(tx); err != nil {
		result = ResultError
		log.Error("ledger persist failed", "error", err)
		return Tx{}, false, fmt.Errorf("ledger: persist: %w", err)
	}
	l.apply(tx)
	l.metrics.EntriesWritten(len(tx.Entries))
	log.Info("ledger tx committed", "seq", seq, "event_id", ev.ID, "entries", len(tx.Entries))
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

// EventSource is the Source of every event the ledger emits.
const EventSource = "ledger"

// EventsSince returns outbox events from position offset onwards.
func (l *Ledger) EventsSince(offset int) []events.Event {
	l.mu.Lock()
	defer l.mu.Unlock()
	if offset >= len(l.events) {
		return nil
	}
	return append([]events.Event(nil), l.events[offset:]...)
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
			return Violation("entries_sum_zero", "%s entries sum to %d", asset, s)
		}
	}
	for a, b := range l.balances {
		if recomputed[a] != b {
			return Violation("balance_matches_entries", "%s balance %d, entries say %d", a, b, recomputed[a])
		}
		if b < 0 && !a.mayBeNegative() {
			return Violation("non_negative_balance", "%s is negative (%d)", a, b)
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
			return Violation("in_flight_matches_pending", "%s is %d, pending withdrawals total %d", a, b, inFlight[a.Asset])
		}
	}
	return nil
}

// InvariantError names the invariant that failed, so it can be alerted on.
type InvariantError struct {
	Invariant string
	Detail    string
}

func (e *InvariantError) Error() string {
	return "invariant " + e.Invariant + " violated: " + e.Detail
}

// Violation builds an *InvariantError.
func Violation(invariant, format string, args ...any) error {
	return &InvariantError{Invariant: invariant, Detail: fmt.Sprintf(format, args...)}
}

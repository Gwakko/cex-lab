package custody_test

import (
	"context"
	"errors"
	"path/filepath"
	"sync"
	"testing"

	"github.com/gwakko/cex-lab/internal/custody"
	"github.com/gwakko/cex-lab/internal/custody/adapters/fakechain"
	"github.com/gwakko/cex-lab/internal/custody/adapters/filecursor"
	"github.com/gwakko/cex-lab/internal/custody/adapters/memcursor"
)

const depositAddr = "0xdeposit-alice"

// creditor is the DepositCreditor contract as the ledger implements it:
// idempotent by (txhash, log index). The real ledger wiring is tested in
// internal/app.
type creditor struct {
	mu      sync.Mutex
	seen    map[string]bool
	balance int64
}

func (c *creditor) credit(_ context.Context, d custody.Deposit) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.seen[d.Key()] {
		return nil
	}
	c.seen[d.Key()] = true
	c.balance += d.Amount
	return nil
}

// harness wires a watcher to a fake chain and an idempotent creditor.
type harness struct {
	t        *testing.T
	ctx      context.Context
	chain    *fakechain.Chain
	creditor *creditor
	store    custody.CursorStore
	cfg      custody.Config
	w        *custody.Watcher
}

func newHarness(t *testing.T, confirmations uint64, store custody.CursorStore) *harness {
	h := &harness{
		t:        t,
		ctx:      context.Background(),
		chain:    fakechain.New(),
		creditor: &creditor{seen: map[string]bool{}},
		store:    store,
		cfg:      custody.Config{Confirmations: confirmations, Watched: []string{depositAddr}, StartBlock: 1, HashWindow: 16},
	}
	h.restart()
	return h
}

func (h *harness) credit(ctx context.Context, d custody.Deposit) error {
	return h.creditor.credit(ctx, d)
}

func (h *harness) restart() {
	w, err := custody.NewWatcher(h.chain, h.store, h.credit, h.cfg)
	if err != nil {
		h.t.Fatal(err)
	}
	h.w = w
}

func (h *harness) poll() {
	h.t.Helper()
	if err := h.w.Poll(h.ctx); err != nil {
		h.t.Fatal(err)
	}
}

func (h *harness) balance() int64 {
	h.creditor.mu.Lock()
	defer h.creditor.mu.Unlock()
	return h.creditor.balance
}

func (h *harness) want(pending, credited int, balance int64) {
	h.t.Helper()
	if got := len(h.w.Pending()); got != pending {
		h.t.Errorf("pending = %d, want %d", got, pending)
	}
	if got := len(h.w.Credited()); got != credited {
		h.t.Errorf("credited = %d, want %d", got, credited)
	}
	if got := h.balance(); got != balance {
		h.t.Errorf("balance = %d, want %d", got, balance)
	}
	if h.t.Failed() {
		h.t.FailNow()
	}
}

func transfer(tx string, amount int64) custody.Transfer {
	return custody.Transfer{TxHash: tx, LogIndex: 0, To: depositAddr, Asset: "ETH", Amount: amount}
}

func TestDepositCreditedAfterConfirmations(t *testing.T) {
	h := newHarness(t, 3, &memcursor.Store{})
	h.chain.Mine(transfer("0x1", 100), custody.Transfer{TxHash: "0x2", To: "0xsomeone-else", Asset: "ETH", Amount: 7})
	h.poll()
	h.want(1, 0, 0) // 1 confirmation
	h.chain.Mine()
	h.poll()
	h.want(1, 0, 0) // 2 confirmations
	h.chain.Mine()
	h.poll()
	h.want(0, 1, 100) // 3 confirmations
	h.chain.MineEmpty(5)
	h.poll()
	h.want(0, 1, 100)
}

func TestReorgBeforeConfirmationDropsAndReAdds(t *testing.T) {
	h := newHarness(t, 3, &memcursor.Store{})
	h.chain.Mine(transfer("0xgone", 50))
	h.chain.Mine(transfer("0xmoved", 70))
	h.poll()
	h.want(2, 0, 0)

	// The new branch drops 0xgone entirely and includes 0xmoved one block later.
	if err := h.chain.Reorg(2, nil, nil, []custody.Transfer{transfer("0xmoved", 70)}); err != nil {
		t.Fatal(err)
	}
	h.poll()
	p := h.w.Pending()
	if len(p) != 1 || p[0].TxHash != "0xmoved" || p[0].Block != 3 {
		t.Fatalf("pending after reorg: %+v", p)
	}
	h.want(1, 0, 0)
	h.chain.MineEmpty(2)
	h.poll()
	h.want(0, 1, 70)
}

func TestReorgPastConfirmationRaisesAlert(t *testing.T) {
	h := newHarness(t, 2, &memcursor.Store{})
	h.chain.Mine(transfer("0xbad", 500))
	h.chain.Mine()
	h.poll()
	h.want(0, 1, 500)

	// A 2-block reorg orphans a block whose deposit was credited at 2 confirmations.
	if err := h.chain.Reorg(2, nil, nil, nil); err != nil {
		t.Fatal(err)
	}
	err := h.w.Poll(h.ctx)
	var alert *custody.ReorgAlert
	if !errors.Is(err, custody.ErrConfirmedDepositReorged) || !errors.As(err, &alert) {
		t.Fatalf("expected reorg alert, got %v", err)
	}
	if alert.Ancestor != 0 || len(alert.Deposits) != 1 || alert.Deposits[0].TxHash != "0xbad" {
		t.Fatalf("alert %+v", alert)
	}
	// No silent reversal or double credit, and the watcher stays halted.
	h.want(0, 1, 500)
	if err := h.w.Poll(h.ctx); !errors.Is(err, custody.ErrConfirmedDepositReorged) {
		t.Fatalf("watcher resumed after alert: %v", err)
	}
	if h.creditor.balance != 500 || len(h.creditor.seen) != 1 {
		t.Fatalf("creditor changed after alert: %+v", h.creditor)
	}
}

func TestReorgDeeperThanWindow(t *testing.T) {
	h := newHarness(t, 2, &memcursor.Store{})
	h.chain.MineEmpty(40)
	h.poll()
	branch := make([][]custody.Transfer, 41)
	if err := h.chain.Reorg(40, branch...); err != nil {
		t.Fatal(err)
	}
	if err := h.w.Poll(h.ctx); !errors.Is(err, custody.ErrReorgTooDeep) {
		t.Fatalf("got %v", err)
	}
}

func TestDuplicateDeliveryCreditsOnce(t *testing.T) {
	h := newHarness(t, 1, &memcursor.Store{})
	tr := transfer("0xdup", 30)
	h.chain.Mine(tr, tr) // the node reports the same log twice
	h.poll()
	h.poll()
	h.want(0, 1, 30)

	h.chain.Mine(tr) // a buggy source re-delivers it in a later block
	h.poll()
	h.want(0, 1, 30)

	// The creditor (the ledger in production) is the last line of defence:
	// crediting again is a no-op.
	if err := h.credit(h.ctx, h.w.Credited()[0]); err != nil {
		t.Fatal(err)
	}
	h.want(0, 1, 30)
}

func TestResumeFromPersistedCursor(t *testing.T) {
	h := newHarness(t, 3, filecursor.Store{Path: filepath.Join(t.TempDir(), "watcher.json")})
	h.chain.Mine(transfer("0xa", 10))
	h.poll()
	h.want(1, 0, 0)

	h.restart()
	h.want(1, 0, 0)
	h.chain.MineEmpty(2)
	h.poll()
	h.want(0, 1, 10)

	// A reorg that happens while we are down is still detected after restart.
	h.chain.Mine(transfer("0xb", 20))
	h.poll()
	h.restart()
	if err := h.chain.Reorg(1, nil, []custody.Transfer{transfer("0xb", 20)}); err != nil {
		t.Fatal(err)
	}
	h.poll()
	if p := h.w.Pending(); len(p) != 1 || p[0].Block != 5 {
		t.Fatalf("pending after offline reorg: %+v", p)
	}
}

func TestCrashAfterCreditBeforeSaveDoesNotDoubleCredit(t *testing.T) {
	store := &memcursor.Store{}
	h := newHarness(t, 2, store)
	h.chain.Mine(transfer("0xc", 40))
	h.poll()
	stale, _, _ := store.Load() // cursor as it was before the credit

	h.chain.Mine()
	h.poll()
	h.want(0, 1, 40)

	// Simulate losing the last save: restart from the stale cursor.
	store.Save(stale)
	h.restart()
	h.poll()
	h.want(0, 1, 40)
}

func TestConfigValidation(t *testing.T) {
	for _, cfg := range []custody.Config{{Confirmations: 0}, {Confirmations: 20, HashWindow: 10}} {
		if _, err := custody.NewWatcher(fakechain.New(), &memcursor.Store{}, nil, cfg); err == nil {
			t.Fatalf("config %+v accepted", cfg)
		}
	}
}

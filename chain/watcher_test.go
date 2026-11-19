package chain

import (
	"context"
	"errors"
	"path/filepath"
	"testing"

	"github.com/gwakko/cex-lab/ledger"
)

const depositAddr = "0xdeposit-alice"

// harness wires a watcher to a fake chain and a real ledger.
type harness struct {
	t      *testing.T
	ctx    context.Context
	chain  *FakeChain
	ledger *ledger.Ledger
	store  StateStore
	cfg    Config
	w      *Watcher
}

func newHarness(t *testing.T, confirmations uint64, store StateStore) *harness {
	h := &harness{
		t:      t,
		ctx:    context.Background(),
		chain:  NewFakeChain(),
		ledger: ledger.New(),
		store:  store,
		cfg:    Config{Confirmations: confirmations, Watched: []string{depositAddr}, StartBlock: 1, HashWindow: 16},
	}
	h.restart()
	return h
}

// credit books a deposit idempotently by (txhash, log index).
func (h *harness) credit(d Deposit) error {
	_, _, err := h.ledger.Apply(ledger.Deposit{
		Key: "chain:" + d.Key(), User: "alice", Asset: ledger.Asset(d.Asset), Amount: d.Amount,
	})
	return err
}

func (h *harness) restart() {
	w, err := NewWatcher(h.chain, h.store, h.credit, h.cfg)
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
	return h.ledger.Balance(ledger.UserAccount("alice", "ETH", ledger.Available))
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

func transfer(tx string, amount int64) Transfer {
	return Transfer{TxHash: tx, LogIndex: 0, To: depositAddr, Asset: "ETH", Amount: amount}
}

func TestDepositCreditedAfterConfirmations(t *testing.T) {
	h := newHarness(t, 3, &MemStateStore{})
	h.chain.Mine(transfer("0x1", 100), Transfer{TxHash: "0x2", To: "0xsomeone-else", Asset: "ETH", Amount: 7})
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
	h := newHarness(t, 3, &MemStateStore{})
	h.chain.Mine(transfer("0xgone", 50))
	h.chain.Mine(transfer("0xmoved", 70))
	h.poll()
	h.want(2, 0, 0)

	// The new branch drops 0xgone entirely and includes 0xmoved one block later.
	if err := h.chain.Reorg(2, nil, nil, []Transfer{transfer("0xmoved", 70)}); err != nil {
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
	h := newHarness(t, 2, &MemStateStore{})
	h.chain.Mine(transfer("0xbad", 500))
	h.chain.Mine()
	h.poll()
	h.want(0, 1, 500)

	// A 2-block reorg orphans a block whose deposit was credited at 2 confirmations.
	if err := h.chain.Reorg(2, nil, nil, nil); err != nil {
		t.Fatal(err)
	}
	err := h.w.Poll(h.ctx)
	var alert *ReorgAlert
	if !errors.Is(err, ErrConfirmedDepositReorged) || !errors.As(err, &alert) {
		t.Fatalf("expected reorg alert, got %v", err)
	}
	if alert.Ancestor != 0 || len(alert.Deposits) != 1 || alert.Deposits[0].TxHash != "0xbad" {
		t.Fatalf("alert %+v", alert)
	}
	// No silent reversal or double credit, and the watcher stays halted.
	h.want(0, 1, 500)
	if err := h.w.Poll(h.ctx); !errors.Is(err, ErrConfirmedDepositReorged) {
		t.Fatalf("watcher resumed after alert: %v", err)
	}
	if err := h.ledger.Verify(); err != nil {
		t.Fatal(err)
	}
}

func TestReorgDeeperThanWindow(t *testing.T) {
	h := newHarness(t, 2, &MemStateStore{})
	h.chain.MineEmpty(40)
	h.poll()
	branch := make([][]Transfer, 41)
	if err := h.chain.Reorg(40, branch...); err != nil {
		t.Fatal(err)
	}
	if err := h.w.Poll(h.ctx); !errors.Is(err, ErrReorgTooDeep) {
		t.Fatalf("got %v", err)
	}
}

func TestDuplicateDeliveryCreditsOnce(t *testing.T) {
	h := newHarness(t, 1, &MemStateStore{})
	tr := transfer("0xdup", 30)
	h.chain.Mine(tr, tr) // the node reports the same log twice
	h.poll()
	h.poll()
	h.want(0, 1, 30)

	h.chain.Mine(tr) // a buggy source re-delivers it in a later block
	h.poll()
	h.want(0, 1, 30)

	// The ledger is the last line of defence: crediting again is a no-op.
	if err := h.credit(h.w.Credited()[0]); err != nil {
		t.Fatal(err)
	}
	h.want(0, 1, 30)
}

func TestResumeFromPersistedCursor(t *testing.T) {
	h := newHarness(t, 3, FileStateStore{Path: filepath.Join(t.TempDir(), "watcher.json")})
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
	if err := h.chain.Reorg(1, nil, []Transfer{transfer("0xb", 20)}); err != nil {
		t.Fatal(err)
	}
	h.poll()
	if p := h.w.Pending(); len(p) != 1 || p[0].Block != 5 {
		t.Fatalf("pending after offline reorg: %+v", p)
	}
}

func TestCrashAfterCreditBeforeSaveDoesNotDoubleCredit(t *testing.T) {
	store := &MemStateStore{}
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
	for _, cfg := range []Config{{Confirmations: 0}, {Confirmations: 20, HashWindow: 10}} {
		if _, err := NewWatcher(NewFakeChain(), &MemStateStore{}, nil, cfg); err == nil {
			t.Fatalf("config %+v accepted", cfg)
		}
	}
}

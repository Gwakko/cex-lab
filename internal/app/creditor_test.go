package app

import (
	"context"
	"errors"
	"testing"

	"github.com/gwakko/cex-lab/internal/custody"
	"github.com/gwakko/cex-lab/internal/custody/adapters/fakechain"
	"github.com/gwakko/cex-lab/internal/custody/adapters/memcursor"
	"github.com/gwakko/cex-lab/internal/ledger"
	"github.com/gwakko/cex-lab/internal/ledger/adapters/memstore"
)

func TestDepositCreditorIsIdempotentOnTheLedger(t *testing.T) {
	l := memstore.NewLedger()
	credit := NewDepositCreditor(l, map[string]string{"addr": "alice"})
	ctx := context.Background()
	d := custody.Deposit{Transfer: custody.Transfer{TxHash: "0x1", To: "addr", Asset: "ETH", Amount: 30}, Block: 1}
	for range 3 {
		if err := credit(ctx, d); err != nil {
			t.Fatal(err)
		}
	}
	if got := l.Balance(ledger.UserAccount("alice", "ETH", ledger.Available)); got != 30 {
		t.Fatalf("balance = %d, want 30", got)
	}
	d.To = "stranger"
	if err := credit(ctx, d); err == nil {
		t.Fatal("deposit to an unassigned address must fail")
	}
}

func TestReorgAlertLeavesLedgerConsistent(t *testing.T) {
	l := memstore.NewLedger()
	fc := fakechain.New()
	w, err := custody.NewWatcher(fc, &memcursor.Store{}, NewDepositCreditor(l, map[string]string{"addr": "alice"}),
		custody.Config{Confirmations: 2, Watched: []string{"addr"}, StartBlock: 1})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	fc.Mine(custody.Transfer{TxHash: "0xbad", To: "addr", Asset: "ETH", Amount: 500})
	fc.Mine()
	if err := w.Poll(ctx); err != nil {
		t.Fatal(err)
	}
	fc.Reorg(2, nil, nil, nil)
	if err := w.Poll(ctx); !errors.Is(err, custody.ErrConfirmedDepositReorged) {
		t.Fatalf("got %v", err)
	}
	if got := l.Balance(ledger.UserAccount("alice", "ETH", ledger.Available)); got != 500 {
		t.Fatalf("balance silently changed to %d", got)
	}
	if err := l.Verify(); err != nil {
		t.Fatal(err)
	}
}

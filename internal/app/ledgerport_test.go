package app

import (
	"context"
	"errors"
	"testing"

	"github.com/gwakko/cex-lab/internal/ledger"
	"github.com/gwakko/cex-lab/internal/ledger/adapters/memstore"
	"github.com/gwakko/cex-lab/internal/trading"
)

func TestLedgerPortTranslatesAndQueries(t *testing.T) {
	l := memstore.NewLedger()
	p := NewLedgerPort(l)
	ctx := context.Background()

	_, err := p.Hold(ctx, "h0", "alice", "USDT", 5)
	if !errors.Is(err, trading.ErrInsufficientFunds) || !errors.Is(err, ledger.ErrInsufficientFunds) {
		t.Fatalf("insufficient funds must match both sentinels: %v", err)
	}
	if err.Error() != "ledger: insufficient funds: alice/USDT/available has 0, need 5" {
		t.Fatalf("message changed: %q", err.Error())
	}

	l.Apply(ledger.Deposit{Key: "d1", User: "alice", Asset: "USDT", Amount: 100})
	if replayed, err := p.Hold(ctx, "h1", "alice", "USDT", 40); err != nil || replayed {
		t.Fatalf("hold: %v %v", replayed, err)
	}
	if replayed, _ := p.Hold(ctx, "h1", "alice", "USDT", 40); !replayed {
		t.Fatal("repeated hold key must report replayed")
	}
	if err := p.Release(ctx, "r1", "alice", "USDT", 15); err != nil {
		t.Fatal(err)
	}
	got := p.LockedBalances()
	if len(got) != 1 || got[trading.Holding{User: "alice", Asset: "USDT"}] != 25 {
		t.Fatalf("locked balances %v", got)
	}
	if err := p.Verify(); err != nil {
		t.Fatal(err)
	}
}

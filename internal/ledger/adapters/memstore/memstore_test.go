package memstore_test

import (
	"reflect"
	"testing"

	"github.com/gwakko/cex-lab/internal/ledger"
	"github.com/gwakko/cex-lab/internal/ledger/adapters/memstore"
)

func TestReopenReplaysTransactions(t *testing.T) {
	store := &memstore.Store{}
	l, err := ledger.Open(store)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := l.Apply(ledger.Deposit{Key: "d1", User: "a", Asset: "BTC", Amount: 7}); err != nil {
		t.Fatal(err)
	}
	l2, err := ledger.Open(store)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(l2.Balances(), l.Balances()) {
		t.Fatalf("replayed %v, live %v", l2.Balances(), l.Balances())
	}
	if l := memstore.NewLedger(); len(l.Balances()) != 0 {
		t.Fatal("NewLedger must start empty")
	}
}

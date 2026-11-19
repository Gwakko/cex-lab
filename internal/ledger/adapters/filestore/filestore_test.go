package filestore_test

import (
	"path/filepath"
	"reflect"
	"testing"

	"github.com/gwakko/cex-lab/internal/ledger"
	"github.com/gwakko/cex-lab/internal/ledger/adapters/filestore"
)

const btc ledger.Asset = "BTC"

func mustApply(t *testing.T, l *ledger.Ledger, c ledger.Command) {
	t.Helper()
	if _, _, err := l.Apply(c); err != nil {
		t.Fatalf("apply %T: %v", c, err)
	}
}

func verify(t *testing.T, l *ledger.Ledger) {
	t.Helper()
	if err := l.Verify(); err != nil {
		t.Fatal(err)
	}
}

func fileLedger(t *testing.T, path string) (*ledger.Ledger, *filestore.Store) {
	t.Helper()
	fs, err := filestore.Open(path, nil)
	if err != nil {
		t.Fatal(err)
	}
	l, err := ledger.Open(fs)
	if err != nil {
		t.Fatal(err)
	}
	return l, fs
}

func TestFileJournalReplayRebuildsState(t *testing.T) {
	path := filepath.Join(t.TempDir(), "ledger.journal")
	l, fs := fileLedger(t, path)
	mustApply(t, l, ledger.Deposit{Key: "d1", User: "alice", Asset: btc, Amount: 1_000})
	mustApply(t, l, ledger.Hold{Key: "h1", User: "alice", Asset: btc, Amount: 300})
	mustApply(t, l, ledger.WithdrawRequest{Key: "w1", ID: "W1", User: "alice", Asset: btc, Amount: 200})
	want := l.Balances()
	fs.Close()

	l2, fs2 := fileLedger(t, path)
	defer fs2.Close()
	if got := l2.Balances(); !reflect.DeepEqual(got, want) {
		t.Fatalf("replayed balances %v, want %v", got, want)
	}
	if w, ok := l2.Withdrawal("W1"); !ok || w.State != ledger.WithdrawalPending {
		t.Fatalf("withdrawal after replay: %+v %v", w, ok)
	}
	if _, replayed, err := l2.Apply(ledger.Deposit{Key: "d1", User: "alice", Asset: btc, Amount: 1_000}); err != nil || !replayed {
		t.Fatalf("idempotency must survive restart: replayed=%v err=%v", replayed, err)
	}
	verify(t, l2)
}

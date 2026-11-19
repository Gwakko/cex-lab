package ledger

import (
	"errors"
	"path/filepath"
	"reflect"
	"testing"
)

const (
	btc  Asset = "BTC"
	usdt Asset = "USDT"
)

func mustApply(t *testing.T, l *Ledger, c Command) Tx {
	t.Helper()
	tx, _, err := l.Apply(c)
	if err != nil {
		t.Fatalf("apply %T: %v", c, err)
	}
	return tx
}

func wantBalance(t *testing.T, l *Ledger, a Account, want int64) {
	t.Helper()
	if got := l.Balance(a); got != want {
		t.Fatalf("%s = %d, want %d", a, got, want)
	}
}

func verify(t *testing.T, l *Ledger) {
	t.Helper()
	if err := l.Verify(); err != nil {
		t.Fatal(err)
	}
}

func TestDepositHoldRelease(t *testing.T) {
	l := New()
	mustApply(t, l, Deposit{Key: "d1", User: "alice", Asset: usdt, Amount: 1000})
	mustApply(t, l, Hold{Key: "h1", User: "alice", Asset: usdt, Amount: 400})
	wantBalance(t, l, UserAccount("alice", usdt, Available), 600)
	wantBalance(t, l, UserAccount("alice", usdt, Locked), 400)

	mustApply(t, l, Release{Key: "r1", User: "alice", Asset: usdt, Amount: 150})
	wantBalance(t, l, UserAccount("alice", usdt, Available), 750)
	wantBalance(t, l, UserAccount("alice", usdt, Locked), 250)
	if got := l.Supply(usdt); got != 1000 {
		t.Fatalf("supply = %d", got)
	}
	verify(t, l)
}

func TestRejectsInsufficientAndInvalid(t *testing.T) {
	l := New()
	mustApply(t, l, Deposit{Key: "d1", User: "alice", Asset: usdt, Amount: 100})

	cases := []struct {
		name string
		cmd  Command
		want error
	}{
		{"hold too much", Hold{Key: "h", User: "alice", Asset: usdt, Amount: 101}, ErrInsufficientFunds},
		{"release unlocked", Release{Key: "r", User: "alice", Asset: usdt, Amount: 1}, ErrInsufficientFunds},
		{"zero deposit", Deposit{Key: "z", User: "alice", Asset: usdt, Amount: 0}, ErrInvalidCommand},
		{"negative hold", Hold{Key: "n", User: "alice", Asset: usdt, Amount: -5}, ErrInvalidCommand},
		{"system as user", Deposit{Key: "s", User: System, Asset: usdt, Amount: 5}, ErrInvalidCommand},
		{"same asset settle", Settle{Key: "x", Buyer: "a", Seller: "b", Base: usdt, Quote: usdt, BaseQty: 1, QuoteQty: 1}, ErrInvalidCommand},
		{"fee above amount", Settle{Key: "f", Buyer: "a", Seller: "b", Base: btc, Quote: usdt, BaseQty: 1, QuoteQty: 1, BuyerFee: 2}, ErrInvalidCommand},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, _, err := l.Apply(tc.cmd); !errors.Is(err, tc.want) {
				t.Fatalf("got %v, want %v", err, tc.want)
			}
		})
	}
	wantBalance(t, l, UserAccount("alice", usdt, Available), 100)
	verify(t, l)
}

func TestSettleWithFeesAndRefund(t *testing.T) {
	l := New()
	mustApply(t, l, Deposit{Key: "d1", User: "buyer", Asset: usdt, Amount: 10_000})
	mustApply(t, l, Deposit{Key: "d2", User: "seller", Asset: btc, Amount: 500})
	mustApply(t, l, Hold{Key: "h1", User: "buyer", Asset: usdt, Amount: 6_000})
	mustApply(t, l, Hold{Key: "h2", User: "seller", Asset: btc, Amount: 500})

	tx := mustApply(t, l, Settle{
		Key: "s1", Buyer: "buyer", Seller: "seller", Base: btc, Quote: usdt,
		BaseQty: 200, QuoteQty: 5_000, BuyerFee: 2, SellerFee: 10, BuyerRefund: 1_000,
	})
	if len(tx.Entries) != 7 {
		t.Fatalf("expected 7 legs, got %d", len(tx.Entries))
	}
	wantBalance(t, l, UserAccount("buyer", usdt, Locked), 0)
	wantBalance(t, l, UserAccount("buyer", usdt, Available), 5_000)
	wantBalance(t, l, UserAccount("buyer", btc, Available), 198)
	wantBalance(t, l, UserAccount("seller", btc, Locked), 300)
	wantBalance(t, l, UserAccount("seller", usdt, Available), 4_990)
	wantBalance(t, l, SystemAccount(btc, Fees), 2)
	wantBalance(t, l, SystemAccount(usdt, Fees), 10)
	verify(t, l)

	_, _, err := l.Apply(Settle{Key: "s2", Buyer: "buyer", Seller: "seller", Base: btc, Quote: usdt, BaseQty: 1, QuoteQty: 1})
	if !errors.Is(err, ErrInsufficientFunds) {
		t.Fatalf("settle without buyer reserve: %v", err)
	}
}

func TestWithdrawLifecycle(t *testing.T) {
	l := New()
	mustApply(t, l, Deposit{Key: "d1", User: "alice", Asset: btc, Amount: 1_000})
	mustApply(t, l, WithdrawRequest{Key: "w1", ID: "W1", User: "alice", Asset: btc, Amount: 300})
	mustApply(t, l, WithdrawRequest{Key: "w2", ID: "W2", User: "alice", Asset: btc, Amount: 200})
	wantBalance(t, l, UserAccount("alice", btc, Available), 500)
	wantBalance(t, l, SystemAccount(btc, WithdrawalsInFlight), 500)
	verify(t, l)

	mustApply(t, l, WithdrawConfirm{Key: "c1", ID: "W1"})
	mustApply(t, l, WithdrawFail{Key: "f2", ID: "W2"})
	wantBalance(t, l, UserAccount("alice", btc, Available), 700)
	wantBalance(t, l, SystemAccount(btc, WithdrawalsInFlight), 0)
	if got := l.Supply(btc); got != 700 {
		t.Fatalf("supply after confirmed withdrawal = %d, want 700", got)
	}
	verify(t, l)

	if _, _, err := l.Apply(WithdrawConfirm{Key: "c2", ID: "W2"}); !errors.Is(err, ErrWithdrawalState) {
		t.Fatalf("confirm failed withdrawal: %v", err)
	}
	if _, _, err := l.Apply(WithdrawFail{Key: "f3", ID: "nope"}); !errors.Is(err, ErrUnknownWithdrawal) {
		t.Fatalf("unknown withdrawal: %v", err)
	}
	if _, _, err := l.Apply(WithdrawRequest{Key: "w3", ID: "W1", User: "alice", Asset: btc, Amount: 1}); !errors.Is(err, ErrInvalidCommand) {
		t.Fatalf("reused withdrawal id: %v", err)
	}
}

func TestDepositOverflowRejected(t *testing.T) {
	l := New()
	mustApply(t, l, Deposit{Key: "d1", User: "a", Asset: btc, Amount: 1 << 62})
	mustApply(t, l, Deposit{Key: "d2", User: "b", Asset: btc, Amount: 1<<62 - 1})
	if _, _, err := l.Apply(Deposit{Key: "d3", User: "c", Asset: btc, Amount: 2}); !errors.Is(err, ErrOverflow) {
		t.Fatalf("expected overflow, got %v", err)
	}
	verify(t, l)
}

func TestIdempotentRetry(t *testing.T) {
	l := New()
	dep := Deposit{Key: "dep-1", User: "alice", Asset: btc, Amount: 100}
	first := mustApply(t, l, dep)

	again, replayed, err := l.Apply(dep)
	if err != nil || !replayed {
		t.Fatalf("retry: replayed=%v err=%v", replayed, err)
	}
	if again.Seq != first.Seq {
		t.Fatalf("retry returned tx %d, want %d", again.Seq, first.Seq)
	}
	wantBalance(t, l, UserAccount("alice", btc, Available), 100)

	_, _, err = l.Apply(Deposit{Key: "dep-1", User: "alice", Asset: btc, Amount: 999})
	if !errors.Is(err, ErrIdempotencyConflict) {
		t.Fatalf("different payload: %v", err)
	}
	_, _, err = l.Apply(Hold{Key: "dep-1", User: "alice", Asset: btc, Amount: 100})
	if !errors.Is(err, ErrIdempotencyConflict) {
		t.Fatalf("different command type: %v", err)
	}
	if _, _, err := l.Apply(Deposit{User: "alice", Asset: btc, Amount: 1}); !errors.Is(err, ErrInvalidCommand) {
		t.Fatalf("empty key: %v", err)
	}
	wantBalance(t, l, UserAccount("alice", btc, Available), 100)
	verify(t, l)
}

func TestRejectedCommandIsNotRecorded(t *testing.T) {
	l := New()
	hold := Hold{Key: "h1", User: "alice", Asset: btc, Amount: 50}
	if _, _, err := l.Apply(hold); !errors.Is(err, ErrInsufficientFunds) {
		t.Fatalf("expected insufficient funds, got %v", err)
	}
	mustApply(t, l, Deposit{Key: "d1", User: "alice", Asset: btc, Amount: 50})
	if _, replayed, err := l.Apply(hold); err != nil || replayed {
		t.Fatalf("retry after funding: replayed=%v err=%v", replayed, err)
	}
	wantBalance(t, l, UserAccount("alice", btc, Locked), 50)
}

func fileLedger(t *testing.T, path string) (*Ledger, *FileStore) {
	t.Helper()
	fs, err := OpenFileStore(path, nil)
	if err != nil {
		t.Fatal(err)
	}
	l, err := Open(fs)
	if err != nil {
		t.Fatal(err)
	}
	return l, fs
}

func TestFileJournalReplayRebuildsState(t *testing.T) {
	path := filepath.Join(t.TempDir(), "ledger.journal")
	l, fs := fileLedger(t, path)
	mustApply(t, l, Deposit{Key: "d1", User: "alice", Asset: btc, Amount: 1_000})
	mustApply(t, l, Hold{Key: "h1", User: "alice", Asset: btc, Amount: 300})
	mustApply(t, l, WithdrawRequest{Key: "w1", ID: "W1", User: "alice", Asset: btc, Amount: 200})
	want := l.Balances()
	fs.Close()

	l2, fs2 := fileLedger(t, path)
	defer fs2.Close()
	if got := l2.Balances(); !reflect.DeepEqual(got, want) {
		t.Fatalf("replayed balances %v, want %v", got, want)
	}
	if w, ok := l2.Withdrawal("W1"); !ok || w.State != WithdrawalPending {
		t.Fatalf("withdrawal after replay: %+v %v", w, ok)
	}
	if _, replayed, err := l2.Apply(Deposit{Key: "d1", User: "alice", Asset: btc, Amount: 1_000}); err != nil || !replayed {
		t.Fatalf("idempotency must survive restart: replayed=%v err=%v", replayed, err)
	}
	verify(t, l2)
}

type failingStore struct{ MemStore }

func (*failingStore) Append(Tx) error { return errors.New("disk full") }

func TestFailedPersistLeavesStateUntouched(t *testing.T) {
	l, err := Open(&failingStore{})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := l.Apply(Deposit{Key: "d1", User: "a", Asset: btc, Amount: 5}); err == nil {
		t.Fatal("expected persist error")
	}
	wantBalance(t, l, UserAccount("a", btc, Available), 0)
	if _, replayed, _ := l.Apply(Deposit{Key: "d1", User: "a", Asset: btc, Amount: 5}); replayed {
		t.Fatal("unpersisted key must not be remembered")
	}
}

func TestReplayRejectsTamperedJournal(t *testing.T) {
	bad := &MemStore{}
	bad.Append(Tx{Seq: 1, Key: "x", Entries: []Entry{{UserAccount("a", btc, Available), 10}}})
	if _, err := Open(bad); err == nil {
		t.Fatal("expected unbalanced replay to fail")
	}
}

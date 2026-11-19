package ledger

import (
	"errors"
	"testing"
)

const (
	btc  Asset = "BTC"
	usdt Asset = "USDT"
)

func mustApply(t *testing.T, l *Ledger, c Command) Tx {
	t.Helper()
	tx, err := l.Apply(c)
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
			if _, err := l.Apply(tc.cmd); !errors.Is(err, tc.want) {
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

	_, err := l.Apply(Settle{Key: "s2", Buyer: "buyer", Seller: "seller", Base: btc, Quote: usdt, BaseQty: 1, QuoteQty: 1})
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

	if _, err := l.Apply(WithdrawConfirm{Key: "c2", ID: "W2"}); !errors.Is(err, ErrWithdrawalState) {
		t.Fatalf("confirm failed withdrawal: %v", err)
	}
	if _, err := l.Apply(WithdrawFail{Key: "f3", ID: "nope"}); !errors.Is(err, ErrUnknownWithdrawal) {
		t.Fatalf("unknown withdrawal: %v", err)
	}
	if _, err := l.Apply(WithdrawRequest{Key: "w3", ID: "W1", User: "alice", Asset: btc, Amount: 1}); !errors.Is(err, ErrInvalidCommand) {
		t.Fatalf("reused withdrawal id: %v", err)
	}
}

func TestDepositOverflowRejected(t *testing.T) {
	l := New()
	mustApply(t, l, Deposit{Key: "d1", User: "a", Asset: btc, Amount: 1 << 62})
	mustApply(t, l, Deposit{Key: "d2", User: "b", Asset: btc, Amount: 1<<62 - 1})
	if _, err := l.Apply(Deposit{Key: "d3", User: "c", Asset: btc, Amount: 2}); !errors.Is(err, ErrOverflow) {
		t.Fatalf("expected overflow, got %v", err)
	}
	verify(t, l)
}

package trading_test

import (
	"context"
	"errors"
	"testing"

	"github.com/gwakko/cex-lab/internal/app"
	"github.com/gwakko/cex-lab/internal/ledger"
	"github.com/gwakko/cex-lab/internal/ledger/adapters/memstore"
	"github.com/gwakko/cex-lab/internal/trading"
	"github.com/gwakko/cex-lab/internal/trading/adapters/memlog"
)

const (
	btc  = "BTC"
	eth  = "ETH"
	usdt = "USDT"
)

// Prices are USDT cents per lot; BTC-USDT lot = 10 base units.
func testConfigs() []trading.MarketConfig {
	return []trading.MarketConfig{
		{Symbol: "BTC-USDT", Base: btc, Quote: usdt, Tick: 1, Lot: 10, MakerFeeBps: 10, TakerFeeBps: 20, Log: &memlog.Log{}},
		{Symbol: "ETH-USDT", Base: eth, Quote: usdt, Tick: 5, Lot: 1, MakerFeeBps: 0, TakerFeeBps: 50, Log: &memlog.Log{}},
	}
}

// env is the trading service wired to a real ledger through the app adapter;
// tests fund users and read balances on the ledger directly.
type env struct {
	*trading.Service
	l *ledger.Ledger
}

func newEnv(t *testing.T, l *ledger.Ledger, cfgs []trading.MarketConfig) *env {
	t.Helper()
	x, err := trading.New(app.NewLedgerPort(l), cfgs)
	if err != nil {
		t.Fatal(err)
	}
	return &env{Service: x, l: l}
}

func newExchange(t *testing.T) *env {
	t.Helper()
	x := newEnv(t, memstore.NewLedger(), testConfigs())
	t.Cleanup(x.Close)
	return x
}

func deposit(t *testing.T, x *env, key, user string, asset ledger.Asset, amt int64) {
	t.Helper()
	if _, _, err := x.l.Apply(ledger.Deposit{Key: key, User: user, Asset: asset, Amount: amt}); err != nil {
		t.Fatal(err)
	}
}

func bal(x *env, user string, asset ledger.Asset, k ledger.Kind) int64 {
	return x.l.Balance(ledger.UserAccount(user, asset, k))
}

func place(t *testing.T, x *env, req trading.PlaceRequest) trading.PlaceResult {
	t.Helper()
	r, err := x.Place(context.Background(), req)
	if err != nil {
		t.Fatalf("place %+v: %v", req, err)
	}
	return r
}

func TestPlaceMatchSettleWithFeesAndPriceImprovement(t *testing.T) {
	x := newExchange(t)
	deposit(t, x, "d1", "seller", btc, 100)
	deposit(t, x, "d2", "buyer", usdt, 1_000_000)

	ask := place(t, x, trading.PlaceRequest{Market: "BTC-USDT", User: "seller", Side: trading.Sell, Type: trading.Limit, Price: 50_000, Qty: 100})
	if !ask.Resting || bal(x, "seller", btc, ledger.Locked) != 100 {
		t.Fatalf("ask not reserved: %+v", ask)
	}

	// Buyer bids above the ask: fills at the maker price, gets the difference back.
	r := place(t, x, trading.PlaceRequest{Market: "BTC-USDT", User: "buyer", Side: trading.Buy, Type: trading.Limit, Price: 51_000, Qty: 60})
	if len(r.Trades) != 1 || r.Trades[0].Price != 50_000 || r.Resting {
		t.Fatalf("unexpected result %+v", r)
	}
	notional := int64(50_000 * 6)
	checks := []struct {
		name string
		got  int64
		want int64
	}{
		{"buyer usdt available", bal(x, "buyer", usdt, ledger.Available), 1_000_000 - notional},
		{"buyer usdt locked", bal(x, "buyer", usdt, ledger.Locked), 0},
		{"buyer btc (taker fee 20bps of 60 floors to 0)", bal(x, "buyer", btc, ledger.Available), 60},
		{"seller btc locked", bal(x, "seller", btc, ledger.Locked), 40},
		{"seller usdt (maker fee 10bps)", bal(x, "seller", usdt, ledger.Available), notional - notional/1000},
		{"usdt fees", x.l.Balance(ledger.SystemAccount(usdt, ledger.Fees)), notional / 1000},
	}
	for _, c := range checks {
		if c.got != c.want {
			t.Errorf("%s = %d, want %d", c.name, c.got, c.want)
		}
	}
	if err := x.Check(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestInsufficientBalanceRejectsBeforeEngine(t *testing.T) {
	x := newExchange(t)
	deposit(t, x, "d1", "alice", usdt, 999)
	_, err := x.Place(context.Background(), trading.PlaceRequest{Market: "BTC-USDT", User: "alice", Side: trading.Buy, Type: trading.Limit, Price: 100, Qty: 100})
	if !errors.Is(err, ledger.ErrInsufficientFunds) {
		t.Fatalf("got %v", err)
	}
	if snap, err := x.Book(context.Background(), "BTC-USDT"); err != nil || snap.Seq != 0 {
		t.Fatalf("rejected order reached the engine (seq %d, err %v)", snap.Seq, err)
	}
	if bal(x, "alice", usdt, ledger.Available) != 999 {
		t.Fatal("balance changed")
	}
}

func TestInvalidOrderAndUnknownMarket(t *testing.T) {
	x := newExchange(t)
	deposit(t, x, "d1", "alice", usdt, 1_000_000)
	if _, err := x.Place(context.Background(), trading.PlaceRequest{Market: "ETH-USDT", User: "alice", Side: trading.Buy, Type: trading.Limit, Price: 101, Qty: 1}); !errors.Is(err, trading.ErrInvalidOrder) {
		t.Fatalf("off-tick: %v", err)
	}
	if _, err := x.Place(context.Background(), trading.PlaceRequest{Market: "DOGE-USDT"}); !errors.Is(err, trading.ErrUnknownMarket) {
		t.Fatalf("unknown market: %v", err)
	}
}

func TestCancelReleasesRemainingReserve(t *testing.T) {
	x := newExchange(t)
	deposit(t, x, "d1", "alice", usdt, 10_000)
	deposit(t, x, "d2", "bob", btc, 50)

	bid := place(t, x, trading.PlaceRequest{Market: "BTC-USDT", User: "alice", Side: trading.Buy, Type: trading.Limit, Price: 100, Qty: 100})
	place(t, x, trading.PlaceRequest{Market: "BTC-USDT", User: "bob", Side: trading.Sell, Type: trading.Limit, Price: 100, Qty: 30})
	if got := bal(x, "alice", usdt, ledger.Locked); got != 700 {
		t.Fatalf("locked after partial fill = %d, want 700", got)
	}
	if _, err := x.Cancel(context.Background(), "BTC-USDT", "bob", bid.OrderID); !errors.Is(err, trading.ErrNotOwner) {
		t.Fatalf("foreign cancel: %v", err)
	}
	o, err := x.Cancel(context.Background(), "BTC-USDT", "alice", bid.OrderID)
	if err != nil || o.Remaining != 70 {
		t.Fatalf("cancel: %+v %v", o, err)
	}
	if bal(x, "alice", usdt, ledger.Locked) != 0 || bal(x, "alice", usdt, ledger.Available) != 9_700 {
		t.Fatal("reserve not released")
	}
	if _, err := x.Cancel(context.Background(), "BTC-USDT", "alice", bid.OrderID); !errors.Is(err, trading.ErrUnknownOrder) {
		t.Fatalf("double cancel: %v", err)
	}
	if err := x.Check(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestMarketOrdersReleaseUnusedReserve(t *testing.T) {
	x := newExchange(t)
	deposit(t, x, "d1", "maker", eth, 10)
	deposit(t, x, "d2", "taker", usdt, 10_000)
	place(t, x, trading.PlaceRequest{Market: "ETH-USDT", User: "maker", Side: trading.Sell, Type: trading.Limit, Price: 1_000, Qty: 3})

	r := place(t, x, trading.PlaceRequest{Market: "ETH-USDT", User: "taker", Side: trading.Buy, Type: trading.Market, Qty: 5, QuoteBudget: 2_500})
	if len(r.Trades) != 1 || r.Trades[0].Qty != 2 || r.Resting {
		t.Fatalf("budget-limited market buy: %+v", r)
	}
	if bal(x, "taker", usdt, ledger.Locked) != 0 || bal(x, "taker", usdt, ledger.Available) != 8_000 {
		t.Fatalf("unused budget not released")
	}
	if got := bal(x, "taker", eth, ledger.Available); got != 2 { // 50bps of 2 floors to 0
		t.Fatalf("taker eth = %d", got)
	}

	deposit(t, x, "d3", "seller", eth, 5)
	r = place(t, x, trading.PlaceRequest{Market: "ETH-USDT", User: "seller", Side: trading.Sell, Type: trading.Market, Qty: 5})
	if len(r.Trades) != 0 || bal(x, "seller", eth, ledger.Available) != 5 {
		t.Fatalf("market sell into empty book must release: %+v", r)
	}
	if err := x.Check(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestRestartRecoversBookAndOrderIDs(t *testing.T) {
	store := &memstore.Store{}
	cfgs := testConfigs()
	open := func() *env {
		l, err := ledger.Open(store)
		if err != nil {
			t.Fatal(err)
		}
		return newEnv(t, l, cfgs)
	}
	x := open()
	deposit(t, x, "d1", "alice", usdt, 10_000)
	first := place(t, x, trading.PlaceRequest{Market: "BTC-USDT", User: "alice", Side: trading.Buy, Type: trading.Limit, Price: 100, Qty: 10})
	x.Close()

	x = open()
	defer x.Close()
	if err := x.Check(context.Background()); err != nil {
		t.Fatalf("after restart: %v", err)
	}
	second := place(t, x, trading.PlaceRequest{Market: "BTC-USDT", User: "alice", Side: trading.Buy, Type: trading.Limit, Price: 100, Qty: 10})
	if second.OrderID == first.OrderID {
		t.Fatalf("order id reused after restart: %s", second.OrderID)
	}
	if _, err := x.Cancel(context.Background(), "BTC-USDT", "alice", first.OrderID); err != nil {
		t.Fatalf("cancel order from before restart: %v", err)
	}
	if err := x.Check(context.Background()); err != nil {
		t.Fatal(err)
	}
}

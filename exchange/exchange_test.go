package exchange

import (
	"context"
	"errors"
	"math"
	"testing"

	"github.com/gwakko/cex-lab/engine"
	"github.com/gwakko/cex-lab/ledger"
)

const (
	btc  ledger.Asset = "BTC"
	eth  ledger.Asset = "ETH"
	usdt ledger.Asset = "USDT"
)

// Prices are USDT cents per lot; BTC-USDT lot = 10 base units.
func testConfigs() []MarketConfig {
	return []MarketConfig{
		{Symbol: "BTC-USDT", Base: btc, Quote: usdt, Tick: 1, Lot: 10, MakerFeeBps: 10, TakerFeeBps: 20, Log: &engine.MemLog{}},
		{Symbol: "ETH-USDT", Base: eth, Quote: usdt, Tick: 5, Lot: 1, MakerFeeBps: 0, TakerFeeBps: 50, Log: &engine.MemLog{}},
	}
}

func newExchange(t *testing.T) *Exchange {
	t.Helper()
	x, err := New(ledger.New(), testConfigs())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(x.Close)
	return x
}

func deposit(t *testing.T, x *Exchange, key, user string, asset ledger.Asset, amt int64) {
	t.Helper()
	if _, _, err := x.Ledger().Apply(ledger.Deposit{Key: key, User: user, Asset: asset, Amount: amt}); err != nil {
		t.Fatal(err)
	}
}

func bal(x *Exchange, user string, asset ledger.Asset, k ledger.Kind) int64 {
	return x.Ledger().Balance(ledger.UserAccount(user, asset, k))
}

func place(t *testing.T, x *Exchange, req PlaceRequest) PlaceResult {
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

	ask := place(t, x, PlaceRequest{Market: "BTC-USDT", User: "seller", Side: engine.Sell, Type: engine.Limit, Price: 50_000, Qty: 100})
	if !ask.Resting || bal(x, "seller", btc, ledger.Locked) != 100 {
		t.Fatalf("ask not reserved: %+v", ask)
	}

	// Buyer bids above the ask: fills at the maker price, gets the difference back.
	r := place(t, x, PlaceRequest{Market: "BTC-USDT", User: "buyer", Side: engine.Buy, Type: engine.Limit, Price: 51_000, Qty: 60})
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
		{"usdt fees", x.Ledger().Balance(ledger.SystemAccount(usdt, ledger.Fees)), notional / 1000},
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
	_, err := x.Place(context.Background(), PlaceRequest{Market: "BTC-USDT", User: "alice", Side: engine.Buy, Type: engine.Limit, Price: 100, Qty: 100})
	if !errors.Is(err, ledger.ErrInsufficientFunds) {
		t.Fatalf("got %v", err)
	}
	if seq := x.markets["BTC-USDT"].eng.LastSeq(); seq != 0 {
		t.Fatalf("rejected order reached the engine (seq %d)", seq)
	}
	if bal(x, "alice", usdt, ledger.Available) != 999 {
		t.Fatal("balance changed")
	}
}

func TestInvalidOrderAndUnknownMarket(t *testing.T) {
	x := newExchange(t)
	deposit(t, x, "d1", "alice", usdt, 1_000_000)
	if _, err := x.Place(context.Background(), PlaceRequest{Market: "ETH-USDT", User: "alice", Side: engine.Buy, Type: engine.Limit, Price: 101, Qty: 1}); !errors.Is(err, engine.ErrInvalidOrder) {
		t.Fatalf("off-tick: %v", err)
	}
	if _, err := x.Place(context.Background(), PlaceRequest{Market: "DOGE-USDT"}); !errors.Is(err, ErrUnknownMarket) {
		t.Fatalf("unknown market: %v", err)
	}
}

func TestCancelReleasesRemainingReserve(t *testing.T) {
	x := newExchange(t)
	deposit(t, x, "d1", "alice", usdt, 10_000)
	deposit(t, x, "d2", "bob", btc, 50)

	bid := place(t, x, PlaceRequest{Market: "BTC-USDT", User: "alice", Side: engine.Buy, Type: engine.Limit, Price: 100, Qty: 100})
	place(t, x, PlaceRequest{Market: "BTC-USDT", User: "bob", Side: engine.Sell, Type: engine.Limit, Price: 100, Qty: 30})
	if got := bal(x, "alice", usdt, ledger.Locked); got != 700 {
		t.Fatalf("locked after partial fill = %d, want 700", got)
	}
	if _, err := x.Cancel(context.Background(), "BTC-USDT", "bob", bid.OrderID); !errors.Is(err, engine.ErrNotOwner) {
		t.Fatalf("foreign cancel: %v", err)
	}
	o, err := x.Cancel(context.Background(), "BTC-USDT", "alice", bid.OrderID)
	if err != nil || o.Remaining != 70 {
		t.Fatalf("cancel: %+v %v", o, err)
	}
	if bal(x, "alice", usdt, ledger.Locked) != 0 || bal(x, "alice", usdt, ledger.Available) != 9_700 {
		t.Fatal("reserve not released")
	}
	if _, err := x.Cancel(context.Background(), "BTC-USDT", "alice", bid.OrderID); !errors.Is(err, engine.ErrUnknownOrder) {
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
	place(t, x, PlaceRequest{Market: "ETH-USDT", User: "maker", Side: engine.Sell, Type: engine.Limit, Price: 1_000, Qty: 3})

	r := place(t, x, PlaceRequest{Market: "ETH-USDT", User: "taker", Side: engine.Buy, Type: engine.Market, Qty: 5, QuoteBudget: 2_500})
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
	r = place(t, x, PlaceRequest{Market: "ETH-USDT", User: "seller", Side: engine.Sell, Type: engine.Market, Qty: 5})
	if len(r.Trades) != 0 || bal(x, "seller", eth, ledger.Available) != 5 {
		t.Fatalf("market sell into empty book must release: %+v", r)
	}
	if err := x.Check(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestRestartRecoversBookAndOrderIDs(t *testing.T) {
	store := &ledger.MemStore{}
	cfgs := testConfigs()
	open := func() *Exchange {
		l, err := ledger.Open(store)
		if err != nil {
			t.Fatal(err)
		}
		x, err := New(l, cfgs)
		if err != nil {
			t.Fatal(err)
		}
		return x
	}
	x := open()
	deposit(t, x, "d1", "alice", usdt, 10_000)
	first := place(t, x, PlaceRequest{Market: "BTC-USDT", User: "alice", Side: engine.Buy, Type: engine.Limit, Price: 100, Qty: 10})
	x.Close()

	x = open()
	defer x.Close()
	if err := x.Check(context.Background()); err != nil {
		t.Fatalf("after restart: %v", err)
	}
	second := place(t, x, PlaceRequest{Market: "BTC-USDT", User: "alice", Side: engine.Buy, Type: engine.Limit, Price: 100, Qty: 10})
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

func TestFeeNoOverflow(t *testing.T) {
	if got := fee(math.MaxInt64, 10_000); got != math.MaxInt64 {
		t.Fatalf("fee(max, 100%%) = %d", got)
	}
	if got := fee(1_000_000, 25); got != 2_500 {
		t.Fatalf("fee = %d", got)
	}
}

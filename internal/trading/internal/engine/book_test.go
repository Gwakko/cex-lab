package engine

import (
	"errors"
	"testing"
)

var spec = MarketSpec{Symbol: "BTC-USDT", Tick: 5, Lot: 10}

type bookT struct {
	t *testing.T
	b *Book
}

func newBookT(t *testing.T) *bookT {
	b, err := NewBook(spec)
	if err != nil {
		t.Fatal(err)
	}
	return &bookT{t: t, b: b}
}

func (bt *bookT) do(typ CommandType, o Order) Result {
	return bt.b.Apply(Command{Seq: bt.b.Seq() + 1, Type: typ, Order: o})
}

func (bt *bookT) ok(typ CommandType, o Order) Result {
	bt.t.Helper()
	r := bt.do(typ, o)
	if r.Err != nil {
		bt.t.Fatalf("%v %s: %v", typ, o.ID, r.Err)
	}
	if bt.b.Snapshot().Crossed() {
		bt.t.Fatalf("book crossed after %s", o.ID)
	}
	return r
}

func limit(id, user string, side Side, price, qty int64) Order {
	return Order{ID: id, User: user, Side: side, Type: Limit, Price: price, Qty: qty}
}

func TestPriceTimePriority(t *testing.T) {
	bt := newBookT(t)
	bt.ok(Place, limit("a1", "m1", Sell, 105, 10))
	bt.ok(Place, limit("a2", "m2", Sell, 100, 10))
	bt.ok(Place, limit("a3", "m3", Sell, 100, 10))

	r := bt.ok(Place, limit("b1", "t", Buy, 105, 30))
	if len(r.Trades) != 3 {
		t.Fatalf("got %d trades", len(r.Trades))
	}
	wantOrder := []struct {
		id    string
		price int64
	}{{"a2", 100}, {"a3", 100}, {"a1", 105}}
	for i, w := range wantOrder {
		if r.Trades[i].MakerOrderID != w.id || r.Trades[i].Price != w.price {
			t.Fatalf("trade %d = %+v, want maker %s at %d", i, r.Trades[i], w.id, w.price)
		}
	}
	if r.Resting || r.Order.Remaining != 0 {
		t.Fatalf("taker should be filled: %+v", r)
	}
}

func TestPartialFillRestsRemainder(t *testing.T) {
	bt := newBookT(t)
	bt.ok(Place, limit("a1", "m", Sell, 100, 20))
	r := bt.ok(Place, limit("b1", "t", Buy, 110, 50))
	if len(r.Trades) != 1 || r.Trades[0].Qty != 20 || r.Trades[0].Price != 100 {
		t.Fatalf("trades %+v", r.Trades)
	}
	if !r.Resting || r.Order.Remaining != 30 {
		t.Fatalf("expected 30 resting, got %+v", r.Order)
	}
	s := bt.b.Snapshot()
	if len(s.Asks) != 0 || len(s.Bids) != 1 || s.Bids[0].Remaining != 30 {
		t.Fatalf("snapshot %+v", s)
	}

	r = bt.ok(Place, limit("a2", "m", Sell, 110, 10))
	if len(r.Trades) != 1 || r.Trades[0].MakerOrderID != "b1" || r.Trades[0].Price != 110 {
		t.Fatalf("maker partial: %+v", r.Trades)
	}
	if got := bt.b.Snapshot().Bids[0].Remaining; got != 20 {
		t.Fatalf("maker remaining %d", got)
	}
}

func TestNonCrossingLimitRests(t *testing.T) {
	bt := newBookT(t)
	bt.ok(Place, limit("a1", "m", Sell, 105, 10))
	r := bt.ok(Place, limit("b1", "t", Buy, 100, 10))
	if len(r.Trades) != 0 || !r.Resting {
		t.Fatalf("%+v", r)
	}
}

func TestMarketOrders(t *testing.T) {
	bt := newBookT(t)
	bt.ok(Place, limit("a1", "m", Sell, 100, 10))
	bt.ok(Place, limit("a2", "m", Sell, 120, 30))

	// Budget 359 buys 1 lot at 100 and 2 lots at 120 (340); 19 cannot buy another lot.
	r := bt.ok(Place, Order{ID: "mb", User: "t", Side: Buy, Type: Market, Qty: 100, QuoteBudget: 359})
	if len(r.Trades) != 2 || r.Trades[1].Qty != 20 {
		t.Fatalf("budget-limited market buy: %+v", r.Trades)
	}
	if r.Resting || r.Order.Remaining != 70 {
		t.Fatalf("market remainder must be dropped, got %+v", r)
	}

	r = bt.ok(Place, Order{ID: "ms", User: "t", Side: Sell, Type: Market, Qty: 50})
	if len(r.Trades) != 0 || r.Resting {
		t.Fatalf("market sell into empty bids: %+v", r)
	}
	if got := len(bt.b.Snapshot().Asks); got != 1 {
		t.Fatalf("asks left %d", got)
	}
}

func TestCancel(t *testing.T) {
	bt := newBookT(t)
	bt.ok(Place, limit("b1", "alice", Buy, 100, 10))
	bt.ok(Place, limit("b2", "alice", Buy, 100, 20))

	if r := bt.do(Cancel, Order{ID: "b1", User: "mallory"}); !errors.Is(r.Err, ErrNotOwner) {
		t.Fatalf("foreign cancel: %v", r.Err)
	}
	r := bt.ok(Cancel, Order{ID: "b1", User: "alice"})
	if r.Order.Remaining != 10 {
		t.Fatalf("cancelled %+v", r.Order)
	}
	if r := bt.do(Cancel, Order{ID: "b1", User: "alice"}); !errors.Is(r.Err, ErrUnknownOrder) {
		t.Fatalf("double cancel: %v", r.Err)
	}
	s := bt.b.Snapshot()
	if len(s.Bids) != 1 || s.Bids[0].ID != "b2" {
		t.Fatalf("snapshot %+v", s)
	}
	bt.ok(Cancel, Order{ID: "b2", User: "alice"})
	if s := bt.b.Snapshot(); len(s.Bids) != 0 {
		t.Fatalf("empty level not removed: %+v", s)
	}
}

func TestValidation(t *testing.T) {
	cases := map[string]Order{
		"off tick":           limit("x", "u", Buy, 101, 10),
		"off lot":            limit("x", "u", Buy, 100, 15),
		"zero qty":           limit("x", "u", Buy, 100, 0),
		"negative price":     limit("x", "u", Buy, -5, 10),
		"no user":            limit("x", "", Buy, 100, 10),
		"bad side":           {ID: "x", User: "u", Side: 9, Type: Limit, Price: 100, Qty: 10},
		"market with price":  {ID: "x", User: "u", Side: Sell, Type: Market, Price: 100, Qty: 10},
		"market buy no cash": {ID: "x", User: "u", Side: Buy, Type: Market, Qty: 10},
		"overflow":           limit("x", "u", Buy, 1<<62, 1<<40),
	}
	for name, o := range cases {
		t.Run(name, func(t *testing.T) {
			bt := newBookT(t)
			if r := bt.do(Place, o); !errors.Is(r.Err, ErrInvalidOrder) {
				t.Fatalf("got %v", r.Err)
			}
		})
	}

	bt := newBookT(t)
	bt.ok(Place, limit("dup", "u", Buy, 100, 10))
	if r := bt.do(Place, limit("dup", "u", Buy, 100, 10)); !errors.Is(r.Err, ErrDuplicateOrder) {
		t.Fatalf("duplicate: %v", r.Err)
	}
	if r := bt.b.Apply(Command{Seq: 99, Type: Place}); !errors.Is(r.Err, ErrOutOfOrder) {
		t.Fatalf("gap: %v", r.Err)
	}
}

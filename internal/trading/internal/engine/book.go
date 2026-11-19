package engine

import (
	"cmp"
	"fmt"
	"slices"
	"sort"

	"github.com/gwakko/cex-lab/internal/events"
)

type level struct {
	price  int64
	orders []*Order // FIFO: time priority within a price
}

// Book is the pure, single-threaded state machine of one market. Apply is
// deterministic: the same command sequence always yields the same results.
type Book struct {
	spec   MarketSpec
	bids   []*level // best (highest) first
	asks   []*level // best (lowest) first
	orders map[string]*Order
	seq    uint64
	// touched records the price levels the last command changed, for
	// market data deltas. It never influences matching.
	touched map[levelKey]struct{}
}

type levelKey struct {
	side  Side
	price int64
}

// NewBook returns an empty book.
func NewBook(spec MarketSpec) (*Book, error) {
	if err := spec.validate(); err != nil {
		return nil, err
	}
	return &Book{spec: spec, orders: map[string]*Order{}, touched: map[levelKey]struct{}{}}, nil
}

// Seq is the sequence number of the last applied command.
func (b *Book) Seq() uint64 { return b.seq }

// Apply executes one command. Commands must arrive with Seq = previous + 1.
func (b *Book) Apply(cmd Command) Result {
	if cmd.Seq != b.seq+1 {
		return Result{Seq: cmd.Seq, Err: fmt.Errorf("%w: got %d, want %d", ErrOutOfOrder, cmd.Seq, b.seq+1)}
	}
	b.seq = cmd.Seq
	clear(b.touched)
	switch cmd.Type {
	case Place:
		return b.place(cmd)
	case Cancel:
		return b.cancel(cmd)
	}
	return Result{Seq: cmd.Seq, Err: fmt.Errorf("%w: unknown command type %d", ErrInvalidOrder, cmd.Type)}
}

func (b *Book) place(cmd Command) Result {
	o := cmd.Order
	res := Result{Seq: cmd.Seq, Order: o}
	if err := b.spec.Validate(o); err != nil {
		res.Err = err
		return res
	}
	if _, dup := b.orders[o.ID]; dup {
		res.Err = fmt.Errorf("%w: %s", ErrDuplicateOrder, o.ID)
		return res
	}
	o.Remaining = o.Qty
	o.Seq = cmd.Seq
	res.Trades = b.match(&o)
	if o.Type == Limit && o.Remaining > 0 {
		b.rest(&o)
		res.Resting = true
	}
	res.Order = o
	return res
}

func (b *Book) match(t *Order) []Trade {
	var trades []Trade
	budget := t.QuoteBudget
	opp := b.levels(t.Side.opposite())
	for t.Remaining > 0 && len(*opp) > 0 {
		lvl := (*opp)[0]
		if t.Type == Limit && !crosses(t, lvl.price) {
			break
		}
		maker := lvl.orders[0]
		qty := min(t.Remaining, maker.Remaining)
		if t.Type == Market && t.Side == Buy {
			qty = min(qty/b.spec.Lot, budget/lvl.price) * b.spec.Lot
			if qty == 0 {
				break
			}
			budget -= lvl.price * (qty / b.spec.Lot)
		}
		trades = append(trades, Trade{
			Seq:          t.Seq,
			MakerOrderID: maker.ID,
			TakerOrderID: t.ID,
			MakerUser:    maker.User,
			TakerUser:    t.User,
			TakerSide:    t.Side,
			Price:        lvl.price,
			Qty:          qty,
		})
		t.Remaining -= qty
		maker.Remaining -= qty
		b.touch(maker.Side, lvl.price)
		if maker.Remaining == 0 {
			b.popHead(opp)
		}
	}
	return trades
}

func crosses(t *Order, price int64) bool {
	if t.Side == Buy {
		return t.Price >= price
	}
	return t.Price <= price
}

func (b *Book) popHead(levels *[]*level) {
	lvl := (*levels)[0]
	delete(b.orders, lvl.orders[0].ID)
	lvl.orders[0] = nil
	lvl.orders = lvl.orders[1:]
	if len(lvl.orders) == 0 {
		*levels = (*levels)[1:]
	}
}

func (b *Book) levels(s Side) *[]*level {
	if s == Buy {
		return &b.bids
	}
	return &b.asks
}

// levelIndex returns where price is (or would be inserted) in priority order.
func levelIndex(levels []*level, side Side, price int64) int {
	return sort.Search(len(levels), func(i int) bool {
		if side == Buy {
			return levels[i].price <= price
		}
		return levels[i].price >= price
	})
}

func (b *Book) rest(o *Order) {
	levels := b.levels(o.Side)
	i := levelIndex(*levels, o.Side, o.Price)
	if i < len(*levels) && (*levels)[i].price == o.Price {
		(*levels)[i].orders = append((*levels)[i].orders, o)
	} else {
		*levels = slices.Insert(*levels, i, &level{price: o.Price, orders: []*Order{o}})
	}
	b.orders[o.ID] = o
	b.touch(o.Side, o.Price)
}

func (b *Book) cancel(cmd Command) Result {
	res := Result{Seq: cmd.Seq}
	o, ok := b.orders[cmd.Order.ID]
	switch {
	case !ok:
		res.Err = fmt.Errorf("%w: %s", ErrUnknownOrder, cmd.Order.ID)
		return res
	case o.User != cmd.Order.User:
		res.Err = fmt.Errorf("%w: %s", ErrNotOwner, cmd.Order.ID)
		return res
	}
	levels := b.levels(o.Side)
	i := levelIndex(*levels, o.Side, o.Price)
	lvl := (*levels)[i]
	lvl.orders = slices.DeleteFunc(lvl.orders, func(x *Order) bool { return x.ID == o.ID })
	if len(lvl.orders) == 0 {
		*levels = slices.Delete(*levels, i, i+1)
	}
	delete(b.orders, o.ID)
	b.touch(o.Side, o.Price)
	res.Order = *o
	return res
}

func (b *Book) touch(s Side, price int64) { b.touched[levelKey{s, price}] = struct{}{} }

// Changes returns the aggregate quantity now resting at every price level the
// last command touched (0 if the level is gone), bids best first, asks best
// first.
func (b *Book) Changes() (bids, asks []events.Level) {
	for k := range b.touched {
		l := events.Level{Price: k.price, Qty: b.levelQty(k.side, k.price)}
		if k.side == Buy {
			bids = append(bids, l)
		} else {
			asks = append(asks, l)
		}
	}
	slices.SortFunc(bids, func(x, y events.Level) int { return cmp.Compare(y.Price, x.Price) })
	slices.SortFunc(asks, func(x, y events.Level) int { return cmp.Compare(x.Price, y.Price) })
	return bids, asks
}

func (b *Book) levelQty(s Side, price int64) int64 {
	levels := *b.levels(s)
	i := levelIndex(levels, s, price)
	if i == len(levels) || levels[i].price != price {
		return 0
	}
	var q int64
	for _, o := range levels[i].orders {
		q += o.Remaining
	}
	return q
}

// Depth returns the number of price levels on each side.
func (b *Book) Depth() (bids, asks int) { return len(b.bids), len(b.asks) }

// Snapshot is a copy of the resting orders in priority order.
type Snapshot struct {
	Seq  uint64
	Bids []Order
	Asks []Order
}

// Snapshot copies the book state.
func (b *Book) Snapshot() Snapshot {
	flat := func(levels []*level) []Order {
		var out []Order
		for _, l := range levels {
			for _, o := range l.orders {
				out = append(out, *o)
			}
		}
		return out
	}
	return Snapshot{Seq: b.seq, Bids: flat(b.bids), Asks: flat(b.asks)}
}

// Crossed reports whether the best bid is at or above the best ask, which
// must never be observable after a command completes.
func (s Snapshot) Crossed() bool {
	return len(s.Bids) > 0 && len(s.Asks) > 0 && s.Bids[0].Price >= s.Asks[0].Price
}

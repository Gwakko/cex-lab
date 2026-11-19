// Package engine is a deterministic spot matching engine for one market.
//
// Prices are integer quote minimal units per lot and quantities are integer
// base minimal units, so the notional of a fill is price * (qty / lot) with no
// rounding.
package engine

import (
	"errors"
	"fmt"
	"math"
)

var (
	ErrInvalidOrder   = errors.New("engine: invalid order")
	ErrDuplicateOrder = errors.New("engine: duplicate order id")
	ErrUnknownOrder   = errors.New("engine: unknown order")
	ErrNotOwner       = errors.New("engine: order belongs to another user")
	ErrOutOfOrder     = errors.New("engine: command out of sequence")
	ErrStopped        = errors.New("engine: stopped")
)

type Side uint8

const (
	Buy Side = iota + 1
	Sell
)

func (s Side) String() string {
	switch s {
	case Buy:
		return "buy"
	case Sell:
		return "sell"
	}
	return fmt.Sprintf("side(%d)", s)
}

func (s Side) opposite() Side {
	if s == Buy {
		return Sell
	}
	return Buy
}

type OrderType uint8

func (t OrderType) String() string {
	switch t {
	case Limit:
		return "limit"
	case Market:
		return "market"
	}
	return fmt.Sprintf("type(%d)", t)
}

const (
	Limit OrderType = iota + 1
	// Market orders never rest. A market buy is bounded by QuoteBudget, so the
	// caller knows up front how much quote to reserve.
	Market
)

// MarketSpec defines the price and quantity grid of a market.
type MarketSpec struct {
	Symbol string
	Tick   int64 // minimal price increment, quote units per lot
	Lot    int64 // minimal quantity increment, base units
}

func (m MarketSpec) validate() error {
	if m.Symbol == "" || m.Tick <= 0 || m.Lot <= 0 {
		return fmt.Errorf("engine: invalid market spec %+v", m)
	}
	return nil
}

// Notional is the quote amount for qty at price. ok is false on overflow.
func (m MarketSpec) Notional(price, qty int64) (n int64, ok bool) {
	lots := qty / m.Lot
	if price > 0 && lots > math.MaxInt64/price {
		return 0, false
	}
	return price * lots, true
}

// Order is a client order. Remaining and Seq are set by the engine.
type Order struct {
	ID          string
	User        string
	Side        Side
	Type        OrderType
	Price       int64
	Qty         int64
	QuoteBudget int64
	Remaining   int64
	Seq         uint64
}

// Validate checks an order against the market grid before it is accepted.
func (m MarketSpec) Validate(o Order) error {
	bad := func(format string, args ...any) error {
		return fmt.Errorf("%w: %s", ErrInvalidOrder, fmt.Sprintf(format, args...))
	}
	switch {
	case o.ID == "" || o.User == "":
		return bad("id and user required")
	case o.Side != Buy && o.Side != Sell:
		return bad("unknown side %d", o.Side)
	case o.Qty <= 0 || o.Qty%m.Lot != 0:
		return bad("qty %d is not a positive multiple of lot %d", o.Qty, m.Lot)
	}
	switch o.Type {
	case Limit:
		if o.Price <= 0 || o.Price%m.Tick != 0 {
			return bad("price %d is not a positive multiple of tick %d", o.Price, m.Tick)
		}
		if o.QuoteBudget != 0 {
			return bad("limit orders take no quote budget")
		}
		if _, ok := m.Notional(o.Price, o.Qty); !ok {
			return bad("notional overflows")
		}
	case Market:
		if o.Price != 0 {
			return bad("market orders take no price")
		}
		if (o.Side == Buy) != (o.QuoteBudget > 0) || o.QuoteBudget < 0 {
			return bad("market buys need a positive quote budget, sells none")
		}
	default:
		return bad("unknown type %d", o.Type)
	}
	return nil
}

// Trade is one fill between a resting maker and an incoming taker. Price is
// always the maker's price.
type Trade struct {
	Seq          uint64
	MakerOrderID string
	TakerOrderID string
	MakerUser    string
	TakerUser    string
	TakerSide    Side
	Price        int64
	Qty          int64
}

type CommandType uint8

func (t CommandType) String() string {
	switch t {
	case Place:
		return "place"
	case Cancel:
		return "cancel"
	}
	return fmt.Sprintf("command(%d)", t)
}

const (
	Place CommandType = iota + 1
	Cancel
)

// Command is the unit written to the command log. For Cancel only Order.ID and
// Order.User are used.
type Command struct {
	Seq     uint64
	Type    CommandType
	Order   Order
	TraceID string `json:",omitempty"` // correlation only; never affects matching
}

// Result is the outcome of one command. For Place, Order is the taker after
// matching; for Cancel, it is the removed order.
type Result struct {
	Seq     uint64
	Order   Order
	Trades  []Trade
	Resting bool
	Err     error
}

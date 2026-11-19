// Package service is the trading application service. It drives the
// matching engines and reserves and settles funds through LedgerPort:
// place = hold -> match -> settle each trade -> release leftovers;
// cancel = cancel in the engine -> release the remaining reserve.
package service

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math/bits"
	"slices"
	"sync"
	"time"

	"github.com/gwakko/cex-lab/internal/platform/trace"
	"github.com/gwakko/cex-lab/internal/trading/internal/engine"
)

var (
	ErrUnknownMarket = errors.New("exchange: unknown market")
	// ErrInsufficientFunds is returned (wrapped) by LedgerPort when a reserve
	// cannot be taken.
	ErrInsufficientFunds = errors.New("trading: insufficient funds")
)

// MarketConfig describes one spot market.
type MarketConfig struct {
	Symbol      string
	Base        string
	Quote       string
	Tick        int64
	Lot         int64
	MakerFeeBps int64
	TakerFeeBps int64
	Log         engine.Log
}

type market struct {
	cfg  MarketConfig
	spec engine.MarketSpec
	eng  *engine.Engine
	// mu serialises the hold -> match -> settle pipeline of this market, so
	// ledger reserves and the book agree whenever no call is in progress.
	mu     sync.Mutex
	nextID uint64
}

// Exchange is the application service in front of the ledger and engines.
type Exchange struct {
	ledger        LedgerPort
	markets       map[string]*market
	symbols       []string // sorted; fixed lock order
	metrics       Metrics
	engineMetrics engine.Metrics
	marketData    MarketDataSink
	log           *slog.Logger

	checkMu   sync.Mutex
	lastCheck error
}

// Option configures an Exchange.
type Option func(*Exchange)

// WithMetrics reports order, trade and invariant measurements to m.
func WithMetrics(m Metrics) Option { return func(x *Exchange) { x.metrics = m } }

// WithLogger logs order lifecycle events.
func WithLogger(l *slog.Logger) Option { return func(x *Exchange) { x.log = l } }

// WithEngineMetrics reports every market engine's measurements to m.
func WithEngineMetrics(m engine.Metrics) Option { return func(x *Exchange) { x.engineMetrics = m } }

// WithMarketData publishes trades and book changes to sink.
func WithMarketData(sink MarketDataSink) Option { return func(x *Exchange) { x.marketData = sink } }

// New starts one engine per market, recovering each from its log.
func New(l LedgerPort, cfgs []MarketConfig, opts ...Option) (*Exchange, error) {
	x := &Exchange{ledger: l, markets: map[string]*market{}, metrics: nopMetrics{}, log: slog.New(slog.DiscardHandler)}
	for _, o := range opts {
		o(x)
	}
	for _, c := range cfgs {
		if err := validateConfig(c, x.markets); err != nil {
			x.Close()
			return nil, err
		}
		spec := engine.MarketSpec{Symbol: c.Symbol, Tick: c.Tick, Lot: c.Lot}
		engOpts := []engine.Option{engine.WithLogger(x.log)}
		if x.engineMetrics != nil {
			engOpts = append(engOpts, engine.WithMetrics(x.engineMetrics))
		}
		if x.marketData != nil {
			engOpts = append(engOpts, engine.WithListener(marketData{x.marketData}))
		}
		eng, err := engine.Start(spec, c.Log, engOpts...)
		if err != nil {
			x.Close()
			return nil, err
		}
		// Every accepted order consumes at least one engine sequence number,
		// so numbering from the last seq never reuses an earlier order id.
		x.markets[c.Symbol] = &market{cfg: c, spec: spec, eng: eng, nextID: eng.LastSeq() + 1}
		x.symbols = append(x.symbols, c.Symbol)
	}
	slices.Sort(x.symbols)
	return x, nil
}

func validateConfig(c MarketConfig, existing map[string]*market) error {
	switch {
	case existing[c.Symbol] != nil:
		return fmt.Errorf("exchange: duplicate market %s", c.Symbol)
	case c.Base == "" || c.Quote == "" || c.Base == c.Quote:
		return fmt.Errorf("exchange: %s needs two distinct assets", c.Symbol)
	case c.MakerFeeBps < 0 || c.MakerFeeBps > 10_000 || c.TakerFeeBps < 0 || c.TakerFeeBps > 10_000:
		return fmt.Errorf("exchange: %s fee out of range", c.Symbol)
	case c.Log == nil:
		return fmt.Errorf("exchange: %s has no command log", c.Symbol)
	}
	return nil
}

// Close stops all engines.
func (x *Exchange) Close() {
	for _, m := range x.markets {
		m.eng.Stop()
	}
}

// Book returns a copy of a market's resting orders (market data query).
func (x *Exchange) Book(ctx context.Context, symbol string) (engine.Snapshot, error) {
	m, err := x.market(symbol)
	if err != nil {
		return engine.Snapshot{}, err
	}
	return m.eng.Snapshot(ctx)
}

func (x *Exchange) market(symbol string) (*market, error) {
	m, ok := x.markets[symbol]
	if !ok {
		return nil, fmt.Errorf("%w: %s", ErrUnknownMarket, symbol)
	}
	return m, nil
}

// PlaceRequest is a new order from a user.
type PlaceRequest struct {
	Market      string
	User        string
	Side        engine.Side
	Type        engine.OrderType
	Price       int64
	Qty         int64
	QuoteBudget int64
}

// PlaceResult reports what happened to the order.
type PlaceResult struct {
	OrderID   string
	Trades    []engine.Trade
	Remaining int64
	Resting   bool
}

// Place reserves funds, matches the order and settles every trade. The trace
// id in ctx (created if absent) follows the order into the engine command,
// the ledger transactions and their outbox events.
func (x *Exchange) Place(ctx context.Context, req PlaceRequest) (res PlaceResult, err error) {
	start := time.Now()
	ctx, traceID := trace.Ensure(ctx)
	log := x.log.With("trace_id", traceID, "market", req.Market, "user_id", req.User)
	m, err := x.market(req.Market)
	if err != nil {
		x.metrics.OrderRejected("unknown", rejectReason(err))
		log.Info("order rejected", "error", err)
		return PlaceResult{}, err
	}
	defer func() {
		x.metrics.PlaceDuration(m.cfg.Symbol, time.Since(start))
		if err != nil {
			x.metrics.OrderRejected(m.cfg.Symbol, rejectReason(err))
			log.Info("order rejected", "reason", rejectReason(err), "error", err, "duration_ms", msSince(start))
			return
		}
		x.metrics.OrderPlaced(m.cfg.Symbol, req.Side.String(), req.Type.String())
		log.Info("order placed", "order_id", res.OrderID, "side", req.Side.String(), "type", req.Type.String(),
			"price", req.Price, "qty", req.Qty, "trades", len(res.Trades), "resting", res.Resting, "duration_ms", msSince(start))
	}()
	return x.place(ctx, m, req, start)
}

func (x *Exchange) place(ctx context.Context, m *market, req PlaceRequest, start time.Time) (PlaceResult, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	o := engine.Order{
		ID:          fmt.Sprintf("%s-%d", m.cfg.Symbol, m.nextID),
		User:        req.User,
		Side:        req.Side,
		Type:        req.Type,
		Price:       req.Price,
		Qty:         req.Qty,
		QuoteBudget: req.QuoteBudget,
	}
	if err := m.spec.Validate(o); err != nil {
		return PlaceResult{}, err
	}
	asset, reserve := m.initialReserve(o)
	replayed, err := x.ledger.Hold(ctx, "hold:"+o.ID, o.User, asset, reserve)
	if err != nil {
		return PlaceResult{}, fmt.Errorf("exchange: reserve for %s: %w", o.ID, err)
	}
	m.nextID++
	if replayed {
		return PlaceResult{}, fmt.Errorf("exchange: order id %s was already used", o.ID)
	}

	res, err := m.eng.Submit(ctx, engine.Command{Type: engine.Place, Order: o, TraceID: trace.ID(ctx)})
	if err == nil {
		err = res.Err
	}
	if err != nil {
		return PlaceResult{}, errors.Join(err, x.release(ctx, o.ID+":rejected", o.User, asset, reserve))
	}
	if len(res.Trades) > 0 {
		x.metrics.OrderToTrade(m.cfg.Symbol, time.Since(start))
	}

	for i, t := range res.Trades {
		used, err := x.settle(ctx, m, o, t, i)
		if err != nil {
			return PlaceResult{}, fmt.Errorf("exchange: settle %s trade %d (ledger and book diverged): %w", o.ID, i, err)
		}
		reserve -= used
	}
	if !res.Resting {
		if err := x.release(ctx, o.ID+":unfilled", o.User, asset, reserve); err != nil {
			return PlaceResult{}, err
		}
	}
	return PlaceResult{OrderID: o.ID, Trades: res.Trades, Remaining: res.Order.Remaining, Resting: res.Resting}, nil
}

// Cancel removes a resting order and releases what it still reserves.
func (x *Exchange) Cancel(ctx context.Context, symbol, user, orderID string) (engine.Order, error) {
	ctx, traceID := trace.Ensure(ctx)
	log := x.log.With("trace_id", traceID, "market", symbol, "user_id", user, "order_id", orderID)
	m, err := x.market(symbol)
	if err != nil {
		return engine.Order{}, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()

	res, err := m.eng.Submit(ctx, engine.Command{Type: engine.Cancel, Order: engine.Order{ID: orderID, User: user}, TraceID: traceID})
	if err == nil {
		err = res.Err
	}
	if err != nil {
		log.Info("cancel rejected", "error", err)
		return engine.Order{}, err
	}
	asset, amount := m.restingReserve(res.Order)
	if err := x.release(ctx, orderID+":cancel", user, asset, amount); err != nil {
		return engine.Order{}, err
	}
	x.metrics.OrderCancelled(m.cfg.Symbol)
	log.Info("order cancelled", "remaining", res.Order.Remaining)
	return res.Order, nil
}

// initialReserve is what an order locks before matching.
func (m *market) initialReserve(o engine.Order) (string, int64) {
	switch {
	case o.Side == engine.Sell:
		return m.cfg.Base, o.Qty
	case o.Type == engine.Market:
		return m.cfg.Quote, o.QuoteBudget
	default:
		n, _ := m.spec.Notional(o.Price, o.Qty) // checked by Validate
		return m.cfg.Quote, n
	}
}

// restingReserve is what a resting limit order still locks.
func (m *market) restingReserve(o engine.Order) (string, int64) {
	if o.Side == engine.Sell {
		return m.cfg.Base, o.Remaining
	}
	n, _ := m.spec.Notional(o.Price, o.Remaining)
	return m.cfg.Quote, n
}

// settle books one trade and returns how much of the taker's reserve it used.
// A limit buyer reserved at its own price; filling at a better maker price
// refunds the difference in the same ledger transaction.
func (x *Exchange) settle(ctx context.Context, m *market, taker engine.Order, t engine.Trade, i int) (int64, error) {
	notional, _ := m.spec.Notional(t.Price, t.Qty)
	s := Settlement{
		Key:      fmt.Sprintf("settle:%s:%d:%d", m.cfg.Symbol, t.Seq, i),
		Base:     m.cfg.Base,
		Quote:    m.cfg.Quote,
		BaseQty:  t.Qty,
		QuoteQty: notional,
	}
	buyerBps, sellerBps := m.cfg.MakerFeeBps, m.cfg.TakerFeeBps
	used := t.Qty
	if t.TakerSide == engine.Buy {
		s.Buyer, s.Seller = t.TakerUser, t.MakerUser
		buyerBps, sellerBps = m.cfg.TakerFeeBps, m.cfg.MakerFeeBps
		used = notional
		if taker.Type == engine.Limit {
			atLimit, _ := m.spec.Notional(taker.Price, t.Qty)
			s.BuyerRefund = atLimit - notional
			used = atLimit
		}
	} else {
		s.Buyer, s.Seller = t.MakerUser, t.TakerUser
	}
	s.BuyerFee = fee(s.BaseQty, buyerBps)
	s.SellerFee = fee(s.QuoteQty, sellerBps)
	if err := x.ledger.Settle(ctx, s); err != nil {
		return 0, err
	}
	x.metrics.Trade(m.cfg.Symbol, m.cfg.Base, m.cfg.Quote, s.BaseQty, s.QuoteQty)
	return used, nil
}

func (x *Exchange) release(ctx context.Context, key, user, asset string, amount int64) error {
	if amount == 0 {
		return nil
	}
	return x.ledger.Release(ctx, "release:"+key, user, asset, amount)
}

// fee is floor(amount * bps / 10000) without intermediate overflow.
func fee(amount, bps int64) int64 {
	hi, lo := bits.Mul64(uint64(amount), uint64(bps))
	q, _ := bits.Div64(hi, lo, 10_000)
	return int64(q)
}

// rejectReason maps an error to a bounded label value.
func rejectReason(err error) string {
	switch {
	case errors.Is(err, ErrInsufficientFunds):
		return "insufficient_funds"
	case errors.Is(err, engine.ErrInvalidOrder):
		return "invalid_order"
	case errors.Is(err, ErrUnknownMarket):
		return "unknown_market"
	case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
		return "timeout"
	}
	return "internal"
}

func msSince(t time.Time) float64 { return float64(time.Since(t).Microseconds()) / 1000 }

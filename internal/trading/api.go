// Package trading is the trading module: spot order books with price-time
// priority, one single-writer engine per market with a sequenced command
// log, and the order service that reserves and settles funds through
// LedgerPort.
//
// This file is the module's public API. The implementation lives in
// internal/engine and internal/service.
package trading

import (
	"log/slog"

	"github.com/gwakko/cex-lab/internal/trading/internal/engine"
	"github.com/gwakko/cex-lab/internal/trading/internal/service"
)

// Service places and cancels orders, answers book queries and checks the
// cross-module invariants. Methods: Place, Cancel, Book, Check,
// LastCheckError, Close.
type Service = service.Exchange

// New starts one engine per market, recovering each from its command log.
func New(l LedgerPort, markets []MarketConfig, opts ...Option) (*Service, error) {
	return service.New(l, markets, opts...)
}

// Option configures a Service.
type Option = service.Option

// WithMetrics reports order, trade and invariant measurements.
func WithMetrics(m Metrics) Option { return service.WithMetrics(m) }

// WithEngineMetrics reports per-market engine measurements.
func WithEngineMetrics(m EngineMetrics) Option { return service.WithEngineMetrics(m) }

// WithMarketData publishes public trades and book deltas to sink.
func WithMarketData(sink MarketDataSink) Option { return service.WithMarketData(sink) }

// WithLogger logs order lifecycle and engine commands.
func WithLogger(l *slog.Logger) Option { return service.WithLogger(l) }

// Requests, results and order types.
type (
	MarketConfig   = service.MarketConfig
	PlaceRequest   = service.PlaceRequest
	PlaceResult    = service.PlaceResult
	InvariantError = service.InvariantError
	Side           = engine.Side
	OrderType      = engine.OrderType
	Order          = engine.Order
	Trade          = engine.Trade
	BookSnapshot   = engine.Snapshot
	Command        = engine.Command
)

const (
	Buy    = engine.Buy
	Sell   = engine.Sell
	Limit  = engine.Limit
	Market = engine.Market
)

// Errors.
var (
	ErrUnknownMarket     = service.ErrUnknownMarket
	ErrInsufficientFunds = service.ErrInsufficientFunds
	ErrInvalidOrder      = engine.ErrInvalidOrder
	ErrUnknownOrder      = engine.ErrUnknownOrder
	ErrNotOwner          = engine.ErrNotOwner
	ErrStopped           = engine.ErrStopped
)

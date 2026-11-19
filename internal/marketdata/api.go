// Package marketdata is the market data module: public trades and top-N order
// book channels with per-market sequence numbers, built only from
// internal/events. Transport adapters (adapters/ws) serve Sessions.
//
// This file is the module's public API. The implementation lives in
// internal/hub.
package marketdata

import (
	"log/slog"

	"github.com/gwakko/cex-lab/internal/marketdata/internal/hub"
)

// Hub keeps book replicas and fans messages out to sessions. Methods: Seed,
// PublishTrade, PublishBookDelta, Connect, TopN, Close. It implements
// trading's MarketDataSink.
type Hub = hub.Hub

// New returns a hub for cfg.Markets.
func New(cfg Config, opts ...Option) *Hub { return hub.New(cfg, opts...) }

// Option configures a Hub.
type Option = hub.Option

// WithMetrics reports hub measurements to m.
func WithMetrics(m Metrics) Option { return hub.WithMetrics(m) }

// WithLogger logs published trades, drops and shutdown.
func WithLogger(l *slog.Logger) Option { return hub.WithLogger(l) }

// Sessions, protocol and client-side helpers.
type (
	Config      = hub.Config
	Session     = hub.Session
	Message     = hub.Message
	Request     = hub.Request
	Envelope    = hub.Envelope
	BookReplica = hub.BookReplica
)

// NewBookReplica returns an unsynced client-side book for market.
func NewBookReplica(market string) *BookReplica { return hub.NewBookReplica(market) }

// TradesChannel and BookChannel name the public channels of a market.
func TradesChannel(market string) string { return hub.TradesChannel(market) }
func BookChannel(market string) string   { return hub.BookChannel(market) }

// Message types and session end reasons.
const (
	TypeSubscribed   = hub.TypeSubscribed
	TypeUnsubscribed = hub.TypeUnsubscribed
	TypeError        = hub.TypeError
	TypeBookSnapshot = hub.TypeBookSnapshot
	TypeBookUpdate   = hub.TypeBookUpdate
	TypeTrade        = hub.TypeTrade

	ReasonSlowConsumer = hub.ReasonSlowConsumer
	ReasonShutdown     = hub.ReasonShutdown
	ReasonClosed       = hub.ReasonClosed
)

// Errors.
var (
	ErrGap                = hub.ErrGap
	ErrClosed             = hub.ErrClosed
	ErrTooManyConnections = hub.ErrTooManyConnections
)

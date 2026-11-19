package trading

import (
	"github.com/gwakko/cex-lab/internal/trading/internal/engine"
	"github.com/gwakko/cex-lab/internal/trading/internal/service"
)

// LedgerPort is implemented outside the module (internal/app adapts the
// ledger to it). Trading never imports the ledger.
type (
	LedgerPort = service.LedgerPort
	Settlement = service.Settlement
	Holding    = service.Holding
)

// MarketDataSink receives public trades and book deltas (internal/app passes
// the marketdata module). It must not block.
type MarketDataSink = service.MarketDataSink

// CommandLog stores engine commands before they are applied (adapters:
// adapters/filelog, adapters/memlog).
type CommandLog = engine.Log

// Metrics and EngineMetrics are implemented by internal/app/instrument.
type (
	Metrics       = service.Metrics
	EngineMetrics = engine.Metrics
)

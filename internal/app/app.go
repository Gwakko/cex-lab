// Package app is the composition root. It is the only place that knows every
// module and every adapter: it builds the ledger, trading and custody
// modules, picks file or in-memory adapters, connects trading and custody to
// the ledger through their ports, and attaches metrics, logs and health
// checks. Modules never import it.
package app

import (
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"path/filepath"

	"github.com/gwakko/cex-lab/internal/app/instrument"
	"github.com/gwakko/cex-lab/internal/custody"
	"github.com/gwakko/cex-lab/internal/ledger"
	"github.com/gwakko/cex-lab/internal/ledger/adapters/filestore"
	"github.com/gwakko/cex-lab/internal/ledger/adapters/memstore"
	"github.com/gwakko/cex-lab/internal/platform/outbox"
	"github.com/gwakko/cex-lab/internal/platform/telemetry"
	"github.com/gwakko/cex-lab/internal/trading"
	"github.com/gwakko/cex-lab/internal/trading/adapters/filelog"
	"github.com/gwakko/cex-lab/internal/trading/adapters/memlog"
)

// Config selects adapters and markets.
type Config struct {
	// DataDir holds the ledger journal and one command log per market. Empty
	// means in-memory adapters (nothing survives the process).
	DataDir string
	// Markets to run. Log is filled in by New and must be left nil.
	Markets []trading.MarketConfig
	// Logger receives every module's logs; nil discards them.
	Logger *slog.Logger
}

// App is the wired system.
type App struct {
	Ledger   *ledger.Ledger
	Trading  *trading.Service
	Registry *telemetry.Registry
	Health   *telemetry.Health

	logger  *slog.Logger
	outbox  *instrument.Outbox
	closers []func() error
}

// New builds and starts every module.
func New(cfg Config) (_ *App, err error) {
	a := &App{Registry: telemetry.NewRegistry(), Health: &telemetry.Health{}, logger: cfg.Logger}
	if a.logger == nil {
		a.logger = slog.New(slog.DiscardHandler)
	}
	a.outbox = instrument.NewOutbox(a.Registry)
	defer func() {
		if err != nil {
			a.Close()
		}
	}()

	store, err := a.ledgerStore(cfg.DataDir)
	if err != nil {
		return nil, err
	}
	a.Ledger, err = ledger.Open(store, ledger.WithMetrics(instrument.NewLedger(a.Registry)), ledger.WithLogger(a.logger))
	if err != nil {
		return nil, err
	}

	markets := make([]trading.MarketConfig, len(cfg.Markets))
	for i, m := range cfg.Markets {
		if m.Log != nil {
			return nil, fmt.Errorf("app: market %s: Log is chosen by app, leave it nil", m.Symbol)
		}
		if m.Log, err = a.commandLog(cfg.DataDir, m.Symbol); err != nil {
			return nil, err
		}
		markets[i] = m
	}
	a.Trading, err = trading.New(NewLedgerPort(a.Ledger), markets,
		trading.WithMetrics(instrument.NewExchange(a.Registry)),
		trading.WithEngineMetrics(instrument.NewEngine(a.Registry)),
		trading.WithLogger(a.logger))
	if err != nil {
		return nil, err
	}
	a.Health.AddReadinessCheck("invariants", a.Trading.LastCheckError)
	return a, nil
}

func (a *App) ledgerStore(dir string) (ledger.Store, error) {
	if dir == "" {
		return &memstore.Store{}, nil
	}
	fs, err := filestore.Open(filepath.Join(dir, "ledger.journal"), instrument.NewJournal(a.Registry, "ledger"))
	if err != nil {
		return nil, err
	}
	a.closers = append(a.closers, fs.Close)
	a.Health.AddReadinessCheck("ledger_journal", fs.Err)
	return fs, nil
}

func (a *App) commandLog(dir, symbol string) (trading.CommandLog, error) {
	if dir == "" {
		return &memlog.Log{}, nil
	}
	name := "engine_" + symbol
	fl, err := filelog.Open(filepath.Join(dir, symbol+".cmdlog"), instrument.NewJournal(a.Registry, name))
	if err != nil {
		return nil, err
	}
	a.closers = append(a.closers, fl.Close)
	a.Health.AddReadinessCheck("engine_log_"+symbol, fl.Err)
	return fl, nil
}

// NewDepositWatcher builds a custody watcher that credits the ledger.
// owners maps each watched address to its user; cfg.Metrics and cfg.Logger
// are set by the app.
func (a *App) NewDepositWatcher(client custody.ChainClient, cursor custody.CursorStore, owners map[string]string, cfg custody.Config) (*custody.Watcher, error) {
	cfg.Metrics = instrument.NewChain(a.Registry)
	cfg.Logger = a.logger
	return custody.NewWatcher(client, cursor, NewDepositCreditor(a.Ledger, owners), cfg)
}

// NewRelay drains the ledger outbox into deliver from offset. opts are
// applied after the app's defaults (metrics, logger), so they can override.
func (a *App) NewRelay(deliver outbox.Handler, offset int, opts ...outbox.Option) *outbox.Relay {
	base := []outbox.Option{outbox.WithMetrics(a.outbox), outbox.WithLogger(a.logger)}
	return outbox.NewRelay(a.Ledger, deliver, offset, append(base, opts...)...)
}

// NewInbox wraps a consumer handler with dedup and duplicate metrics.
func (a *App) NewInbox(h outbox.Handler, opts ...outbox.Option) *outbox.Inbox {
	return outbox.NewInbox(h, append([]outbox.Option{outbox.WithMetrics(a.outbox)}, opts...)...)
}

// Handler serves /metrics, /healthz and /readyz.
func (a *App) Handler() http.Handler { return telemetry.NewMux(a.Registry, a.Health) }

// Close stops the engines and closes the files.
func (a *App) Close() error {
	if a.Trading != nil {
		a.Trading.Close()
	}
	var errs []error
	for _, c := range a.closers {
		errs = append(errs, c())
	}
	return errors.Join(errs...)
}

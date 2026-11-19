package instrument

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"github.com/gwakko/cex-lab/internal/app"
	"github.com/gwakko/cex-lab/internal/events"
	"github.com/gwakko/cex-lab/internal/ledger"
	"github.com/gwakko/cex-lab/internal/ledger/adapters/filestore"
	"github.com/gwakko/cex-lab/internal/platform/telemetry"
	"github.com/gwakko/cex-lab/internal/platform/trace"
	"github.com/gwakko/cex-lab/internal/trading"
	"github.com/gwakko/cex-lab/internal/trading/adapters/memlog"
	"github.com/gwakko/cex-lab/outbox"
)

const market = "BTC-USDT"

type stack struct {
	reg    *telemetry.Registry
	logs   *bytes.Buffer
	logger *slog.Logger
	store  *filestore.Store
	l      *ledger.Ledger
	x      *trading.Service
	health *telemetry.Health
}

func newStack(t *testing.T) *stack {
	t.Helper()
	s := &stack{reg: telemetry.NewRegistry(), logs: &bytes.Buffer{}, health: &telemetry.Health{}}
	s.logger = slog.New(slog.NewJSONHandler(s.logs, nil))
	var err error
	s.store, err = filestore.Open(filepath.Join(t.TempDir(), "ledger.journal"), NewJournal(s.reg, "ledger"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.store.Close() })
	s.l, err = ledger.Open(s.store, ledger.WithMetrics(NewLedger(s.reg)), ledger.WithLogger(s.logger))
	if err != nil {
		t.Fatal(err)
	}
	s.x, err = trading.New(app.NewLedgerPort(s.l), []trading.MarketConfig{{
		Symbol: market, Base: "BTC", Quote: "USDT", Tick: 1, Lot: 10, MakerFeeBps: 10, TakerFeeBps: 20, Log: &memlog.Log{},
	}},
		trading.WithMetrics(NewExchange(s.reg)),
		trading.WithLogger(s.logger),
		trading.WithEngineMetrics(NewEngine(s.reg)))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s.x.Close)
	s.health.AddReadinessCheck("invariants", s.x.LastCheckError)
	s.health.AddReadinessCheck("ledger_journal", s.store.Err)
	return s
}

func (s *stack) deposit(t *testing.T, key, user string, asset ledger.Asset, amount int64) {
	t.Helper()
	if _, _, err := s.l.Apply(ledger.Deposit{Key: key, User: user, Asset: asset, Amount: amount}); err != nil {
		t.Fatal(err)
	}
}

func (s *stack) place(ctx context.Context, user string, side trading.Side, price, qty int64) (trading.PlaceResult, error) {
	return s.x.Place(ctx, trading.PlaceRequest{Market: market, User: user, Side: side, Type: trading.Limit, Price: price, Qty: qty})
}

func TestExchangeScenarioMetrics(t *testing.T) {
	s := newStack(t)
	ctx := context.Background()
	s.deposit(t, "dep-alice", "alice", "USDT", 1_000_000)
	s.deposit(t, "dep-bob", "bob", "BTC", 1_000)
	s.deposit(t, "dep-alice", "alice", "USDT", 1_000_000) // client retry

	ask, err := s.place(ctx, "bob", trading.Sell, 100, 100)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.place(trace.WithID(ctx, "order-trace-1"), "alice", trading.Buy, 100, 50); err != nil {
		t.Fatal(err)
	}
	if _, err := s.place(ctx, "carol", trading.Buy, 100, 10); !errors.Is(err, ledger.ErrInsufficientFunds) {
		t.Fatalf("carol: %v", err)
	}
	if _, err := s.place(ctx, "alice", trading.Buy, 100, 15); !errors.Is(err, trading.ErrInvalidOrder) {
		t.Fatalf("off-lot: %v", err)
	}
	if _, err := s.x.Cancel(ctx, market, "bob", ask.OrderID); err != nil {
		t.Fatal(err)
	}
	if err := s.x.Check(ctx); err != nil {
		t.Fatal(err)
	}

	r := s.reg
	wantMetric(t, r, 1, "ledger_commands_total", "deposit", ledger.ResultReplay) // exactly one retry
	wantMetric(t, r, 2, "ledger_commands_total", "deposit", ledger.ResultApplied)
	wantMetric(t, r, 1, "exchange_orders_placed_total", market, "sell", "limit")
	wantMetric(t, r, 1, "exchange_orders_placed_total", market, "buy", "limit")
	wantMetric(t, r, 1, "exchange_orders_rejected_total", market, "insufficient_funds")
	wantMetric(t, r, 1, "exchange_orders_rejected_total", market, "invalid_order")
	wantMetric(t, r, 1, "exchange_orders_cancelled_total", market)
	wantMetric(t, r, 1, "exchange_trades_total", market)
	wantMetric(t, r, 50, "exchange_trade_volume_total", market, "BTC")
	wantMetric(t, r, 500, "exchange_trade_volume_total", market, "USDT")
	wantMetric(t, r, 1, "exchange_order_to_trade_latency_seconds", market)
	wantMetric(t, r, 4, "exchange_place_order_duration_seconds", market)
	wantMetric(t, r, 2, "engine_commands_applied_total", market, "place") // rejected orders never reach the engine
	wantMetric(t, r, 1, "engine_commands_applied_total", market, "cancel")
	wantMetric(t, r, 1, "invariant_check_runs_total")
	wantMetric(t, r, 1, "invariant_ok")
	if metric(t, r, "last_invariant_check_timestamp_seconds") == 0 {
		t.Error("last check timestamp not set")
	}
	// 2 deposits, 2 holds, 1 settle, 1 cancel release: one fsync per committed tx.
	wantMetric(t, r, 6, "journal_fsync_duration_seconds", "ledger")
	wantMetric(t, r, float64(len(s.l.EventsSince(0))), "journal_fsync_duration_seconds", "ledger")
}

func TestOneOrderIsTraceableAcrossLayers(t *testing.T) {
	s := newStack(t)
	ctx := context.Background()
	s.deposit(t, "d1", "alice", "USDT", 1_000_000)
	s.deposit(t, "d2", "bob", "BTC", 1_000)
	s.place(ctx, "bob", trading.Sell, 100, 100)
	if _, err := s.place(trace.WithID(ctx, "order-trace-1"), "alice", trading.Buy, 101, 50); err != nil {
		t.Fatal(err)
	}
	relay := outbox.NewRelay(s.l, func(events.Event) error { return nil }, 0, outbox.WithLogger(s.logger))
	if _, err := relay.Drain(); err != nil {
		t.Fatal(err)
	}

	seen := map[string]int{}
	for _, line := range logLines(t, s.logs) {
		if line["trace_id"] == "order-trace-1" {
			seen[line["msg"].(string)]++
		}
	}
	// hold + settle (with refund) for the taker: two ledger txs, two events.
	want := map[string]int{"order placed": 1, "engine command applied": 1, "ledger tx committed": 2, "outbox event published": 2}
	for msg, n := range want {
		if seen[msg] != n {
			t.Errorf("%q with trace id: %d lines, want %d (all: %v)", msg, seen[msg], n, seen)
		}
	}
}

func TestInjectedInvariantViolationFlipsReadiness(t *testing.T) {
	s := newStack(t)
	ctx := context.Background()
	srv := httptest.NewServer(telemetry.NewMux(s.reg, s.health))
	defer srv.Close()
	ready := func() int {
		resp, err := srv.Client().Get(srv.URL + "/readyz")
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		return resp.StatusCode
	}

	s.deposit(t, "d1", "alice", "USDT", 1_000)
	if err := s.x.Check(ctx); err != nil {
		t.Fatal(err)
	}
	if code := ready(); code != http.StatusOK {
		t.Fatalf("ready before violation: %d", code)
	}

	// Lock funds behind the exchange's back: locked no longer equals resting reserves.
	if _, _, err := s.l.Apply(ledger.Hold{Key: "rogue", User: "alice", Asset: "USDT", Amount: 10}); err != nil {
		t.Fatal(err)
	}
	err := s.x.Check(ctx)
	var ie *trading.InvariantError
	if !errors.As(err, &ie) || ie.Invariant != "locked_equals_reserves" {
		t.Fatalf("expected locked_equals_reserves violation, got %v", err)
	}
	wantMetric(t, s.reg, 0, "invariant_ok")
	wantMetric(t, s.reg, 1, "invariant_violations_total", "locked_equals_reserves")
	wantMetric(t, s.reg, 2, "invariant_check_runs_total")
	if code := ready(); code != http.StatusServiceUnavailable {
		t.Fatalf("readyz after violation: %d", code)
	}
}

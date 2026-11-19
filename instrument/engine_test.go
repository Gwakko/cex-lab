package instrument

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"sync"
	"testing"

	"github.com/gwakko/cex-lab/engine"
	"github.com/gwakko/cex-lab/internal/platform/telemetry"
)

var spec = engine.MarketSpec{Symbol: "BTC-USDT", Tick: 1, Lot: 1}

func limit(id string, side engine.Side, price, qty int64) engine.Command {
	return engine.Command{Type: engine.Place, Order: engine.Order{ID: id, User: "u", Side: side, Type: engine.Limit, Price: price, Qty: qty}}
}

func TestEngineMetricsAndLogs(t *testing.T) {
	reg := telemetry.NewRegistry()
	var logs bytes.Buffer
	e, err := engine.Start(spec, &engine.MemLog{},
		engine.WithMetrics(NewEngine(reg)), engine.WithLogger(slog.New(slog.NewJSONHandler(&logs, nil))))
	if err != nil {
		t.Fatal(err)
	}
	defer e.Stop()
	ctx := context.Background()
	cmd := limit("a1", engine.Sell, 101, 5)
	cmd.TraceID = "trace-1"
	e.Submit(ctx, cmd)
	e.Submit(ctx, limit("a2", engine.Sell, 102, 5))
	e.Submit(ctx, limit("b1", engine.Buy, 99, 5))
	e.Submit(ctx, limit("bad", engine.Buy, 0, 5)) // rejected, still logged and applied
	e.Submit(ctx, engine.Command{Type: engine.Cancel, Order: engine.Order{ID: "b1", User: "u"}})

	wantMetric(t, reg, 4, "engine_commands_applied_total", "BTC-USDT", "place")
	wantMetric(t, reg, 1, "engine_commands_applied_total", "BTC-USDT", "cancel")
	wantMetric(t, reg, 5, "engine_match_duration_seconds", "BTC-USDT")
	wantMetric(t, reg, 5, "engine_last_seq", "BTC-USDT")
	wantMetric(t, reg, 0, "engine_book_depth", "BTC-USDT", "buy")
	wantMetric(t, reg, 2, "engine_book_depth", "BTC-USDT", "sell")
	wantMetric(t, reg, 0, "engine_command_queue_depth", "BTC-USDT")

	first := logLines(t, &logs)[0]
	if first["trace_id"] != "trace-1" || first["order_id"] != "a1" || first["market"] != "BTC-USDT" || first["seq"] != float64(1) {
		t.Fatalf("engine log %v", first)
	}
}

func TestEngineQueueDepthUnderLoad(t *testing.T) {
	reg := telemetry.NewRegistry()
	e, _ := engine.Start(spec, &engine.MemLog{}, engine.WithMetrics(NewEngine(reg)))
	defer e.Stop()
	var wg sync.WaitGroup
	for w := range 16 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range 50 {
				e.Submit(context.Background(), limit(fmt.Sprintf("w%d-%d", w, i), engine.Side(1+i%2), 100+int64(i%3), 1))
			}
		}()
	}
	wg.Wait()
	wantMetric(t, reg, 800, "engine_commands_applied_total", "BTC-USDT", "place")
	if q := metric(t, reg, "engine_command_queue_depth", "BTC-USDT"); q < 0 || q > 16 {
		t.Fatalf("queue depth out of range: %v", q)
	}
}

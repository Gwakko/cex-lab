package engine

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"
)

// recorder is a fake Metrics that keeps the last reported values.
type recorder struct {
	mu        sync.Mutex
	applied   map[string]int
	matches   int
	queue     int
	maxQueue  int
	bids      int
	asks      int
	lastSeq   uint64
	marketSet map[string]bool
}

func newRecorder() *recorder {
	return &recorder{applied: map[string]int{}, marketSet: map[string]bool{}}
}

func (r *recorder) QueueDepth(market string, n int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.marketSet[market] = true
	r.queue = n
	r.maxQueue = max(r.maxQueue, n)
}

func (r *recorder) CommandApplied(market, typ string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.marketSet[market] = true
	r.applied[typ]++
}

func (r *recorder) MatchDuration(string, time.Duration) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.matches++
}

func (r *recorder) BookDepth(_ string, bids, asks int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.bids, r.asks = bids, asks
}

func (r *recorder) LastSeq(_ string, seq uint64) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.lastSeq = seq
}

var unitSpec = MarketSpec{Symbol: "BTC-USDT", Tick: 1, Lot: 1}

func unitLimit(id string, side Side, price, qty int64) Command {
	return Command{Type: Place, Order: Order{ID: id, User: "u", Side: side, Type: Limit, Price: price, Qty: qty}}
}

func TestEngineMetricsAndLogs(t *testing.T) {
	rec := newRecorder()
	var logs bytes.Buffer
	e, err := Start(unitSpec, &memLog{}, WithMetrics(rec), WithLogger(slog.New(slog.NewJSONHandler(&logs, nil))))
	if err != nil {
		t.Fatal(err)
	}
	defer e.Stop()
	ctx := context.Background()
	cmd := unitLimit("a1", Sell, 101, 5)
	cmd.TraceID = "trace-1"
	e.Submit(ctx, cmd)
	e.Submit(ctx, unitLimit("a2", Sell, 102, 5))
	e.Submit(ctx, unitLimit("b1", Buy, 99, 5))
	e.Submit(ctx, unitLimit("bad", Buy, 0, 5)) // rejected, still logged and applied
	e.Submit(ctx, Command{Type: Cancel, Order: Order{ID: "b1", User: "u"}})

	rec.mu.Lock()
	got := fmt.Sprintf("place=%d cancel=%d matches=%d seq=%d bids=%d asks=%d queue=%d markets=%v",
		rec.applied["place"], rec.applied["cancel"], rec.matches, rec.lastSeq, rec.bids, rec.asks, rec.queue, rec.marketSet)
	rec.mu.Unlock()
	if want := "place=4 cancel=1 matches=5 seq=5 bids=0 asks=2 queue=0 markets=map[BTC-USDT:true]"; got != want {
		t.Fatalf("metrics %s, want %s", got, want)
	}

	first := strings.SplitN(logs.String(), "\n", 2)[0]
	var line map[string]any
	if err := json.Unmarshal([]byte(first), &line); err != nil {
		t.Fatal(err)
	}
	if line["trace_id"] != "trace-1" || line["order_id"] != "a1" || line["market"] != "BTC-USDT" || line["seq"] != float64(1) {
		t.Fatalf("engine log %v", line)
	}
}

func TestEngineQueueDepthUnderLoad(t *testing.T) {
	rec := newRecorder()
	e, _ := Start(unitSpec, &memLog{}, WithMetrics(rec))
	defer e.Stop()
	var wg sync.WaitGroup
	for w := range 16 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range 50 {
				e.Submit(context.Background(), unitLimit(fmt.Sprintf("w%d-%d", w, i), Side(1+i%2), 100+int64(i%3), 1))
			}
		}()
	}
	wg.Wait()
	rec.mu.Lock()
	defer rec.mu.Unlock()
	if rec.applied["place"] != 800 {
		t.Fatalf("applied %d, want 800", rec.applied["place"])
	}
	if rec.queue < 0 || rec.queue > 16 || rec.maxQueue > 16 {
		t.Fatalf("queue depth out of range: last %d, max %d", rec.queue, rec.maxQueue)
	}
}

package engine

import (
	"context"
	"maps"
	"math/rand/v2"
	"sync"
	"testing"

	"github.com/gwakko/cex-lab/internal/events"
)

type deltaCollector struct {
	mu      sync.Mutex
	bids    map[int64]int64
	asks    map[int64]int64
	lastSeq uint64
	trades  int
	t       *testing.T
}

func (c *deltaCollector) Applied(market string, cmd Command, res Result, bids, asks []events.Level) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if market != spec.Symbol || cmd.Seq <= c.lastSeq || res.Seq != cmd.Seq {
		c.t.Errorf("out of order or wrong market: %s seq %d after %d", market, cmd.Seq, c.lastSeq)
	}
	c.lastSeq = cmd.Seq
	c.trades += len(res.Trades)
	apply := func(side map[int64]int64, levels []events.Level) {
		for _, l := range levels {
			if l.Qty == 0 {
				delete(side, l.Price)
			} else {
				side[l.Price] = l.Qty
			}
		}
	}
	apply(c.bids, bids)
	apply(c.asks, asks)
}

func aggregate(orders []Order) map[int64]int64 {
	out := map[int64]int64{}
	for _, o := range orders {
		out[o.Price] += o.Remaining
	}
	return out
}

func TestListenerDeltasRebuildTheBook(t *testing.T) {
	for seed := range uint64(10) {
		c := &deltaCollector{bids: map[int64]int64{}, asks: map[int64]int64{}, t: t}
		e, err := Start(spec, &memLog{}, WithListener(c))
		if err != nil {
			t.Fatal(err)
		}
		ctx := context.Background()
		for _, cmd := range randomCommands(rand.New(rand.NewPCG(seed, 3)), 400) {
			if _, err := e.Submit(ctx, cmd); err != nil {
				t.Fatal(err)
			}
			snap, _ := e.Snapshot(ctx)
			c.mu.Lock()
			okB, okA := maps.Equal(c.bids, aggregate(snap.Bids)), maps.Equal(c.asks, aggregate(snap.Asks))
			c.mu.Unlock()
			if !okB || !okA {
				t.Fatalf("seed %d seq %d: replica from deltas differs from the book", seed, snap.Seq)
			}
		}
		e.Stop()
		if c.trades == 0 {
			t.Fatalf("seed %d produced no trades", seed)
		}
	}
}

func TestRejectedCommandsAreNotReported(t *testing.T) {
	c := &deltaCollector{bids: map[int64]int64{}, asks: map[int64]int64{}, t: t}
	e, _ := Start(spec, &memLog{}, WithListener(c))
	defer e.Stop()
	e.Submit(context.Background(), Command{Type: Place, Order: limit("bad", "u", Buy, 101, 10)}) // off tick
	e.Submit(context.Background(), Command{Type: Cancel, Order: Order{ID: "nope", User: "u"}})
	if c.lastSeq != 0 {
		t.Fatalf("rejected commands reached the listener (last seq %d)", c.lastSeq)
	}
}

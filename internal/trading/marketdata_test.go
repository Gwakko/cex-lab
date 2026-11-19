package trading_test

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"testing"

	"github.com/gwakko/cex-lab/internal/app"
	"github.com/gwakko/cex-lab/internal/events"
	"github.com/gwakko/cex-lab/internal/ledger"
	"github.com/gwakko/cex-lab/internal/ledger/adapters/memstore"
	"github.com/gwakko/cex-lab/internal/platform/trace"
	"github.com/gwakko/cex-lab/internal/trading"
)

type sink struct {
	mu     sync.Mutex
	trades []events.TradeV1
	deltas []events.BookDeltaV1
}

func (s *sink) PublishTrade(t events.TradeV1) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.trades = append(s.trades, t)
}

func (s *sink) PublishBookDelta(d events.BookDeltaV1) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.deltas = append(s.deltas, d)
}

func TestMarketDataSinkGetsPublicTradesAndDeltas(t *testing.T) {
	l := memstore.NewLedger()
	md := &sink{}
	x, err := trading.New(app.NewLedgerPort(l), testConfigs(), trading.WithMarketData(md))
	if err != nil {
		t.Fatal(err)
	}
	defer x.Close()
	l.Apply(ledger.Deposit{Key: "d1", User: "seller", Asset: btc, Amount: 100})
	l.Apply(ledger.Deposit{Key: "d2", User: "buyer", Asset: usdt, Amount: 1_000_000})
	ctx := context.Background()
	if _, err := x.Place(ctx, trading.PlaceRequest{Market: "BTC-USDT", User: "seller", Side: trading.Sell, Type: trading.Limit, Price: 100, Qty: 50}); err != nil {
		t.Fatal(err)
	}
	if _, err := x.Place(trace.WithID(ctx, "taker-trace"), trading.PlaceRequest{Market: "BTC-USDT", User: "buyer", Side: trading.Buy, Type: trading.Limit, Price: 100, Qty: 20}); err != nil {
		t.Fatal(err)
	}
	x.Place(ctx, trading.PlaceRequest{Market: "BTC-USDT", User: "nobody", Side: trading.Buy, Type: trading.Limit, Price: 100, Qty: 20}) // rejected: no funds

	md.mu.Lock()
	defer md.mu.Unlock()
	if len(md.trades) != 1 || md.trades[0].Qty != 20 || md.trades[0].TakerSide != "buy" || md.trades[0].TraceID != "taker-trace" {
		t.Fatalf("trades %+v", md.trades)
	}
	if len(md.deltas) != 2 {
		t.Fatalf("want 2 deltas (rest, then partial fill), got %+v", md.deltas)
	}
	if got := md.deltas[1].Asks; len(got) != 1 || got[0] != (events.Level{Price: 100, Qty: 30}) || md.deltas[1].Seq != md.trades[0].Seq {
		t.Fatalf("second delta %+v", md.deltas[1])
	}
	raw, _ := json.Marshal([]any{md.trades, md.deltas})
	for _, private := range []string{"seller", "buyer", "BTC-USDT-1", "BTC-USDT-2"} {
		if strings.Contains(string(raw), private) {
			t.Fatalf("public market data leaks %q: %s", private, raw)
		}
	}
}

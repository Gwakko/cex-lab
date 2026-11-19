package app

import (
	"context"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/gwakko/cex-lab/internal/ledger"
	"github.com/gwakko/cex-lab/internal/marketdata"
	"github.com/gwakko/cex-lab/internal/marketdata/adapters/ws"
	"github.com/gwakko/cex-lab/internal/platform/trace"
	"github.com/gwakko/cex-lab/internal/trading"
)

func TestMarketDataOverWebSocketMatchesTheBook(t *testing.T) {
	dir := t.TempDir()
	ctx := context.Background()
	place := func(a *App, user string, side trading.Side, price, qty int64) {
		t.Helper()
		if _, err := a.Trading.Place(ctx, trading.PlaceRequest{Market: "ETH-USDT", User: user, Side: side, Type: trading.Limit, Price: price, Qty: qty}); err != nil {
			t.Fatal(err)
		}
	}

	// Resting orders placed before a restart must appear in the first snapshot.
	a, err := New(Config{DataDir: dir, Markets: markets()})
	if err != nil {
		t.Fatal(err)
	}
	a.Ledger.Apply(ledger.Deposit{Key: "d1", User: "maker", Asset: "ETH", Amount: 100})
	a.Ledger.Apply(ledger.Deposit{Key: "d2", User: "taker", Asset: "USDT", Amount: 100_000})
	place(a, "maker", trading.Sell, 105, 10)
	place(a, "maker", trading.Sell, 106, 5)
	place(a, "taker", trading.Buy, 100, 3)
	a.Close()

	a, err = New(Config{DataDir: dir, Markets: markets()})
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	srv := httptest.NewServer(a.Handler())
	defer srv.Close()
	c, err := ws.Dial(ctx, "ws"+strings.TrimPrefix(srv.URL, "http")+"/ws")
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	c.Subscribe(ctx, marketdata.BookChannel("ETH-USDT"))
	c.Subscribe(ctx, marketdata.TradesChannel("ETH-USDT"))

	read := func() marketdata.Envelope {
		rctx, cancel := context.WithTimeout(ctx, 5*time.Second)
		defer cancel()
		e, _, err := c.Read(rctx)
		if err != nil {
			t.Fatal(err)
		}
		return e
	}
	replica := marketdata.NewBookReplica("ETH-USDT")
	for range 3 { // subscribed, snapshot, subscribed
		replica.Apply(read())
	}
	bids, asks := replica.Levels()
	if len(bids) != 1 || bids[0].Price != 100 || len(asks) != 2 || asks[0].Qty != 10 {
		t.Fatalf("snapshot after restart: bids %v asks %v", bids, asks)
	}

	if _, err := a.Trading.Place(trace.WithID(ctx, "md-trace"), trading.PlaceRequest{Market: "ETH-USDT", User: "taker", Side: trading.Buy, Type: trading.Limit, Price: 106, Qty: 12}); err != nil {
		t.Fatal(err)
	}
	var trades []marketdata.Envelope
	for len(trades) < 2 || replica.Seq() < 1 {
		e := read()
		if e.Type == marketdata.TypeTrade {
			trades = append(trades, e)
		}
		if err := replica.Apply(e); err != nil {
			t.Fatal(err)
		}
	}
	if trades[0].Price != 105 || trades[0].Qty != 10 || trades[1].Price != 106 || trades[1].Qty != 2 || trades[1].Seq != 2 {
		t.Fatalf("trades %+v", trades)
	}
	book, _ := a.Trading.Book(ctx, "ETH-USDT")
	gb, ga := replica.Levels()
	if !slices.Equal(gb, aggregate(book.Bids)) || !slices.Equal(ga, aggregate(book.Asks)) {
		t.Fatalf("replica %v %v, book %v %v", gb, ga, aggregate(book.Bids), aggregate(book.Asks))
	}
	// Delivery metrics are recorded after the server's write returns, which
	// can be just after the client has read the frame.
	metric := func(name string, labels ...string) float64 { v, _ := a.Registry.Get(name, labels...); return v }
	deadline := time.Now().Add(5 * time.Second)
	for metric("marketdata_messages_sent_total", marketdata.TypeTrade) != 2 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if got := metric("marketdata_messages_sent_total", marketdata.TypeTrade); got != 2 {
		t.Errorf("trade messages sent = %v", got)
	}
	if got := metric("marketdata_subscriptions", "book"); got != 1 {
		t.Errorf("book subscriptions = %v", got)
	}
	if got := metric("marketdata_connections"); got != 1 {
		t.Errorf("connections = %v", got)
	}
}

package ws_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gwakko/cex-lab/internal/events"
	"github.com/gwakko/cex-lab/internal/marketdata"
	"github.com/gwakko/cex-lab/internal/marketdata/adapters/ws"
)

const mkt = "BTC-USDT"

type counters struct {
	dropped atomic.Int64
	sent    atomic.Int64
	latency atomic.Int64
	conns   atomic.Int64
}

func (c *counters) Connections(n int)             { c.conns.Store(int64(n)) }
func (c *counters) Subscriptions(string, int)     {}
func (c *counters) MessageSent(string)            { c.sent.Add(1) }
func (c *counters) SlowConsumerDropped()          { c.dropped.Add(1) }
func (c *counters) PublishToSend(d time.Duration) { c.latency.Add(1) }

type fixture struct {
	hub *marketdata.Hub
	srv *httptest.Server
	url string
	m   *counters
}

func newFixture(t *testing.T, hcfg marketdata.Config, wcfg ws.Config) *fixture {
	t.Helper()
	hcfg.Markets = []string{mkt}
	m := &counters{}
	h := marketdata.New(hcfg, marketdata.WithMetrics(m))
	srv := httptest.NewServer(ws.NewHandler(h, wcfg))
	t.Cleanup(func() { h.Close(); srv.Close() })
	return &fixture{hub: h, srv: srv, url: "ws" + strings.TrimPrefix(srv.URL, "http"), m: m}
}

func (f *fixture) dial(t *testing.T) *ws.Client {
	t.Helper()
	c, err := ws.Dial(context.Background(), f.url)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Close() })
	return c
}

func read(t *testing.T, c *ws.Client) marketdata.Envelope {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	e, _, err := c.Read(ctx)
	if err != nil {
		t.Fatal(err)
	}
	return e
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func TestSubscribeSnapshotUpdatesAndTrades(t *testing.T) {
	f := newFixture(t, marketdata.Config{}, ws.Config{})
	f.hub.Seed(mkt, []events.Level{{Price: 99, Qty: 5}}, nil)
	c := f.dial(t)
	ctx := context.Background()
	c.Subscribe(ctx, marketdata.BookChannel(mkt))
	c.Subscribe(ctx, marketdata.TradesChannel(mkt))
	replica := marketdata.NewBookReplica(mkt)
	for _, want := range []string{marketdata.TypeSubscribed, marketdata.TypeBookSnapshot, marketdata.TypeSubscribed} {
		e := read(t, c)
		if e.Type != want {
			t.Fatalf("got %s, want %s", e.Type, want)
		}
		replica.Apply(e)
	}
	f.hub.PublishBookDelta(events.BookDeltaV1{Market: mkt, Asks: []events.Level{{Price: 101, Qty: 7}}})
	f.hub.PublishTrade(events.TradeV1{Market: mkt, Price: 101, Qty: 2, TakerSide: "buy", Time: time.Now()})
	upd, trade := read(t, c), read(t, c)
	if err := replica.Apply(upd); err != nil || upd.Seq != 1 {
		t.Fatalf("update %+v: %v", upd, err)
	}
	if trade.Type != marketdata.TypeTrade || trade.Seq != 1 || trade.Price != 101 {
		t.Fatalf("trade %+v", trade)
	}
	bids, asks := replica.Levels()
	if !slices.Equal(bids, []events.Level{{Price: 99, Qty: 5}}) || !slices.Equal(asks, []events.Level{{Price: 101, Qty: 7}}) {
		t.Fatalf("replica %v %v", bids, asks)
	}
	waitFor(t, "delivery metrics", func() bool { return f.m.sent.Load() == 5 && f.m.latency.Load() == 5 })
}

func TestInvalidRequestKeepsConnection(t *testing.T) {
	f := newFixture(t, marketdata.Config{}, ws.Config{})
	c := f.dial(t)
	c.SendRaw(context.Background(), []byte(`{"op":"subscribe","channel":"orders:alice"}`))
	if e := read(t, c); e.Type != marketdata.TypeError || !strings.Contains(e.Error, "invalid channel") {
		t.Fatalf("got %+v", e)
	}
	c.Subscribe(context.Background(), marketdata.TradesChannel(mkt))
	if e := read(t, c); e.Type != marketdata.TypeSubscribed {
		t.Fatalf("connection unusable after an invalid request: %+v", e)
	}
}

func TestSlowConsumerIsDisconnected(t *testing.T) {
	f := newFixture(t, marketdata.Config{QueueSize: 4}, ws.Config{WriteTimeout: 200 * time.Millisecond})
	c := f.dial(t)
	c.Subscribe(context.Background(), marketdata.TradesChannel(mkt))
	read(t, c) // ack; then stop reading
	waitFor(t, "slow consumer drop", func() bool {
		for range 500 {
			f.hub.PublishTrade(events.TradeV1{Market: mkt, Price: 1, Qty: 1, TakerSide: "sell", Time: time.Now()})
		}
		return f.m.dropped.Load() == 1
	})
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	for {
		if _, _, err := c.Read(ctx); err != nil {
			if code := ws.CloseStatus(err); code != ws.StatusPolicyViolation {
				t.Fatalf("closed with %d (%v), want policy violation", code, err)
			}
			break
		}
	}
	waitFor(t, "connection gauge to drop", func() bool { return f.m.conns.Load() == 0 })
}

func TestGracefulShutdownSendsGoingAway(t *testing.T) {
	f := newFixture(t, marketdata.Config{}, ws.Config{})
	c := f.dial(t)
	c.Subscribe(context.Background(), marketdata.TradesChannel(mkt))
	read(t, c)
	f.hub.Close()
	_, _, err := c.Read(context.Background())
	if code := ws.CloseStatus(err); code != ws.StatusGoingAway {
		t.Fatalf("closed with %d (%v), want going away", code, err)
	}
	if _, err := ws.Dial(context.Background(), f.url); err == nil {
		t.Fatal("dial after shutdown must fail")
	}
}

func TestConnectionAndFrameLimits(t *testing.T) {
	f := newFixture(t, marketdata.Config{MaxConnections: 1}, ws.Config{ReadLimit: 64})
	c := f.dial(t)
	_, err := ws.Dial(context.Background(), f.url)
	if err == nil || !strings.Contains(err.Error(), "503") {
		t.Fatalf("second connection: %v", err)
	}
	c.SendRaw(context.Background(), []byte(`{"op":"subscribe","channel":"`+strings.Repeat("x", 200)+`"}`))
	_, _, err = c.Read(context.Background())
	if code := ws.CloseStatus(err); code != ws.StatusMessageTooBig {
		t.Fatalf("oversized frame closed with %d (%v)", code, err)
	}
}

func TestPingKeepsIdleConnectionOpen(t *testing.T) {
	f := newFixture(t, marketdata.Config{}, ws.Config{PingInterval: 20 * time.Millisecond, WriteTimeout: time.Second})
	c := f.dial(t)
	c.Subscribe(context.Background(), marketdata.TradesChannel(mkt))
	read(t, c)
	var got marketdata.Envelope
	var wg sync.WaitGroup
	wg.Add(1)
	go func() { defer wg.Done(); got = read(t, c) }() // reading answers the server's pings
	time.Sleep(200 * time.Millisecond)
	f.hub.PublishTrade(events.TradeV1{Market: mkt, Price: 5, Qty: 1, TakerSide: "buy", Time: time.Now()})
	wg.Wait()
	if got.Type != marketdata.TypeTrade {
		t.Fatalf("idle connection did not survive pings: %+v", got)
	}
}

func TestRejectsCrossOrigin(t *testing.T) {
	f := newFixture(t, marketdata.Config{}, ws.Config{})
	req, _ := http.NewRequest("GET", f.srv.URL, nil)
	req.Header.Set("Connection", "Upgrade")
	req.Header.Set("Upgrade", "websocket")
	req.Header.Set("Sec-WebSocket-Version", "13")
	req.Header.Set("Sec-WebSocket-Key", "dGhlIHNhbXBsZSBub25jZQ==")
	req.Header.Set("Origin", "https://evil.example")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("cross-origin upgrade: %d", resp.StatusCode)
	}
}

// Command demo runs a tiny exchange end to end with observability: a chain
// deposit through the watcher, ledger deposits, crossing orders, settlement,
// a cancel, the outbox and the invariant checker, with /metrics, /healthz,
// /readyz and the /ws market data feed served over HTTP, a WebSocket client
// subscribed to the market, and JSON logs correlated by trace id.
package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"log/slog"
	"net"
	"net/http"
	"os"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/gwakko/cex-lab/internal/app"
	"github.com/gwakko/cex-lab/internal/custody"
	"github.com/gwakko/cex-lab/internal/custody/adapters/fakechain"
	"github.com/gwakko/cex-lab/internal/custody/adapters/memcursor"
	"github.com/gwakko/cex-lab/internal/events"
	"github.com/gwakko/cex-lab/internal/ledger"
	"github.com/gwakko/cex-lab/internal/marketdata"
	"github.com/gwakko/cex-lab/internal/marketdata/adapters/ws"
	"github.com/gwakko/cex-lab/internal/platform/outbox"
	"github.com/gwakko/cex-lab/internal/platform/trace"
	"github.com/gwakko/cex-lab/internal/trading"
)

const (
	btc  = "BTC"   // minimal unit: satoshi
	usdt = "USDT"  // minimal unit: cent (a simplification)
	lot  = 100_000 // 0.001 BTC
)

func main() {
	addr := flag.String("addr", "127.0.0.1:0", "listen address for /metrics, /healthz, /readyz")
	wait := flag.Duration("wait", 0, "keep serving this long after the scenario (e.g. 60s to curl /metrics)")
	flag.Parse()

	dir, err := os.MkdirTemp("", "cex-lab-demo-")
	must(err)
	defer os.RemoveAll(dir)

	var logs bytes.Buffer
	a, err := app.New(app.Config{
		DataDir: dir,
		Logger:  slog.New(slog.NewJSONHandler(&logs, nil)),
		Markets: []trading.MarketConfig{{
			Symbol: "BTC-USDT", Base: btc, Quote: usdt,
			Tick: 1, Lot: lot, MakerFeeBps: 10, TakerFeeBps: 20,
		}},
	})
	must(err)
	defer a.Close()
	l, x := a.Ledger, a.Trading

	base := serve(*addr, a.Handler())
	fmt.Printf("serving %s/metrics, /healthz, /readyz, /ws\n", base)
	feed := subscribe(base, "BTC-USDT")
	defer feed.client.Close()

	ctx := context.Background()
	fmt.Println("\n== deposits")
	chainDeposit(ctx, a)
	must2(l.Apply(ledger.Deposit{Key: "bank-1", User: "bob", Asset: usdt, Amount: 100_000_00}))
	must2(l.Apply(ledger.Deposit{Key: "bank-1", User: "bob", Asset: usdt, Amount: 100_000_00})) // client retry
	printBalances(l)

	fmt.Println("\n== orders (price = USDT cents per 0.001 BTC; 6000 = 60,000 USDT/BTC)")
	const followed = "demo-order-1"
	orders := []struct {
		trace string
		req   trading.PlaceRequest
	}{
		{"", trading.PlaceRequest{Market: "BTC-USDT", User: "alice", Side: trading.Sell, Type: trading.Limit, Price: 6_000, Qty: 20 * lot}},
		{"", trading.PlaceRequest{Market: "BTC-USDT", User: "alice", Side: trading.Sell, Type: trading.Limit, Price: 6_050, Qty: 30 * lot}},
		{followed, trading.PlaceRequest{Market: "BTC-USDT", User: "bob", Side: trading.Buy, Type: trading.Limit, Price: 6_100, Qty: 35 * lot}},
		{"", trading.PlaceRequest{Market: "BTC-USDT", User: "bob", Side: trading.Buy, Type: trading.Limit, Price: 5_900, Qty: 10 * lot}},
		{"", trading.PlaceRequest{Market: "BTC-USDT", User: "carol", Side: trading.Buy, Type: trading.Limit, Price: 5_900, Qty: 10 * lot}},
	}
	var lastID string
	for _, o := range orders {
		octx := ctx
		if o.trace != "" {
			octx = trace.WithID(ctx, o.trace)
		}
		r, err := x.Place(octx, o.req)
		if err != nil {
			fmt.Printf("rejected %s %s: %v\n", o.req.User, o.req.Side, err)
			continue
		}
		lastID = r.OrderID
		fmt.Printf("%s %s %s %d lots @ %d -> resting=%v remaining=%d lots\n",
			r.OrderID, o.req.User, o.req.Side, o.req.Qty/lot, o.req.Price, r.Resting, r.Remaining/lot)
		for _, t := range r.Trades {
			fmt.Printf("   trade: %d lots @ %d (maker %s, taker %s)\n", t.Qty/lot, t.Price, t.MakerOrderID, t.TakerOrderID)
		}
	}
	_, err = x.Cancel(ctx, "BTC-USDT", "bob", lastID)
	must(err)
	fmt.Printf("cancelled %s\n", lastID)

	fmt.Println("\n== final balances")
	printBalances(l)

	fmt.Println("\n== outbox")
	seen := 0
	inbox := a.NewInbox(func(events.Event) error { seen++; return nil })
	n, err := a.NewRelay(inbox.Handle, 0).Drain()
	must(err)
	// Redelivery after a "crash": quiet logger so the trace below stays readable.
	_, err = a.NewRelay(inbox.Handle, 0, outbox.WithLogger(slog.New(slog.DiscardHandler))).Drain()
	must(err)
	fmt.Printf("relayed %d events, redelivered all of them, consumer handled %d (deduplicated)\n", n, seen)

	fmt.Println("\n== invariants")
	must(x.Check(ctx))
	fmt.Println("ok: ledger balanced, no negatives, locked == resting reserves, book not crossed")

	received := feed.wait(10, 2*time.Second) // before scraping, so delivery metrics are complete

	fmt.Println("\n== HTTP")
	for _, p := range []string{"/healthz", "/readyz"} {
		code, body := fetch(base + p)
		fmt.Printf("GET %s -> %d %s\n", p, code, strings.TrimSpace(body))
	}
	_, page := fetch(base + "/metrics")
	fmt.Println("\n== /metrics excerpt")
	printExcerpt(page, []string{
		"exchange_orders_placed_total", "exchange_orders_rejected_total", "exchange_trades_total",
		"exchange_trade_volume_total", "ledger_commands_total", "ledger_idempotency_conflicts_total",
		"engine_book_depth", "engine_last_seq", "outbox_backlog", "inbox_duplicates_total",
		"invariant_ok", "invariant_check_runs_total", "chain_deposits_credited_total", "chain_head_block",
		"exchange_place_order_duration_seconds_count", "journal_fsync_duration_seconds_count",
		"marketdata_connections", "marketdata_subscriptions", "marketdata_messages_sent_total",
		"marketdata_publish_to_send_latency_seconds_count",
	})

	fmt.Println("\n== WebSocket /ws messages (book:BTC-USDT, trades:BTC-USDT)")
	for _, m := range received {
		fmt.Println("  " + m)
	}
	r := marketdata.NewBookReplica("BTC-USDT")
	for _, e := range feed.envelopes() {
		must(r.Apply(e))
	}
	bids, asks := r.Levels()
	fmt.Printf("client book replica at seq %d: bids=%v asks=%v\n", r.Seq(), bids, asks)

	fmt.Printf("\n== JSON logs for trace_id=%s (API -> engine -> ledger -> outbox)\n", followed)
	printTrace(&logs, followed)

	if *wait > 0 {
		fmt.Printf("\nserving for %s more; try: curl %s/metrics\n", *wait, base)
		time.Sleep(*wait)
	}
}

// feed collects market data messages received by a WebSocket client.
type feed struct {
	client *ws.Client
	mu     sync.Mutex
	raw    []string
	decode []marketdata.Envelope
}

func subscribe(base, market string) *feed {
	ctx := context.Background()
	c, err := ws.Dial(ctx, "ws"+strings.TrimPrefix(base, "http")+"/ws")
	must(err)
	must(c.Subscribe(ctx, marketdata.BookChannel(market)))
	must(c.Subscribe(ctx, marketdata.TradesChannel(market)))
	f := &feed{client: c}
	go func() {
		for {
			e, raw, err := c.Read(ctx)
			if err != nil {
				return
			}
			f.mu.Lock()
			f.raw, f.decode = append(f.raw, string(raw)), append(f.decode, e)
			f.mu.Unlock()
		}
	}()
	return f
}

// wait returns the raw messages once n have arrived or timeout passes.
func (f *feed) wait(n int, timeout time.Duration) []string {
	deadline := time.Now().Add(timeout)
	for {
		f.mu.Lock()
		got := len(f.raw)
		f.mu.Unlock()
		if got >= n || time.Now().After(deadline) {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.raw)
}

func (f *feed) envelopes() []marketdata.Envelope {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.decode)
}

func serve(addr string, h http.Handler) string {
	ln, err := net.Listen("tcp", addr)
	must(err)
	srv := &http.Server{Handler: h, ReadHeaderTimeout: 5 * time.Second}
	go func() {
		if err := srv.Serve(ln); err != http.ErrServerClosed {
			log.Print(err)
		}
	}()
	return "http://" + ln.Addr().String()
}

func fetch(url string) (int, string) {
	resp, err := http.Get(url)
	must(err)
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	must(err)
	return resp.StatusCode, string(b)
}

func printExcerpt(page string, prefixes []string) {
	sc := bufio.NewScanner(strings.NewReader(page))
	for sc.Scan() {
		line := sc.Text()
		if strings.HasPrefix(line, "#") {
			continue
		}
		for _, p := range prefixes {
			if strings.HasPrefix(line, p+"{") || strings.HasPrefix(line, p+" ") {
				fmt.Println("  " + line)
				break
			}
		}
	}
}

func printTrace(logs *bytes.Buffer, traceID string) {
	sc := bufio.NewScanner(bytes.NewReader(logs.Bytes()))
	for sc.Scan() {
		var m map[string]any
		if json.Unmarshal(sc.Bytes(), &m) == nil && m["trace_id"] == traceID {
			fmt.Println("  " + sc.Text())
		}
	}
}

// chainDeposit credits alice's BTC through the custody watcher on a fake chain.
func chainDeposit(ctx context.Context, a *app.App) {
	fc := fakechain.New()
	w, err := a.NewDepositWatcher(fc, &memcursor.Store{}, map[string]string{"addr-alice": "alice"},
		custody.Config{Confirmations: 3, Watched: []string{"addr-alice"}, StartBlock: 1})
	must(err)
	fc.Mine(custody.Transfer{TxHash: "0xfeed", To: "addr-alice", Asset: btc, Amount: 100 * lot})
	for i := range 3 {
		must(w.Poll(ctx))
		fmt.Printf("block %d: pending=%d credited=%d\n", i+1, len(w.Pending()), len(w.Credited()))
		fc.Mine()
	}
}

func printBalances(l *ledger.Ledger) {
	bs := l.Balances()
	accs := make([]ledger.Account, 0, len(bs))
	for a, b := range bs {
		if b != 0 {
			accs = append(accs, a)
		}
	}
	slices.SortFunc(accs, func(a, b ledger.Account) int { return strings.Compare(a.String(), b.String()) })
	for _, a := range accs {
		fmt.Printf("  %-32s %15d\n", a, bs[a])
	}
}

func must(err error) {
	if err != nil {
		log.Fatal(err)
	}
}

func must2(_ ledger.Tx, _ bool, err error) { must(err) }

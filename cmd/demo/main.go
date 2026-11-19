// Command demo runs a tiny exchange end to end with observability: a chain
// deposit through the watcher, ledger deposits, crossing orders, settlement,
// a cancel, the outbox and the invariant checker, with /metrics, /healthz and
// /readyz served over HTTP and JSON logs correlated by trace id.
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
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/gwakko/cex-lab/chain"
	"github.com/gwakko/cex-lab/engine"
	"github.com/gwakko/cex-lab/exchange"
	"github.com/gwakko/cex-lab/instrument"
	"github.com/gwakko/cex-lab/internal/events"
	"github.com/gwakko/cex-lab/ledger"
	"github.com/gwakko/cex-lab/outbox"
	"github.com/gwakko/cex-lab/telemetry"
	"github.com/gwakko/cex-lab/telemetry/trace"
)

const (
	btc  ledger.Asset = "BTC"   // minimal unit: satoshi
	usdt ledger.Asset = "USDT"  // minimal unit: cent (a simplification)
	lot               = 100_000 // 0.001 BTC
)

func main() {
	addr := flag.String("addr", "127.0.0.1:0", "listen address for /metrics, /healthz, /readyz")
	wait := flag.Duration("wait", 0, "keep serving this long after the scenario (e.g. 60s to curl /metrics)")
	flag.Parse()

	dir, err := os.MkdirTemp("", "cex-lab-demo-")
	must(err)
	defer os.RemoveAll(dir)

	var logs bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&logs, nil))
	reg := telemetry.NewRegistry()
	health := &telemetry.Health{}

	store, err := ledger.OpenFileStore(filepath.Join(dir, "ledger.journal"), instrument.NewJournal(reg, "ledger"))
	must(err)
	defer store.Close()
	cmdLog, err := engine.OpenFileLog(filepath.Join(dir, "btc-usdt.cmdlog"), instrument.NewJournal(reg, "engine_BTC-USDT"))
	must(err)
	defer cmdLog.Close()

	l, err := ledger.Open(store, ledger.WithMetrics(instrument.NewLedger(reg)), ledger.WithLogger(logger))
	must(err)
	x, err := exchange.New(l, []exchange.MarketConfig{{
		Symbol: "BTC-USDT", Base: btc, Quote: usdt,
		Tick: 1, Lot: lot, MakerFeeBps: 10, TakerFeeBps: 20,
		Log: cmdLog,
	}},
		exchange.WithMetrics(instrument.NewExchange(reg)),
		exchange.WithLogger(logger),
		exchange.WithEngineOptions(engine.WithMetrics(instrument.NewEngine(reg)), engine.WithLogger(logger)))
	must(err)
	defer x.Close()

	health.AddReadinessCheck("invariants", x.LastCheckError)
	health.AddReadinessCheck("ledger_journal", store.Err)
	health.AddReadinessCheck("engine_log_BTC-USDT", cmdLog.Err)
	base := serve(*addr, telemetry.NewMux(reg, health))
	fmt.Printf("serving %s/metrics, /healthz, /readyz\n", base)

	ctx := context.Background()
	fmt.Println("\n== deposits")
	chainDeposit(ctx, l, reg, logger)
	must2(l.Apply(ledger.Deposit{Key: "bank-1", User: "bob", Asset: usdt, Amount: 100_000_00}))
	must2(l.Apply(ledger.Deposit{Key: "bank-1", User: "bob", Asset: usdt, Amount: 100_000_00})) // client retry
	printBalances(l)

	fmt.Println("\n== orders (price = USDT cents per 0.001 BTC; 6000 = 60,000 USDT/BTC)")
	const followed = "demo-order-1"
	orders := []struct {
		trace string
		req   exchange.PlaceRequest
	}{
		{"", exchange.PlaceRequest{Market: "BTC-USDT", User: "alice", Side: engine.Sell, Type: engine.Limit, Price: 6_000, Qty: 20 * lot}},
		{"", exchange.PlaceRequest{Market: "BTC-USDT", User: "alice", Side: engine.Sell, Type: engine.Limit, Price: 6_050, Qty: 30 * lot}},
		{followed, exchange.PlaceRequest{Market: "BTC-USDT", User: "bob", Side: engine.Buy, Type: engine.Limit, Price: 6_100, Qty: 35 * lot}},
		{"", exchange.PlaceRequest{Market: "BTC-USDT", User: "bob", Side: engine.Buy, Type: engine.Limit, Price: 5_900, Qty: 10 * lot}},
		{"", exchange.PlaceRequest{Market: "BTC-USDT", User: "carol", Side: engine.Buy, Type: engine.Limit, Price: 5_900, Qty: 10 * lot}},
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
	om := instrument.NewOutbox(reg)
	seen := 0
	inbox := outbox.NewInbox(func(events.Event) error { seen++; return nil }, outbox.WithMetrics(om))
	n, err := outbox.NewRelay(l, inbox.Handle, 0, outbox.WithMetrics(om), outbox.WithLogger(logger)).Drain()
	must(err)
	_, err = outbox.NewRelay(l, inbox.Handle, 0, outbox.WithMetrics(om)).Drain() // redelivery after a "crash"
	must(err)
	fmt.Printf("relayed %d events, redelivered all of them, consumer handled %d (deduplicated)\n", n, seen)

	fmt.Println("\n== invariants")
	must(x.Check(ctx))
	fmt.Println("ok: ledger balanced, no negatives, locked == resting reserves, book not crossed")

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
	})

	fmt.Printf("\n== JSON logs for trace_id=%s (API -> engine -> ledger -> outbox)\n", followed)
	printTrace(&logs, followed)

	if *wait > 0 {
		fmt.Printf("\nserving for %s more; try: curl %s/metrics\n", *wait, base)
		time.Sleep(*wait)
	}
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

// chainDeposit credits alice's BTC through the watcher on a fake chain.
func chainDeposit(ctx context.Context, l *ledger.Ledger, reg *telemetry.Registry, logger *slog.Logger) {
	fc := chain.NewFakeChain()
	credit := func(ctx context.Context, d chain.Deposit) error {
		_, _, err := l.ApplyContext(ctx, ledger.Deposit{Key: "chain:" + d.Key(), User: "alice", Asset: ledger.Asset(d.Asset), Amount: d.Amount})
		return err
	}
	w, err := chain.NewWatcher(fc, &chain.MemStateStore{}, credit, chain.Config{
		Confirmations: 3, Watched: []string{"addr-alice"}, StartBlock: 1,
		Metrics: instrument.NewChain(reg), Logger: logger,
	})
	must(err)
	fc.Mine(chain.Transfer{TxHash: "0xfeed", To: "addr-alice", Asset: string(btc), Amount: 100 * lot})
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

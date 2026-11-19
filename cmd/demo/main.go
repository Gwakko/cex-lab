// Command demo runs a tiny in-memory exchange end to end: a chain deposit
// through the watcher, ledger deposits, crossing orders, settlement, a cancel,
// the outbox, and the invariant checker.
package main

import (
	"context"
	"fmt"
	"log"
	"slices"

	"github.com/gwakko/cex-lab/chain"
	"github.com/gwakko/cex-lab/engine"
	"github.com/gwakko/cex-lab/exchange"
	"github.com/gwakko/cex-lab/ledger"
	"github.com/gwakko/cex-lab/outbox"
)

const (
	btc  ledger.Asset = "BTC"   // minimal unit: satoshi
	usdt ledger.Asset = "USDT"  // minimal unit: cent (a simplification)
	lot               = 100_000 // 0.001 BTC
)

func main() {
	ctx := context.Background()
	l := ledger.New()
	x, err := exchange.New(l, exchange.MarketConfig{
		Symbol: "BTC-USDT", Base: btc, Quote: usdt,
		Tick: 1, Lot: lot, MakerFeeBps: 10, TakerFeeBps: 20,
		Log: &engine.MemLog{},
	})
	must(err)
	defer x.Close()

	fmt.Println("== deposits")
	chainDeposit(ctx, l)
	must2(l.Apply(ledger.Deposit{Key: "bank-1", User: "bob", Asset: usdt, Amount: 100_000_00}))
	printBalances(l)

	fmt.Println("\n== orders (price = USDT cents per 0.001 BTC; 6000 = 60,000 USDT/BTC)")
	orders := []exchange.PlaceRequest{
		{Market: "BTC-USDT", User: "alice", Side: engine.Sell, Type: engine.Limit, Price: 6_000, Qty: 20 * lot},
		{Market: "BTC-USDT", User: "alice", Side: engine.Sell, Type: engine.Limit, Price: 6_050, Qty: 30 * lot},
		{Market: "BTC-USDT", User: "bob", Side: engine.Buy, Type: engine.Limit, Price: 6_100, Qty: 35 * lot},
		{Market: "BTC-USDT", User: "bob", Side: engine.Buy, Type: engine.Limit, Price: 5_900, Qty: 10 * lot},
	}
	var lastID string
	for _, o := range orders {
		r, err := x.Place(ctx, o)
		must(err)
		lastID = r.OrderID
		fmt.Printf("%s %s %s %d lots @ %d -> resting=%v remaining=%d lots\n",
			r.OrderID, o.User, o.Side, o.Qty/lot, o.Price, r.Resting, r.Remaining/lot)
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
	inbox := outbox.NewInbox(func(ledger.Event) error { seen++; return nil })
	relay := outbox.NewRelay(l, inbox.Handle, 0)
	n, err := relay.Drain()
	must(err)
	_, err = outbox.NewRelay(l, inbox.Handle, 0).Drain() // redelivery after a "crash"
	must(err)
	fmt.Printf("relayed %d events, redelivered all of them, consumer handled %d (deduplicated)\n", n, seen)

	fmt.Println("\n== invariants")
	must(x.Check(ctx))
	fmt.Println("ok: ledger balanced, no negatives, locked == resting reserves, book not crossed")
}

// chainDeposit credits alice's BTC through the watcher on a fake chain.
func chainDeposit(ctx context.Context, l *ledger.Ledger) {
	fc := chain.NewFakeChain()
	credit := func(d chain.Deposit) error {
		_, _, err := l.Apply(ledger.Deposit{Key: "chain:" + d.Key(), User: "alice", Asset: ledger.Asset(d.Asset), Amount: d.Amount})
		return err
	}
	w, err := chain.NewWatcher(fc, &chain.MemStateStore{}, credit, chain.Config{Confirmations: 3, Watched: []string{"addr-alice"}, StartBlock: 1})
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
	slices.SortFunc(accs, func(a, b ledger.Account) int {
		if a.String() < b.String() {
			return -1
		}
		return 1
	})
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

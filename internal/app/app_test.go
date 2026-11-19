package app

import (
	"context"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"

	"github.com/gwakko/cex-lab/internal/custody"
	"github.com/gwakko/cex-lab/internal/custody/adapters/fakechain"
	"github.com/gwakko/cex-lab/internal/custody/adapters/memcursor"
	"github.com/gwakko/cex-lab/internal/events"
	"github.com/gwakko/cex-lab/internal/ledger"
	"github.com/gwakko/cex-lab/internal/trading"
	"github.com/gwakko/cex-lab/internal/trading/adapters/memlog"
)

func markets() []trading.MarketConfig {
	return []trading.MarketConfig{{Symbol: "ETH-USDT", Base: "ETH", Quote: "USDT", Tick: 1, Lot: 1, MakerFeeBps: 10, TakerFeeBps: 20}}
}

func TestAppWiresModulesAndSurvivesRestart(t *testing.T) {
	dir := t.TempDir()
	ctx := context.Background()
	a, err := New(Config{DataDir: dir, Markets: markets()})
	if err != nil {
		t.Fatal(err)
	}

	fc := fakechain.New()
	w, err := a.NewDepositWatcher(fc, &memcursor.Store{}, map[string]string{"addr-bob": "bob"},
		custody.Config{Confirmations: 1, Watched: []string{"addr-bob"}, StartBlock: 1})
	if err != nil {
		t.Fatal(err)
	}
	fc.Mine(custody.Transfer{TxHash: "0x1", To: "addr-bob", Asset: "ETH", Amount: 10})
	if err := w.Poll(ctx); err != nil {
		t.Fatal(err)
	}
	if _, _, err := a.Ledger.Apply(ledger.Deposit{Key: "bank-1", User: "alice", Asset: "USDT", Amount: 10_000}); err != nil {
		t.Fatal(err)
	}
	if _, err := a.Trading.Place(ctx, trading.PlaceRequest{Market: "ETH-USDT", User: "bob", Side: trading.Sell, Type: trading.Limit, Price: 100, Qty: 6}); err != nil {
		t.Fatal(err)
	}
	r, err := a.Trading.Place(ctx, trading.PlaceRequest{Market: "ETH-USDT", User: "alice", Side: trading.Buy, Type: trading.Limit, Price: 100, Qty: 4})
	if err != nil || len(r.Trades) != 1 {
		t.Fatalf("trade: %+v %v", r, err)
	}
	if err := a.Trading.Check(ctx); err != nil {
		t.Fatal(err)
	}

	var seen []events.Event
	inbox := a.NewInbox(func(e events.Event) error { seen = append(seen, e); return nil })
	if _, err := a.NewRelay(inbox.Handle, 0).Drain(); err != nil {
		t.Fatal(err)
	}
	if len(seen) != len(a.Ledger.EventsSince(0)) || seen[0].Source != ledger.EventSource {
		t.Fatalf("relayed %d events", len(seen))
	}

	srv := httptest.NewServer(a.Handler())
	resp, err := srv.Client().Get(srv.URL + "/readyz")
	if err != nil || resp.StatusCode != http.StatusOK {
		t.Fatalf("/readyz: %v %v", resp, err)
	}
	resp.Body.Close()
	srv.Close()
	for _, m := range []struct {
		name   string
		labels []string
	}{
		{"chain_deposits_credited_total", []string{"ETH"}},
		{"exchange_trades_total", []string{"ETH-USDT"}},
		{"journal_fsync_duration_seconds", []string{"ledger"}},
		{"journal_fsync_duration_seconds", []string{"engine_ETH-USDT"}},
		{"outbox_relay_publish_total", []string{"ok"}},
	} {
		if v, ok := a.Registry.Get(m.name, m.labels...); !ok || v == 0 {
			t.Errorf("%s%v not recorded", m.name, m.labels)
		}
	}
	balances := a.Ledger.Balances()
	book, _ := a.Trading.Book(ctx, "ETH-USDT")
	a.Close()

	b, err := New(Config{DataDir: dir, Markets: markets()})
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close()
	if !reflect.DeepEqual(b.Ledger.Balances(), balances) {
		t.Fatal("ledger not restored from the journal")
	}
	if got, _ := b.Trading.Book(ctx, "ETH-USDT"); !reflect.DeepEqual(got, book) {
		t.Fatal("book not restored from the command log")
	}
	if err := b.Trading.Check(ctx); err != nil {
		t.Fatal(err)
	}
}

func TestAppRejectsPresetCommandLog(t *testing.T) {
	m := markets()
	m[0].Log = &memlog.Log{}
	if _, err := New(Config{Markets: m}); err == nil {
		t.Fatal("app must own the choice of command log")
	}
}

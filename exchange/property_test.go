package exchange

import (
	"context"
	"errors"
	"fmt"
	"math/rand/v2"
	"reflect"
	"sync"
	"testing"
	"testing/quick"

	"github.com/gwakko/cex-lab/engine"
	"github.com/gwakko/cex-lab/ledger"
)

// source drives a simulation from either a PRNG or fuzzer bytes.
type source interface {
	intn(n int) int
	done() bool
}

type randSource struct{ r *rand.Rand }

func (s randSource) intn(n int) int { return s.r.IntN(n) }
func (randSource) done() bool       { return false }

type byteSource struct{ b []byte }

func (s *byteSource) intn(n int) int {
	if len(s.b) == 0 {
		return 0
	}
	v := int(s.b[0]) % n
	s.b = s.b[1:]
	return v
}
func (s *byteSource) done() bool { return len(s.b) == 0 }

var (
	simUsers  = []string{"alice", "bob", "carol"}
	simAssets = []ledger.Asset{btc, eth, usdt}
)

type openOrder struct{ market, user, id string }

// sim applies random deposit/place/cancel/withdraw/retry operations and
// checks every invariant after each step against an independent model.
type sim struct {
	t       testing.TB
	ctx     context.Context
	store   *ledger.MemStore
	cfgs    []MarketConfig
	x       *Exchange
	supply  map[ledger.Asset]int64 // model: deposits - confirmed withdrawals
	pending []string
	orders  []openOrder
	last    ledger.Command
	n       int
}

func newSim(t testing.TB) *sim {
	s := &sim{t: t, ctx: context.Background(), store: &ledger.MemStore{}, cfgs: testConfigs(), supply: map[ledger.Asset]int64{}}
	l, err := ledger.Open(s.store)
	if err != nil {
		t.Fatal(err)
	}
	if s.x, err = New(l, s.cfgs); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s.x.Close)
	return s
}

func (s *sim) key(prefix string) string {
	s.n++
	return fmt.Sprintf("%s-%d", prefix, s.n)
}

func (s *sim) run(src source, steps int) {
	for i := 0; i < steps && !src.done(); i++ {
		s.step(src)
		s.checkInvariants()
		if i%25 == 0 {
			s.checkReplay()
		}
	}
	s.checkReplay()
}

func (s *sim) step(src source) {
	switch src.intn(8) {
	case 0:
		s.deposit(src)
	case 1, 2, 3:
		s.place(src)
	case 4:
		s.cancel(src)
	case 5:
		s.withdraw(src)
	case 6:
		s.finishWithdrawal(src)
	case 7:
		s.retryLast()
	}
}

// apply runs a ledger command issued by the simulation itself.
func (s *sim) apply(cmd ledger.Command, allowed ...error) bool {
	_, _, err := s.x.Ledger().Apply(cmd)
	if err != nil {
		s.expect(err, allowed...)
		return false
	}
	s.last = cmd
	return true
}

func (s *sim) expect(err error, allowed ...error) {
	s.t.Helper()
	for _, a := range allowed {
		if errors.Is(err, a) {
			return
		}
	}
	s.t.Fatalf("unexpected error: %v", err)
}

func (s *sim) deposit(src source) {
	asset := simAssets[src.intn(len(simAssets))]
	amount := int64(1 + src.intn(map[ledger.Asset]int{btc: 500, eth: 50, usdt: 50_000}[asset]))
	if s.apply(ledger.Deposit{Key: s.key("dep"), User: simUsers[src.intn(len(simUsers))], Asset: asset, Amount: amount}) {
		s.supply[asset] += amount
	}
}

func (s *sim) place(src source) {
	cfg := s.cfgs[src.intn(len(s.cfgs))]
	req := PlaceRequest{
		Market: cfg.Symbol,
		User:   simUsers[src.intn(len(simUsers))],
		Side:   engine.Side(1 + src.intn(2)),
		Type:   engine.Limit,
		Qty:    cfg.Lot * int64(1+src.intn(10)),
	}
	if src.intn(5) == 0 {
		req.Type = engine.Market
		if req.Side == engine.Buy {
			req.QuoteBudget = int64(1 + src.intn(3_000))
		}
	} else {
		req.Price = cfg.Tick * (100/cfg.Tick - 3 + int64(src.intn(7)))
	}
	r, err := s.x.Place(s.ctx, req)
	if err != nil {
		s.expect(err, ledger.ErrInsufficientFunds)
		return
	}
	if r.Resting {
		s.orders = append(s.orders, openOrder{cfg.Symbol, req.User, r.OrderID})
	}
}

func (s *sim) cancel(src source) {
	if len(s.orders) == 0 {
		return
	}
	i := src.intn(len(s.orders))
	o := s.orders[i]
	user := o.user
	if src.intn(4) == 0 {
		user = simUsers[src.intn(len(simUsers))]
	}
	_, err := s.x.Cancel(s.ctx, o.market, user, o.id)
	if err != nil {
		s.expect(err, engine.ErrUnknownOrder, engine.ErrNotOwner)
		if !errors.Is(err, engine.ErrUnknownOrder) {
			return
		}
	}
	s.orders = append(s.orders[:i], s.orders[i+1:]...)
}

func (s *sim) withdraw(src source) {
	id := s.key("wd")
	cmd := ledger.WithdrawRequest{
		Key: id, ID: id,
		User:   simUsers[src.intn(len(simUsers))],
		Asset:  simAssets[src.intn(len(simAssets))],
		Amount: int64(1 + src.intn(2_000)),
	}
	if s.apply(cmd, ledger.ErrInsufficientFunds) {
		s.pending = append(s.pending, id)
	}
}

func (s *sim) finishWithdrawal(src source) {
	if len(s.pending) == 0 {
		return
	}
	i := src.intn(len(s.pending))
	id := s.pending[i]
	s.pending = append(s.pending[:i], s.pending[i+1:]...)
	if src.intn(2) == 0 {
		s.apply(ledger.WithdrawFail{Key: s.key("wf"), ID: id})
		return
	}
	w, _ := s.x.Ledger().Withdrawal(id)
	if s.apply(ledger.WithdrawConfirm{Key: s.key("wc"), ID: id}) {
		s.supply[w.Asset] -= w.Amount
	}
}

// retryLast re-sends the last successful command: it must be a no-op.
func (s *sim) retryLast() {
	if s.last == nil {
		return
	}
	l := s.x.Ledger()
	before, events := l.Balances(), len(l.EventsSince(0))
	_, replayed, err := l.Apply(s.last)
	if err != nil || !replayed {
		s.t.Fatalf("retry of %T: replayed=%v err=%v", s.last, replayed, err)
	}
	if !reflect.DeepEqual(before, l.Balances()) || len(l.EventsSince(0)) != events {
		s.t.Fatalf("idempotent retry of %T changed state", s.last)
	}
}

func (s *sim) checkInvariants() {
	s.t.Helper()
	if err := s.x.Check(s.ctx); err != nil {
		s.t.Fatal(err)
	}
	balances := s.x.Ledger().Balances()
	held := map[ledger.Asset]int64{}
	for acc, b := range balances {
		if acc.Kind != ledger.External {
			held[acc.Asset] += b
		}
	}
	for _, a := range simAssets {
		if got := s.x.Ledger().Supply(a); got != s.supply[a] {
			s.t.Fatalf("%s supply = %d, model says %d", a, got, s.supply[a])
		}
		if held[a] != s.supply[a] {
			s.t.Fatalf("%s held across accounts = %d, supply %d", a, held[a], s.supply[a])
		}
	}
}

// checkReplay rebuilds the ledger from its journal and each book from its
// command log and compares them to the live state.
func (s *sim) checkReplay() {
	s.t.Helper()
	l2, err := ledger.Open(s.store)
	if err != nil {
		s.t.Fatal(err)
	}
	if !reflect.DeepEqual(l2.Balances(), s.x.Ledger().Balances()) {
		s.t.Fatal("ledger replay differs from live balances")
	}
	for _, cfg := range s.cfgs {
		cmds, _ := cfg.Log.Load()
		book, _, err := engine.Replay(engine.MarketSpec{Symbol: cfg.Symbol, Tick: cfg.Tick, Lot: cfg.Lot}, cmds)
		if err != nil {
			s.t.Fatal(err)
		}
		live, err := s.x.markets[cfg.Symbol].eng.Snapshot(s.ctx)
		if err != nil {
			s.t.Fatal(err)
		}
		if !reflect.DeepEqual(book.Snapshot(), live) {
			s.t.Fatalf("%s replay differs from live book", cfg.Symbol)
		}
	}
}

func TestPropertyRandomSequences(t *testing.T) {
	runs := 100
	if testing.Short() {
		runs = 10
	}
	prop := func(seed uint64) bool {
		newSim(t).run(randSource{rand.New(rand.NewPCG(seed, seed^0x9e3779b9))}, 300)
		return !t.Failed()
	}
	if err := quick.Check(prop, &quick.Config{MaxCount: runs}); err != nil {
		t.Fatal(err)
	}
}

func FuzzExchange(f *testing.F) {
	f.Add([]byte{0, 0, 2, 200, 0, 1, 1, 100, 1, 0, 0, 5, 3, 1, 1, 1, 0, 5, 4, 0, 7})
	f.Add([]byte{0, 2, 1, 9, 0, 0, 2, 4, 1, 1, 1, 3, 1, 2, 2, 0, 0, 4, 2, 0, 9, 6, 0, 1})
	f.Fuzz(func(t *testing.T, data []byte) {
		newSim(t).run(&byteSource{b: data}, 1_000)
	})
}

func TestConcurrentTradingKeepsInvariants(t *testing.T) {
	s := newSim(t)
	for _, u := range simUsers {
		for _, a := range simAssets {
			s.apply(ledger.Deposit{Key: "seed-" + u + string(a), User: u, Asset: a, Amount: 1_000_000})
			s.supply[a] += 1_000_000
		}
	}
	var wg sync.WaitGroup
	for w := range 6 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			r := rand.New(rand.NewPCG(uint64(w), 42))
			user := simUsers[w%len(simUsers)]
			var mine []openOrder
			for i := range 300 {
				cfg := s.cfgs[r.IntN(len(s.cfgs))]
				if len(mine) > 0 && r.IntN(3) == 0 {
					o := mine[r.IntN(len(mine))]
					if _, err := s.x.Cancel(s.ctx, o.market, user, o.id); err != nil && !errors.Is(err, engine.ErrUnknownOrder) {
						t.Error(err)
					}
					continue
				}
				if i%50 == 0 {
					id := fmt.Sprintf("cw-%d-%d", w, i)
					s.x.Ledger().Apply(ledger.WithdrawRequest{Key: id, ID: id, User: user, Asset: usdt, Amount: 10})
				}
				res, err := s.x.Place(s.ctx, PlaceRequest{
					Market: cfg.Symbol, User: user, Side: engine.Side(1 + r.IntN(2)), Type: engine.Limit,
					Price: cfg.Tick * (100/cfg.Tick - 3 + int64(r.IntN(7))), Qty: cfg.Lot * int64(1+r.IntN(10)),
				})
				if err != nil {
					t.Error(err)
					return
				}
				if res.Resting {
					mine = append(mine, openOrder{cfg.Symbol, user, res.OrderID})
				}
			}
		}()
	}
	wg.Wait()
	s.checkInvariants()
	s.checkReplay()
}

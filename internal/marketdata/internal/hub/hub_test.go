package hub

import (
	"encoding/json"
	"errors"
	"fmt"
	"math/rand/v2"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gwakko/cex-lab/internal/events"
)

type recorder struct {
	mu          sync.Mutex
	connections int
	subs        map[string]int
	sent        map[string]int
	dropped     int
	latencies   int
}

func newRecorder() *recorder { return &recorder{subs: map[string]int{}, sent: map[string]int{}} }

func (r *recorder) Connections(n int) { r.mu.Lock(); r.connections = n; r.mu.Unlock() }
func (r *recorder) Subscriptions(kind string, n int) {
	r.mu.Lock()
	r.subs[kind] = n
	r.mu.Unlock()
}
func (r *recorder) MessageSent(t string)          { r.mu.Lock(); r.sent[t]++; r.mu.Unlock() }
func (r *recorder) SlowConsumerDropped()          { r.mu.Lock(); r.dropped++; r.mu.Unlock() }
func (r *recorder) PublishToSend(d time.Duration) { r.mu.Lock(); r.latencies++; r.mu.Unlock() }

const mkt = "BTC-USDT"

func newHub(t *testing.T, cfg Config) (*Hub, *recorder) {
	t.Helper()
	if cfg.Markets == nil {
		cfg.Markets = []string{mkt, "ETH-USDT"}
	}
	rec := newRecorder()
	return New(cfg, WithMetrics(rec)), rec
}

func connect(t *testing.T, h *Hub) *Session {
	t.Helper()
	s, err := h.Connect()
	if err != nil {
		t.Fatal(err)
	}
	return s
}

// drain returns every queued message without blocking, marking each delivered.
func drain(t *testing.T, s *Session) []Envelope {
	t.Helper()
	var out []Envelope
	for {
		select {
		case m := <-s.Out():
			var e Envelope
			if err := json.Unmarshal(m.Data, &e); err != nil {
				t.Fatal(err)
			}
			s.Delivered(m)
			out = append(out, e)
		default:
			return out
		}
	}
}

func req(op, ch string) []byte { return fmt.Appendf(nil, `{"op":%q,"channel":%q}`, op, ch) }

func TestRequestValidation(t *testing.T) {
	h, _ := newHub(t, Config{MaxSubscriptions: 2})
	s := connect(t, h)
	cases := map[string]string{
		`not json`: "malformed request",
		`{"op":"subscribe","channel":"x","user":1}`:      "malformed request",
		`{"op":"subscribe","channel":"a"}{}`:             "trailing data",
		`{"op":"dance","channel":"book:BTC-USDT"}`:       "unknown op",
		`{"op":"subscribe","channel":"orders:X"}`:        "invalid channel",
		`{"op":"subscribe","channel":"book:"}`:           "invalid channel",
		`{"op":"subscribe","channel":"book:DOGE"}`:       "unknown market",
		`{"op":"unsubscribe","channel":"book:ETH-USDT"}`: "not subscribed",
	}
	for raw, want := range cases {
		s.Handle([]byte(raw))
		got := drain(t, s)
		if len(got) != 1 || got[0].Type != TypeError || !strings.Contains(got[0].Error, want) {
			t.Errorf("%s: got %+v, want error containing %q", raw, got, want)
		}
	}
	s.Handle(req("subscribe", BookChannel(mkt)))
	s.Handle(req("subscribe", TradesChannel(mkt)))
	s.Handle(req("subscribe", TradesChannel("ETH-USDT")))
	got := drain(t, s)
	if len(got) != 4 || got[0].Type != TypeSubscribed || got[1].Type != TypeBookSnapshot || got[2].Type != TypeSubscribed {
		t.Fatalf("subscriptions: %+v", got)
	}
	if got[3].Type != TypeError || !strings.Contains(got[3].Error, "too many subscriptions") {
		t.Fatalf("limit not enforced: %+v", got[3])
	}
	select {
	case <-s.Done():
		t.Fatal("invalid requests must not end the session")
	default:
	}
}

// model is a full reference book driven by random deltas.
type model struct{ bids, asks map[int64]int64 }

func (m *model) randomDelta(r *rand.Rand) events.BookDeltaV1 {
	d := events.BookDeltaV1{Market: mkt}
	for range 1 + r.IntN(3) {
		bid := r.IntN(2) == 0
		side, list := m.asks, &d.Asks
		price := int64(101 + r.IntN(20))
		if bid {
			side, list, price = m.bids, &d.Bids, int64(80+r.IntN(20))
		}
		qty := int64(0)
		if r.IntN(3) != 0 {
			qty = int64(1 + r.IntN(50))
		}
		applyLevels(side, []events.Level{{Price: price, Qty: qty}})
		*list = append(*list, events.Level{Price: price, Qty: qty})
	}
	return d
}

func TestSnapshotThenIncrementalReconstructsTopN(t *testing.T) {
	const depth = 5
	for seed := range uint64(20) {
		r := rand.New(rand.NewPCG(seed, 11))
		h, _ := newHub(t, Config{Depth: depth, QueueSize: 10_000})
		ref := &model{bids: map[int64]int64{}, asks: map[int64]int64{}}
		type client struct {
			s       *Session
			replica *BookReplica
			lastSeq uint64
		}
		var clients []*client
		for step := range 500 {
			if step%60 == 0 { // late joiners at different points of the stream
				c := &client{s: connect(t, h), replica: NewBookReplica(mkt)}
				c.s.Handle(req("subscribe", BookChannel(mkt)))
				clients = append(clients, c)
			}
			h.PublishBookDelta(ref.randomDelta(r))
			wantB, wantA := topN(ref.bids, true, depth), topN(ref.asks, false, depth)
			hb, ha, seq, _ := h.TopN(mkt)
			if !slices.Equal(hb, wantB) || !slices.Equal(ha, wantA) {
				t.Fatalf("seed %d step %d: hub view differs from reference", seed, step)
			}
			for i, c := range clients {
				for _, e := range drain(t, c.s) {
					if e.Type == TypeBookUpdate && e.Seq != c.lastSeq+1 {
						t.Fatalf("seed %d client %d: seq %d after %d", seed, i, e.Seq, c.lastSeq)
					}
					if e.Type == TypeBookUpdate || e.Type == TypeBookSnapshot {
						c.lastSeq = e.Seq
					}
					if err := c.replica.Apply(e); err != nil {
						t.Fatalf("seed %d client %d: %v", seed, i, err)
					}
				}
				gb, ga := c.replica.Levels()
				if c.replica.Seq() != seq || !slices.Equal(gb, wantB) || !slices.Equal(ga, wantA) {
					t.Fatalf("seed %d step %d client %d: replica differs (seq %d vs %d)", seed, step, i, c.replica.Seq(), seq)
				}
			}
		}
	}
}

func TestTradesAreSequencedAndPublic(t *testing.T) {
	h, _ := newHub(t, Config{})
	s := connect(t, h)
	s.Handle(req("subscribe", TradesChannel(mkt)))
	for i := range 3 {
		h.PublishTrade(events.TradeV1{Market: mkt, Seq: 7, Index: i, Price: 100, Qty: int64(i + 1), TakerSide: "buy", TraceID: "secret-trace", Time: time.Unix(0, 0)})
	}
	h.PublishTrade(events.TradeV1{Market: "ETH-USDT", Price: 1, Qty: 1})
	got := drain(t, s)
	if len(got) != 4 || got[0].Type != TypeSubscribed || got[0].Seq != 0 {
		t.Fatalf("messages %+v", got)
	}
	for i, e := range got[1:] {
		if e.Type != TypeTrade || e.Seq != uint64(i+1) || e.Qty != int64(i+1) || e.TakerSide != "buy" || e.Market != mkt {
			t.Fatalf("trade %d: %+v", i, e)
		}
	}
	raw, _ := json.Marshal(got)
	if strings.Contains(string(raw), "secret-trace") || strings.Contains(string(raw), "user") {
		t.Fatalf("public channel leaks internals: %s", raw)
	}
}

func TestSlowConsumerIsDropped(t *testing.T) {
	h, rec := newHub(t, Config{QueueSize: 4})
	slow, healthy := connect(t, h), connect(t, h)
	slow.Handle(req("subscribe", TradesChannel(mkt)))
	healthy.Handle(req("subscribe", TradesChannel(mkt)))
	for range 10 {
		h.PublishTrade(events.TradeV1{Market: mkt, Price: 1, Qty: 1})
		drain(t, healthy)
	}
	select {
	case <-slow.Done():
	default:
		t.Fatal("slow consumer not dropped")
	}
	if slow.Reason() != ReasonSlowConsumer || rec.dropped != 1 {
		t.Fatalf("reason %q, dropped %d", slow.Reason(), rec.dropped)
	}
	if healthy.Reason() != "" {
		t.Fatal("healthy session affected")
	}
	if rec.connections != 1 || rec.subs[KindTrades] != 2 {
		t.Fatalf("connections %d, subs %d", rec.connections, rec.subs[KindTrades])
	}
	slow.Close() // the adapter always closes; this removes the subscription
	if rec.subs[KindTrades] != 1 {
		t.Fatalf("subscription not removed after close: %d", rec.subs[KindTrades])
	}
}

func TestGapDetectionAndResync(t *testing.T) {
	h, _ := newHub(t, Config{QueueSize: 1000})
	s := connect(t, h)
	s.Handle(req("subscribe", BookChannel(mkt)))
	ref := &model{bids: map[int64]int64{}, asks: map[int64]int64{}}
	r := rand.New(rand.NewPCG(1, 2))
	for range 5 {
		h.PublishBookDelta(ref.randomDelta(r))
	}
	msgs := drain(t, s)
	replica := NewBookReplica(mkt)
	var dropped bool
	for _, e := range msgs {
		if e.Type == TypeBookUpdate && e.Seq == 3 && !dropped {
			dropped = true // lose one message
			continue
		}
		err := replica.Apply(e)
		if e.Type == TypeBookUpdate && e.Seq >= 4 && !errors.Is(err, ErrGap) {
			t.Fatalf("seq %d after a gap: %v", e.Seq, err)
		}
	}
	if replica.Synced() {
		t.Fatal("replica still synced after a gap")
	}
	s.Handle(req("subscribe", BookChannel(mkt))) // resync
	h.PublishBookDelta(ref.randomDelta(r))
	for _, e := range drain(t, s) {
		if err := replica.Apply(e); err != nil {
			t.Fatalf("after resync: %v", err)
		}
	}
	gb, ga := replica.Levels()
	hb, ha, seq, _ := h.TopN(mkt)
	if !replica.Synced() || replica.Seq() != seq || !slices.Equal(gb, hb) || !slices.Equal(ga, ha) {
		t.Fatal("replica not equal to hub after resync")
	}
}

func TestUnsubscribeStopsMessages(t *testing.T) {
	h, rec := newHub(t, Config{})
	s := connect(t, h)
	s.Handle(req("subscribe", TradesChannel(mkt)))
	s.Handle(req("unsubscribe", TradesChannel(mkt)))
	h.PublishTrade(events.TradeV1{Market: mkt, Price: 1, Qty: 1})
	got := drain(t, s)
	if len(got) != 2 || got[1].Type != TypeUnsubscribed || rec.subs[KindTrades] != 0 {
		t.Fatalf("messages %+v, subs %d", got, rec.subs[KindTrades])
	}
}

func TestConnectionLimitShutdownAndMetrics(t *testing.T) {
	h, rec := newHub(t, Config{MaxConnections: 1})
	a := connect(t, h)
	if _, err := h.Connect(); !errors.Is(err, ErrTooManyConnections) {
		t.Fatalf("second connection: %v", err)
	}
	a.Handle(req("subscribe", BookChannel(mkt)))
	drain(t, a)
	if rec.sent["control"] != 1 || rec.sent[TypeBookSnapshot] != 1 || rec.latencies != 2 {
		t.Fatalf("sent %v latencies %d", rec.sent, rec.latencies)
	}
	a.Close()
	b := connect(t, h)
	h.Close()
	select {
	case <-b.Done():
	default:
		t.Fatal("shutdown did not end the session")
	}
	if b.Reason() != ReasonShutdown || rec.connections != 0 {
		t.Fatalf("reason %q connections %d", b.Reason(), rec.connections)
	}
	if _, err := h.Connect(); !errors.Is(err, ErrClosed) {
		t.Fatalf("connect after close: %v", err)
	}
}

func TestSeedSetsInitialView(t *testing.T) {
	h, _ := newHub(t, Config{Depth: 2})
	h.Seed(mkt, []events.Level{{Price: 99, Qty: 1}, {Price: 98, Qty: 2}, {Price: 97, Qty: 3}}, []events.Level{{Price: 101, Qty: 4}})
	s := connect(t, h)
	s.Handle(req("subscribe", BookChannel(mkt)))
	snap := drain(t, s)[1]
	if !slices.Equal(snap.Bids, [][2]int64{{99, 1}, {98, 2}}) || !slices.Equal(snap.Asks, [][2]int64{{101, 4}}) {
		t.Fatalf("snapshot %+v", snap)
	}
	// Removing the best bid brings the third level into the top-2 view.
	h.PublishBookDelta(events.BookDeltaV1{Market: mkt, Bids: []events.Level{{Price: 99}}})
	upd := drain(t, s)[0]
	if !slices.Equal(upd.Bids, [][2]int64{{99, 0}, {97, 3}}) || upd.Seq != 1 {
		t.Fatalf("update %+v", upd)
	}
}

func TestConcurrentPublishSubscribeClose(t *testing.T) {
	h, rec := newHub(t, Config{QueueSize: 8})
	var wg sync.WaitGroup
	for _, m := range []string{mkt, "ETH-USDT"} {
		wg.Add(1)
		go func() {
			defer wg.Done()
			r := rand.New(rand.NewPCG(uint64(len(m)), 5))
			ref := &model{bids: map[int64]int64{}, asks: map[int64]int64{}}
			for range 2000 {
				d := ref.randomDelta(r)
				d.Market = m
				h.PublishBookDelta(d)
				h.PublishTrade(events.TradeV1{Market: m, Price: 1, Qty: 1})
			}
		}()
	}
	for w := range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range 50 {
				s, err := h.Connect()
				if err != nil {
					t.Error(err)
					return
				}
				s.Handle(req("subscribe", BookChannel(mkt)))
				s.Handle(req("subscribe", TradesChannel("ETH-USDT")))
				if (w+i)%2 == 0 {
					for range 3 {
						select {
						case m := <-s.Out():
							s.Delivered(m)
						default:
						}
					}
				}
				s.Close()
			}
		}()
	}
	wg.Wait()
	rec.mu.Lock()
	defer rec.mu.Unlock()
	if rec.connections != 0 || rec.subs[KindBook] != 0 || rec.subs[KindTrades] != 0 {
		t.Fatalf("leaked: connections %d subs %v", rec.connections, rec.subs)
	}
}

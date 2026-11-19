// Package hub implements the marketdata module: it keeps a replica of every
// market's book built from BookDeltaV1 events, publishes a top-N view with a
// per-market sequence number, fans trades and book updates out to sessions
// through bounded queues, and drops sessions that cannot keep up.
package hub

import (
	"cmp"
	"encoding/json"
	"errors"
	"log/slog"
	"maps"
	"slices"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gwakko/cex-lab/internal/events"
)

var (
	ErrClosed             = errors.New("marketdata: hub closed")
	ErrTooManyConnections = errors.New("marketdata: too many connections")
)

// Config limits the hub. Zero values get defaults.
type Config struct {
	Markets          []string
	Depth            int // levels per side in snapshots and updates; default 10
	QueueSize        int // messages buffered per session before it is dropped; default 256
	MaxSubscriptions int // per session; default 16
	MaxConnections   int // default 1000
}

// Metrics receives hub measurements. Labels are bounded: channel kind and
// message type.
type Metrics interface {
	Connections(n int)
	Subscriptions(kind string, n int)
	MessageSent(msgType string)
	SlowConsumerDropped()
	PublishToSend(d time.Duration)
}

type nopMetrics struct{}

func (nopMetrics) Connections(int)             {}
func (nopMetrics) Subscriptions(string, int)   {}
func (nopMetrics) MessageSent(string)          {}
func (nopMetrics) SlowConsumerDropped()        {}
func (nopMetrics) PublishToSend(time.Duration) {}

// Option configures a Hub.
type Option func(*Hub)

// WithMetrics reports hub measurements to m.
func WithMetrics(m Metrics) Option { return func(h *Hub) { h.metrics = m } }

// WithLogger logs published trades, drops and shutdown.
func WithLogger(l *slog.Logger) Option { return func(h *Hub) { h.log = l } }

// Hub is safe for concurrent use. Publish calls for one market must come in
// sequence order (trading calls them from that market's engine goroutine).
type Hub struct {
	cfg     Config
	metrics Metrics
	log     *slog.Logger
	markets map[string]*market // fixed after New

	mu       sync.Mutex
	sessions map[*Session]struct{}
	closed   bool
	nextID   uint64

	subs map[string]*atomic.Int64 // active subscriptions per channel kind
}

type market struct {
	mu                 sync.Mutex
	name               string
	bids, asks         map[int64]int64 // full replica: price -> aggregate qty
	viewBids, viewAsks []events.Level  // last published top-N
	bookSeq, tradeSeq  uint64
	bookSubs           map[*Session]struct{}
	tradeSubs          map[*Session]struct{}
}

// New returns a hub for cfg.Markets with empty books.
func New(cfg Config, opts ...Option) *Hub {
	cfg.Depth = cmp.Or(cfg.Depth, 10)
	cfg.QueueSize = cmp.Or(cfg.QueueSize, 256)
	cfg.MaxSubscriptions = cmp.Or(cfg.MaxSubscriptions, 16)
	cfg.MaxConnections = cmp.Or(cfg.MaxConnections, 1000)
	h := &Hub{
		cfg:      cfg,
		metrics:  nopMetrics{},
		log:      slog.New(slog.DiscardHandler),
		markets:  map[string]*market{},
		sessions: map[*Session]struct{}{},
		subs:     map[string]*atomic.Int64{KindTrades: {}, KindBook: {}},
	}
	for _, o := range opts {
		o(h)
	}
	for _, name := range cfg.Markets {
		h.markets[name] = &market{
			name: name, bids: map[int64]int64{}, asks: map[int64]int64{},
			bookSubs: map[*Session]struct{}{}, tradeSubs: map[*Session]struct{}{},
		}
	}
	return h
}

// Seed sets a market's book before serving (for example after trading has
// replayed its command log). It does not notify subscribers.
func (h *Hub) Seed(name string, bids, asks []events.Level) {
	m, ok := h.markets[name]
	if !ok {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	clear(m.bids)
	clear(m.asks)
	applyLevels(m.bids, bids)
	applyLevels(m.asks, asks)
	m.viewBids, m.viewAsks = topN(m.bids, true, h.cfg.Depth), topN(m.asks, false, h.cfg.Depth)
}

// PublishBookDelta applies d to the replica and, if the top-N view changed,
// sends one book_update with the next sequence number.
func (h *Hub) PublishBookDelta(d events.BookDeltaV1) {
	m, ok := h.markets[d.Market]
	if !ok {
		h.log.Warn("marketdata: delta for unknown market", "market", d.Market)
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	applyLevels(m.bids, d.Bids)
	applyLevels(m.asks, d.Asks)
	newBids, newAsks := topN(m.bids, true, h.cfg.Depth), topN(m.asks, false, h.cfg.Depth)
	db, da := diff(m.viewBids, newBids, true), diff(m.viewAsks, newAsks, false)
	m.viewBids, m.viewAsks = newBids, newAsks
	if len(db) == 0 && len(da) == 0 {
		return
	}
	m.bookSeq++
	h.broadcast(m.bookSubs, TypeBookUpdate, Envelope{
		Type: TypeBookUpdate, Channel: BookChannel(m.name), Market: m.name, Seq: m.bookSeq, Bids: pairs(db), Asks: pairs(da),
	})
}

// PublishTrade sends a trade with the next per-market trade sequence number.
// The trace id is logged, not published.
func (h *Hub) PublishTrade(t events.TradeV1) {
	m, ok := h.markets[t.Market]
	if !ok {
		h.log.Warn("marketdata: trade for unknown market", "market", t.Market)
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.tradeSeq++
	h.log.Info("marketdata trade published", "market", t.Market, "seq", m.tradeSeq, "engine_seq", t.Seq, "trace_id", t.TraceID)
	h.broadcast(m.tradeSubs, TypeTrade, Envelope{
		Type: TypeTrade, Channel: TradesChannel(m.name), Market: m.name, Seq: m.tradeSeq,
		Price: t.Price, Qty: t.Qty, TakerSide: t.TakerSide, Time: t.Time.UTC().Format(time.RFC3339Nano),
	})
}

// broadcast must be called with the market lock held, which orders messages
// per session consistently with snapshots taken under the same lock.
func (h *Hub) broadcast(subs map[*Session]struct{}, msgType string, e Envelope) {
	if len(subs) == 0 {
		return
	}
	data, _ := json.Marshal(e) // Envelope always marshals
	now := time.Now()
	for s := range subs {
		s.enqueue(Message{Data: data, Type: msgType, Enqueued: now})
	}
}

// TopN returns a market's current published view and book sequence number.
func (h *Hub) TopN(name string) (bids, asks []events.Level, seq uint64, ok bool) {
	m, ok := h.markets[name]
	if !ok {
		return nil, nil, 0, false
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	return slices.Clone(m.viewBids), slices.Clone(m.viewAsks), m.bookSeq, true
}

// Connect opens a session, enforcing the connection limit.
func (h *Hub) Connect() (*Session, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.closed {
		return nil, ErrClosed
	}
	if len(h.sessions) >= h.cfg.MaxConnections {
		return nil, ErrTooManyConnections
	}
	h.nextID++
	s := newSession(h, h.nextID)
	h.sessions[s] = struct{}{}
	h.metrics.Connections(len(h.sessions))
	return s, nil
}

func (h *Hub) sessionEnded(s *Session) {
	h.mu.Lock()
	defer h.mu.Unlock()
	delete(h.sessions, s)
	h.metrics.Connections(len(h.sessions))
}

func (h *Hub) subscriptionChanged(kind string, delta int64) {
	h.metrics.Subscriptions(kind, int(h.subs[kind].Add(delta)))
}

// Close ends every session with reason "shutdown" and refuses new ones.
func (h *Hub) Close() {
	h.mu.Lock()
	h.closed = true
	sessions := slices.Collect(maps.Keys(h.sessions))
	h.mu.Unlock()
	for _, s := range sessions {
		s.terminate(ReasonShutdown)
	}
	h.log.Info("marketdata hub closed", "sessions", len(sessions))
}

func applyLevels(side map[int64]int64, levels []events.Level) {
	for _, l := range levels {
		if l.Qty == 0 {
			delete(side, l.Price)
		} else {
			side[l.Price] = l.Qty
		}
	}
}

func topN(side map[int64]int64, bids bool, n int) []events.Level {
	prices := slices.Collect(maps.Keys(side))
	slices.Sort(prices)
	if bids {
		slices.Reverse(prices)
	}
	out := make([]events.Level, 0, min(n, len(prices)))
	for _, p := range prices[:min(n, len(prices))] {
		out = append(out, events.Level{Price: p, Qty: side[p]})
	}
	return out
}

// diff lists what changes old into next: changed or new levels with their
// qty, and levels that left the view with qty 0, in book order.
func diff(old, next []events.Level, bids bool) []events.Level {
	was := map[int64]int64{}
	for _, l := range old {
		was[l.Price] = l.Qty
	}
	var out []events.Level
	now := map[int64]bool{}
	for _, l := range next {
		now[l.Price] = true
		if was[l.Price] != l.Qty {
			out = append(out, l)
		}
	}
	for _, l := range old {
		if !now[l.Price] {
			out = append(out, events.Level{Price: l.Price})
		}
	}
	slices.SortFunc(out, func(a, b events.Level) int {
		if bids {
			return cmp.Compare(b.Price, a.Price)
		}
		return cmp.Compare(a.Price, b.Price)
	})
	return out
}

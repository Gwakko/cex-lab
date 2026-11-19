package hub

import (
	"encoding/json"
	"fmt"
	"sync"
	"sync/atomic"
	"time"
)

// Reasons a session ended.
const (
	ReasonSlowConsumer = "slow_consumer"
	ReasonShutdown     = "shutdown"
	ReasonClosed       = "closed"
)

// Message is one encoded server message waiting in a session queue.
type Message struct {
	Data     []byte
	Type     string
	Enqueued time.Time
}

// Session is one client. The transport adapter reads Out, calls Delivered
// after each successful write, feeds client frames to Handle, and calls
// Close when the connection ends. Done is closed when the hub ends the
// session (slow consumer or shutdown); Reason says why.
type Session struct {
	hub  *Hub
	id   uint64
	out  chan Message
	done chan struct{}

	closed    atomic.Bool
	endOnce   sync.Once
	cleanOnce sync.Once

	mu     sync.Mutex
	subs   map[string]string // channel -> kind
	reason string
}

func newSession(h *Hub, id uint64) *Session {
	return &Session{hub: h, id: id, out: make(chan Message, h.cfg.QueueSize), done: make(chan struct{}), subs: map[string]string{}}
}

// Out delivers queued messages in order.
func (s *Session) Out() <-chan Message { return s.out }

// Done is closed when the session has ended.
func (s *Session) Done() <-chan struct{} { return s.done }

// Reason is why the session ended, or "" while it is open.
func (s *Session) Reason() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.reason
}

// Delivered records a message as written to the client.
func (s *Session) Delivered(m Message) {
	s.hub.metrics.MessageSent(m.Type)
	s.hub.metrics.PublishToSend(time.Since(m.Enqueued))
}

// enqueue never blocks: a full queue ends the session. Holding a market lock
// here is fine because it takes no other lock except via terminate.
func (s *Session) enqueue(m Message) {
	if s.closed.Load() {
		return
	}
	select {
	case s.out <- m:
	default:
		s.hub.metrics.SlowConsumerDropped()
		s.hub.log.Warn("marketdata: dropping slow consumer", "session", s.id, "queue", cap(s.out))
		s.terminate(ReasonSlowConsumer)
	}
}

// terminate ends the session once. Subscriptions are removed by Close, which
// the adapter always calls, so terminate never takes market locks.
func (s *Session) terminate(reason string) {
	s.endOnce.Do(func() {
		s.closed.Store(true)
		s.mu.Lock()
		s.reason = reason
		s.mu.Unlock()
		close(s.done)
		s.hub.sessionEnded(s)
	})
}

// Close ends the session (if still open) and removes its subscriptions.
func (s *Session) Close() {
	s.terminate(ReasonClosed)
	s.cleanOnce.Do(func() {
		s.mu.Lock()
		subs := s.subs
		s.subs = map[string]string{}
		s.mu.Unlock()
		for ch, kind := range subs {
			s.removeFromMarket(ch, kind)
		}
	})
}

// Handle processes one client frame. Invalid requests get an error message
// and leave the session open.
func (s *Session) Handle(raw []byte) {
	req, err := parseRequest(raw)
	if err != nil {
		s.sendControl(Envelope{Type: TypeError, Error: err.Error()})
		return
	}
	switch req.Op {
	case "subscribe":
		err = s.subscribe(req.Channel)
	case "unsubscribe":
		err = s.unsubscribe(req.Channel)
	default:
		err = fmt.Errorf("unknown op %q: want subscribe or unsubscribe", req.Op)
	}
	if err != nil {
		s.sendControl(Envelope{Type: TypeError, Channel: req.Channel, Error: err.Error()})
	}
}

func (s *Session) channel(ch string) (*market, string, error) {
	kind, name, err := parseChannel(ch)
	if err != nil {
		return nil, "", err
	}
	m, ok := s.hub.markets[name]
	if !ok {
		return nil, "", fmt.Errorf("unknown market %q", name)
	}
	return m, kind, nil
}

func (s *Session) subscribe(ch string) error {
	m, kind, err := s.channel(ch)
	if err != nil {
		return err
	}
	s.mu.Lock()
	_, resync := s.subs[ch]
	if !resync && len(s.subs) >= s.hub.cfg.MaxSubscriptions {
		s.mu.Unlock()
		return fmt.Errorf("too many subscriptions (max %d)", s.hub.cfg.MaxSubscriptions)
	}
	s.subs[ch] = kind
	s.mu.Unlock()

	// Ack, snapshot and registration happen under the market lock, so the
	// first update this session sees is exactly snapshot seq + 1.
	m.mu.Lock()
	defer m.mu.Unlock()
	if s.closed.Load() {
		return nil
	}
	set := m.tradeSubs
	ack := Envelope{Type: TypeSubscribed, Channel: ch, Market: m.name, Seq: m.tradeSeq}
	if kind == KindBook {
		set = m.bookSubs
		ack.Seq = m.bookSeq
	}
	s.sendControl(ack)
	if kind == KindBook {
		s.send(TypeBookSnapshot, Envelope{
			Type: TypeBookSnapshot, Channel: ch, Market: m.name, Seq: m.bookSeq, Bids: pairs(m.viewBids), Asks: pairs(m.viewAsks),
		})
	}
	if _, ok := set[s]; !ok {
		set[s] = struct{}{}
		s.hub.subscriptionChanged(kind, 1)
	}
	return nil
}

func (s *Session) unsubscribe(ch string) error {
	if _, _, err := s.channel(ch); err != nil {
		return err
	}
	s.mu.Lock()
	kind, ok := s.subs[ch]
	delete(s.subs, ch)
	s.mu.Unlock()
	if !ok {
		return fmt.Errorf("not subscribed to %q", ch)
	}
	s.removeFromMarket(ch, kind)
	s.sendControl(Envelope{Type: TypeUnsubscribed, Channel: ch})
	return nil
}

func (s *Session) removeFromMarket(ch, kind string) {
	m, _, err := s.channel(ch)
	if err != nil {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	set := m.tradeSubs
	if kind == KindBook {
		set = m.bookSubs
	}
	if _, ok := set[s]; ok {
		delete(set, s)
		s.hub.subscriptionChanged(kind, -1)
	}
}

func (s *Session) sendControl(e Envelope) { s.send("control", e) }

func (s *Session) send(msgType string, e Envelope) {
	data, _ := json.Marshal(e) // Envelope always marshals
	s.enqueue(Message{Data: data, Type: msgType, Enqueued: time.Now()})
}

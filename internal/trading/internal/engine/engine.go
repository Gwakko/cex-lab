package engine

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gwakko/cex-lab/internal/events"
)

// Metrics receives engine measurements. Labels are bounded: market, type, side.
type Metrics interface {
	QueueDepth(market string, n int)
	CommandApplied(market, typ string)
	MatchDuration(market string, d time.Duration)
	BookDepth(market string, bidLevels, askLevels int)
	LastSeq(market string, seq uint64)
}

type nopMetrics struct{}

func (nopMetrics) QueueDepth(string, int)              {}
func (nopMetrics) CommandApplied(string, string)       {}
func (nopMetrics) MatchDuration(string, time.Duration) {}
func (nopMetrics) BookDepth(string, int, int)          {}
func (nopMetrics) LastSeq(string, uint64)              {}

// Listener is told about every successfully applied command that produced
// fills or changed the book, on the writer goroutine, in sequence order. It
// must not block.
type Listener interface {
	Applied(market string, cmd Command, res Result, bids, asks []events.Level)
}

// Option configures an Engine.
type Option func(*Engine)

// WithMetrics reports engine measurements to m.
func WithMetrics(m Metrics) Option { return func(e *Engine) { e.metrics = m } }

// WithListener reports applied commands and book changes to l.
func WithListener(l Listener) Option { return func(e *Engine) { e.listener = l } }

// WithLogger logs every applied command to log.
func WithLogger(l *slog.Logger) Option { return func(e *Engine) { e.logger = l } }

// Engine owns one Book and is its only writer: a single goroutine takes
// commands from a channel, assigns the next sequence number, appends the
// command to the log and only then applies it.
type Engine struct {
	spec     MarketSpec
	log      Log
	reqs     chan request
	quit     chan struct{}
	done     chan struct{}
	stopOnce sync.Once
	lastSeq  atomic.Uint64
	// waiting counts submitters blocked on the writer. Only the writer reports
	// it, after each dequeue, so the gauge is never overwritten by a stale value.
	waiting  atomic.Int64
	metrics  Metrics
	logger   *slog.Logger
	listener Listener
}

type request struct {
	cmd   Command
	query func(*Book)
	reply chan Result
}

// Start recovers the book by replaying log and starts the writer goroutine.
func Start(spec MarketSpec, log Log, opts ...Option) (*Engine, error) {
	cmds, err := log.Load()
	if err != nil {
		return nil, fmt.Errorf("engine: load log: %w", err)
	}
	book, _, err := Replay(spec, cmds)
	if err != nil {
		return nil, fmt.Errorf("engine: replay: %w", err)
	}
	e := &Engine{
		spec:    spec,
		log:     log,
		reqs:    make(chan request),
		quit:    make(chan struct{}),
		done:    make(chan struct{}),
		metrics: nopMetrics{},
		logger:  slog.New(slog.DiscardHandler),
	}
	for _, o := range opts {
		o(e)
	}
	e.logger = e.logger.With("market", spec.Symbol)
	e.lastSeq.Store(book.Seq())
	e.reportBook(book)
	go e.run(book)
	return e, nil
}

func (e *Engine) run(book *Book) {
	defer close(e.done)
	for {
		select {
		case <-e.quit:
			return
		case req := <-e.reqs:
			e.metrics.QueueDepth(e.spec.Symbol, int(e.waiting.Add(-1)))
			if req.query != nil {
				req.query(book)
				req.reply <- Result{}
				continue
			}
			req.reply <- e.execute(book, req.cmd)
		}
	}
}

func (e *Engine) execute(book *Book, cmd Command) Result {
	cmd.Seq = book.Seq() + 1
	if err := e.log.Append(cmd); err != nil {
		e.logger.Error("command log append failed", "seq", cmd.Seq, "trace_id", cmd.TraceID, "error", err)
		return Result{Err: fmt.Errorf("engine: log append: %w", err)}
	}
	start := time.Now()
	res := book.Apply(cmd)
	e.metrics.MatchDuration(e.spec.Symbol, time.Since(start))
	e.lastSeq.Store(cmd.Seq)
	e.metrics.CommandApplied(e.spec.Symbol, cmd.Type.String())
	e.reportBook(book)
	e.notify(book, cmd, res)

	attrs := []any{"seq", cmd.Seq, "type", cmd.Type.String(), "order_id", cmd.Order.ID, "trace_id", cmd.TraceID, "trades", len(res.Trades)}
	if res.Err != nil {
		e.logger.Info("engine command rejected", append(attrs, "error", res.Err)...)
	} else {
		e.logger.Info("engine command applied", attrs...)
	}
	return res
}

func (e *Engine) notify(book *Book, cmd Command, res Result) {
	if e.listener == nil || res.Err != nil {
		return
	}
	bids, asks := book.Changes()
	if len(res.Trades) > 0 || len(bids) > 0 || len(asks) > 0 {
		e.listener.Applied(e.spec.Symbol, cmd, res, bids, asks)
	}
}

func (e *Engine) reportBook(book *Book) {
	bids, asks := book.Depth()
	e.metrics.BookDepth(e.spec.Symbol, bids, asks)
	e.metrics.LastSeq(e.spec.Symbol, book.Seq())
}

// Submit sends a command and waits for its result. Business rejections are
// returned in Result.Err; the error return is for transport problems.
func (e *Engine) Submit(ctx context.Context, cmd Command) (Result, error) {
	return e.roundTrip(ctx, request{cmd: cmd, reply: make(chan Result, 1)})
}

// Snapshot reads the book on the writer goroutine.
func (e *Engine) Snapshot(ctx context.Context) (Snapshot, error) {
	var s Snapshot
	_, err := e.roundTrip(ctx, request{query: func(b *Book) { s = b.Snapshot() }, reply: make(chan Result, 1)})
	return s, err
}

func (e *Engine) roundTrip(ctx context.Context, req request) (Result, error) {
	e.waiting.Add(1)
	select {
	case e.reqs <- req:
	case <-ctx.Done():
		e.waiting.Add(-1)
		return Result{}, ctx.Err()
	case <-e.done:
		e.waiting.Add(-1)
		return Result{}, ErrStopped
	}
	// Once accepted the command will be applied, so wait for it regardless of ctx.
	select {
	case r := <-req.reply:
		return r, nil
	case <-e.done:
		select {
		case r := <-req.reply:
			return r, nil
		default:
			return Result{}, ErrStopped
		}
	}
}

// LastSeq is the sequence number of the last command written to the log.
func (e *Engine) LastSeq() uint64 { return e.lastSeq.Load() }

// Stop terminates the writer goroutine and waits for it.
func (e *Engine) Stop() {
	e.stopOnce.Do(func() { close(e.quit) })
	<-e.done
}

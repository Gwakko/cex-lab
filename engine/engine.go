package engine

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
)

// Engine owns one Book and is its only writer: a single goroutine takes
// commands from a channel, assigns the next sequence number, appends the
// command to the log and only then applies it.
type Engine struct {
	log      Log
	reqs     chan request
	quit     chan struct{}
	done     chan struct{}
	stopOnce sync.Once
	lastSeq  atomic.Uint64
}

type request struct {
	cmd   Command
	query func(*Book)
	reply chan Result
}

// Start recovers the book by replaying log and starts the writer goroutine.
func Start(spec MarketSpec, log Log) (*Engine, error) {
	cmds, err := log.Load()
	if err != nil {
		return nil, fmt.Errorf("engine: load log: %w", err)
	}
	book, _, err := Replay(spec, cmds)
	if err != nil {
		return nil, fmt.Errorf("engine: replay: %w", err)
	}
	e := &Engine{
		log:  log,
		reqs: make(chan request),
		quit: make(chan struct{}),
		done: make(chan struct{}),
	}
	e.lastSeq.Store(book.Seq())
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
		return Result{Err: fmt.Errorf("engine: log append: %w", err)}
	}
	res := book.Apply(cmd)
	e.lastSeq.Store(cmd.Seq)
	return res
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
	select {
	case e.reqs <- req:
	case <-ctx.Done():
		return Result{}, ctx.Err()
	case <-e.done:
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

package hub

import (
	"errors"

	"github.com/gwakko/cex-lab/internal/events"
)

// ErrGap means a book_update did not follow the last applied sequence number.
// The replica stays unsynced until the next snapshot: resubscribe to resync.
var ErrGap = errors.New("marketdata: sequence gap, resubscribe to resync")

// BookReplica is the client side of a book channel: it applies a snapshot and
// then updates, and detects gaps.
type BookReplica struct {
	market     string
	seq        uint64
	synced     bool
	bids, asks map[int64]int64
}

// NewBookReplica returns an unsynced replica for market.
func NewBookReplica(market string) *BookReplica {
	return &BookReplica{market: market, bids: map[int64]int64{}, asks: map[int64]int64{}}
}

// Apply consumes one server message; messages for other markets or channels
// are ignored.
func (r *BookReplica) Apply(e Envelope) error {
	if e.Market != r.market {
		return nil
	}
	switch e.Type {
	case TypeBookSnapshot:
		clear(r.bids)
		clear(r.asks)
		r.set(e)
		r.seq, r.synced = e.Seq, true
	case TypeBookUpdate:
		if !r.synced || e.Seq != r.seq+1 {
			r.synced = false
			return ErrGap
		}
		r.set(e)
		r.seq = e.Seq
	}
	return nil
}

func (r *BookReplica) set(e Envelope) {
	applyLevels(r.bids, toLevels(e.Bids))
	applyLevels(r.asks, toLevels(e.Asks))
}

// Synced reports whether updates are being applied.
func (r *BookReplica) Synced() bool { return r.synced }

// Seq is the last applied sequence number.
func (r *BookReplica) Seq() uint64 { return r.seq }

// Levels returns the book, bids best first and asks best first.
func (r *BookReplica) Levels() (bids, asks []events.Level) {
	return topN(r.bids, true, len(r.bids)), topN(r.asks, false, len(r.asks))
}

func toLevels(ps [][2]int64) []events.Level {
	out := make([]events.Level, len(ps))
	for i, p := range ps {
		out[i] = events.Level{Price: p[0], Qty: p[1]}
	}
	return out
}

package instrument

import (
	"time"

	"github.com/gwakko/cex-lab/telemetry"
)

// Engine implements engine.Metrics.
type Engine struct {
	queue   *telemetry.Gauge
	applied *telemetry.Counter
	match   *telemetry.Histogram
	depth   *telemetry.Gauge
	lastSeq *telemetry.Gauge
}

// NewEngine registers the engine metrics.
func NewEngine(r *telemetry.Registry) *Engine {
	return &Engine{
		queue:   r.Gauge("engine_command_queue_depth", "Commands waiting for the market's writer goroutine.", "market"),
		applied: r.Counter("engine_commands_applied_total", "Commands logged and applied, including rejected ones.", "market", "type"),
		match:   r.Histogram("engine_match_duration_seconds", "Time to apply one command to the book.", telemetry.LatencyBuckets, "market"),
		depth:   r.Gauge("engine_book_depth", "Price levels on each side of the book.", "market", "side"),
		lastSeq: r.Gauge("engine_last_seq", "Sequence number of the last applied command.", "market"),
	}
}

func (m *Engine) QueueDepth(market string, n int)   { m.queue.Set(float64(n), market) }
func (m *Engine) CommandApplied(market, typ string) { m.applied.Inc(market, typ) }
func (m *Engine) MatchDuration(market string, d time.Duration) {
	m.match.Observe(d.Seconds(), market)
}
func (m *Engine) LastSeq(market string, seq uint64) { m.lastSeq.Set(float64(seq), market) }
func (m *Engine) BookDepth(market string, bids, asks int) {
	m.depth.Set(float64(bids), market, "buy")
	m.depth.Set(float64(asks), market, "sell")
}

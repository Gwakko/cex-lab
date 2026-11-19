package instrument

import "github.com/gwakko/cex-lab/telemetry"

// Chain implements chain.Metrics.
type Chain struct {
	head, confirmed, lag, pending *telemetry.Gauge
	credited, reorgs              *telemetry.Counter
	belowThreshold, duplicates    *telemetry.Counter
	depth                         *telemetry.Histogram
}

// NewChain registers the deposit watcher metrics.
func NewChain(r *telemetry.Registry) *Chain {
	return &Chain{
		head:           r.Gauge("chain_head_block", "Latest block reported by the node."),
		confirmed:      r.Gauge("chain_confirmed_block", "Highest scanned block with the required confirmations."),
		lag:            r.Gauge("chain_watcher_lag_blocks", "Blocks between the node head and the watcher cursor."),
		pending:        r.Gauge("chain_deposits_pending", "Deposits seen but not yet confirmed."),
		credited:       r.Counter("chain_deposits_credited_total", "Deposits credited to the ledger.", "asset"),
		reorgs:         r.Counter("chain_reorgs_total", "Reorgs detected by the watcher."),
		belowThreshold: r.Counter("chain_reorg_below_confirmation_threshold_total", "Reorgs that orphaned already credited deposits. Any increase is a page."),
		duplicates:     r.Counter("chain_duplicate_deliveries_total", "Transfers delivered again after being seen or credited."),
		depth:          r.Histogram("chain_reorg_depth_blocks", "Depth of detected reorgs in blocks.", []float64{1, 2, 3, 5, 10, 20, 50, 100}),
	}
}

func (m *Chain) Head(b uint64)         { m.head.Set(float64(b)) }
func (m *Chain) Confirmed(b uint64)    { m.confirmed.Set(float64(b)) }
func (m *Chain) Lag(n uint64)          { m.lag.Set(float64(n)) }
func (m *Chain) Pending(n int)         { m.pending.Set(float64(n)) }
func (m *Chain) Credited(asset string) { m.credited.Inc(asset) }
func (m *Chain) ReorgBelowThreshold()  { m.belowThreshold.Inc() }
func (m *Chain) DuplicateDelivery()    { m.duplicates.Inc() }
func (m *Chain) Reorg(depth uint64) {
	m.reorgs.Inc()
	m.depth.Observe(float64(depth))
}

package instrument

import (
	"time"

	"github.com/gwakko/cex-lab/internal/platform/telemetry"
)

// Exchange implements exchange.Metrics, including the invariant signals.
type Exchange struct {
	placed, rejected, cancelled *telemetry.Counter
	trades, volume              *telemetry.Counter
	orderToTrade, placeDuration *telemetry.Histogram
	checks, violations          *telemetry.Counter
	lastCheck, ok               *telemetry.Gauge
}

// NewExchange registers the exchange and invariant metrics.
func NewExchange(r *telemetry.Registry) *Exchange {
	return &Exchange{
		placed:        r.Counter("exchange_orders_placed_total", "Orders accepted by the engine.", "market", "side", "type"),
		rejected:      r.Counter("exchange_orders_rejected_total", "Orders rejected before or by the engine.", "market", "reason"),
		cancelled:     r.Counter("exchange_orders_cancelled_total", "Orders cancelled by their owner.", "market"),
		trades:        r.Counter("exchange_trades_total", "Trades settled.", "market"),
		volume:        r.Counter("exchange_trade_volume_total", "Traded volume in minimal units of each asset.", "market", "asset"),
		orderToTrade:  r.Histogram("exchange_order_to_trade_latency_seconds", "From order received to its first fill returned by the engine.", telemetry.LatencyBuckets, "market"),
		placeDuration: r.Histogram("exchange_place_order_duration_seconds", "Full Place call: hold, match, settle, release.", telemetry.LatencyBuckets, "market"),
		checks:        r.Counter("invariant_check_runs_total", "Invariant checker runs."),
		violations:    r.Counter("invariant_violations_total", "Invariant violations by invariant. Any increase is a page.", "invariant"),
		lastCheck:     r.Gauge("last_invariant_check_timestamp_seconds", "Unix time of the last invariant check."),
		ok:            r.Gauge("invariant_ok", "1 if the last invariant check passed, 0 if it failed."),
	}
}

func (m *Exchange) OrderPlaced(market, side, typ string) { m.placed.Inc(market, side, typ) }
func (m *Exchange) OrderRejected(market, reason string)  { m.rejected.Inc(market, reason) }
func (m *Exchange) OrderCancelled(market string)         { m.cancelled.Inc(market) }
func (m *Exchange) OrderToTrade(market string, d time.Duration) {
	m.orderToTrade.Observe(d.Seconds(), market)
}
func (m *Exchange) PlaceDuration(market string, d time.Duration) {
	m.placeDuration.Observe(d.Seconds(), market)
}

func (m *Exchange) Trade(market, base, quote string, baseQty, quoteQty int64) {
	m.trades.Inc(market)
	m.volume.Add(float64(baseQty), market, base)
	m.volume.Add(float64(quoteQty), market, quote)
}

func (m *Exchange) InvariantChecked(at time.Time, violated string) {
	m.checks.Inc()
	m.lastCheck.Set(float64(at.Unix()))
	if violated == "" {
		m.ok.Set(1)
		return
	}
	m.ok.Set(0)
	m.violations.Inc(violated)
}

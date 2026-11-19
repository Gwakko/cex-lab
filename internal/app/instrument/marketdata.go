package instrument

import (
	"time"

	"github.com/gwakko/cex-lab/internal/platform/telemetry"
)

// MarketData implements marketdata.Metrics.
type MarketData struct {
	connections, subscriptions *telemetry.Gauge
	sent, dropped              *telemetry.Counter
	latency                    *telemetry.Histogram
}

// NewMarketData registers the market data metrics.
func NewMarketData(r *telemetry.Registry) *MarketData {
	return &MarketData{
		connections:   r.Gauge("marketdata_connections", "Open market data sessions."),
		subscriptions: r.Gauge("marketdata_subscriptions", "Active subscriptions by channel kind (trades, book).", "channel"),
		sent:          r.Counter("marketdata_messages_sent_total", "Messages written to clients by type.", "type"),
		dropped:       r.Counter("marketdata_dropped_slow_consumers_total", "Sessions dropped because their send queue was full."),
		latency:       r.Histogram("marketdata_publish_to_send_latency_seconds", "From enqueue in the hub to a completed socket write.", telemetry.LatencyBuckets),
	}
}

func (m *MarketData) Connections(n int)                { m.connections.Set(float64(n)) }
func (m *MarketData) Subscriptions(kind string, n int) { m.subscriptions.Set(float64(n), kind) }
func (m *MarketData) MessageSent(msgType string)       { m.sent.Inc(msgType) }
func (m *MarketData) SlowConsumerDropped()             { m.dropped.Inc() }
func (m *MarketData) PublishToSend(d time.Duration)    { m.latency.Observe(d.Seconds()) }

package service

import "time"

// Metrics receives exchange measurements. Labels are bounded: market, side,
// type, reason, asset, invariant - never user or order ids.
type Metrics interface {
	OrderPlaced(market, side, typ string)
	OrderRejected(market, reason string)
	OrderCancelled(market string)
	Trade(market, base, quote string, baseQty, quoteQty int64)
	OrderToTrade(market string, d time.Duration)
	PlaceDuration(market string, d time.Duration)
	// InvariantChecked reports one Check run; violated is "" when all held.
	InvariantChecked(at time.Time, violated string)
}

type nopMetrics struct{}

func (nopMetrics) OrderPlaced(string, string, string)         {}
func (nopMetrics) OrderRejected(string, string)               {}
func (nopMetrics) OrderCancelled(string)                      {}
func (nopMetrics) Trade(string, string, string, int64, int64) {}
func (nopMetrics) OrderToTrade(string, time.Duration)         {}
func (nopMetrics) PlaceDuration(string, time.Duration)        {}
func (nopMetrics) InvariantChecked(time.Time, string)         {}

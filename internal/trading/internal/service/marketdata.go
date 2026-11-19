package service

import (
	"time"

	"github.com/gwakko/cex-lab/internal/events"
	"github.com/gwakko/cex-lab/internal/trading/internal/engine"
)

// marketData turns engine notifications into public events. Only public
// fields are copied: no user or order ids.
type marketData struct{ sink MarketDataSink }

func (m marketData) Applied(market string, cmd engine.Command, res engine.Result, bids, asks []events.Level) {
	now := time.Now().UTC()
	for i, t := range res.Trades {
		m.sink.PublishTrade(events.TradeV1{
			Market: market, Seq: t.Seq, Index: i, Price: t.Price, Qty: t.Qty,
			TakerSide: t.TakerSide.String(), TraceID: cmd.TraceID, Time: now,
		})
	}
	if len(bids) > 0 || len(asks) > 0 {
		m.sink.PublishBookDelta(events.BookDeltaV1{Market: market, Seq: cmd.Seq, Bids: bids, Asks: asks, TraceID: cmd.TraceID, Time: now})
	}
}

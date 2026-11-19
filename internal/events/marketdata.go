package events

import "time"

// Public market data contracts emitted by trading. The V1 suffix is the
// schema version: a change in meaning gets a new type, never an edit here.
// They carry no user or order ids, so they are safe for public channels.

// Level is the aggregate resting quantity at one price.
type Level struct {
	Price int64
	Qty   int64
}

// TradeV1 is one fill.
type TradeV1 struct {
	Market    string
	Seq       uint64 // engine command sequence that produced the fill
	Index     int    // position of the fill within that command
	Price     int64
	Qty       int64
	TakerSide string // "buy" or "sell"
	TraceID   string // correlation id of the taker order; for logs, not for public channels
	Time      time.Time
}

// BookDeltaV1 lists the price levels changed by one engine command. Qty is the
// new aggregate quantity at that price; 0 means the level is gone.
type BookDeltaV1 struct {
	Market  string
	Seq     uint64 // engine command sequence
	Bids    []Level
	Asks    []Level
	TraceID string
	Time    time.Time
}

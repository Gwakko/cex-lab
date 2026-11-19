package hub

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/gwakko/cex-lab/internal/events"
)

// Channel kinds.
const (
	KindTrades = "trades"
	KindBook   = "book"
)

// Server message types.
const (
	TypeSubscribed   = "subscribed"
	TypeUnsubscribed = "unsubscribed"
	TypeError        = "error"
	TypeBookSnapshot = "book_snapshot"
	TypeBookUpdate   = "book_update"
	TypeTrade        = "trade"
)

// Request is a client message: {"op":"subscribe","channel":"book:BTC-USDT"}.
// Subscribing again to a book channel is how a client resyncs: it gets a
// fresh snapshot.
type Request struct {
	Op      string `json:"op"`
	Channel string `json:"channel"`
}

// Envelope is every server message. Levels are [price, qty] pairs; qty 0 in a
// book_update removes the level. There are no user or order ids.
type Envelope struct {
	Type      string     `json:"type"`
	Channel   string     `json:"channel,omitempty"`
	Market    string     `json:"market,omitempty"`
	Seq       uint64     `json:"seq,omitempty"`
	Bids      [][2]int64 `json:"bids,omitempty"`
	Asks      [][2]int64 `json:"asks,omitempty"`
	Price     int64      `json:"price,omitempty"`
	Qty       int64      `json:"qty,omitempty"`
	TakerSide string     `json:"taker_side,omitempty"`
	Time      string     `json:"time,omitempty"`
	Error     string     `json:"error,omitempty"`
}

// TradesChannel and BookChannel name the public channels of a market.
func TradesChannel(market string) string { return KindTrades + ":" + market }
func BookChannel(market string) string   { return KindBook + ":" + market }

func parseRequest(raw []byte) (Request, error) {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	var r Request
	if err := dec.Decode(&r); err != nil {
		return Request{}, fmt.Errorf("malformed request: %v", err)
	}
	if dec.More() {
		return Request{}, fmt.Errorf("malformed request: trailing data")
	}
	return r, nil
}

func parseChannel(ch string) (kind, market string, err error) {
	kind, market, ok := strings.Cut(ch, ":")
	if !ok || market == "" || (kind != KindTrades && kind != KindBook) {
		return "", "", fmt.Errorf("invalid channel %q: want trades:<market> or book:<market>", ch)
	}
	return kind, market, nil
}

func pairs(levels []events.Level) [][2]int64 {
	if len(levels) == 0 {
		return nil
	}
	out := make([][2]int64, len(levels))
	for i, l := range levels {
		out[i] = [2]int64{l.Price, l.Qty}
	}
	return out
}

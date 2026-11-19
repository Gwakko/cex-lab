package ws

import (
	"context"
	"encoding/json"

	"github.com/coder/websocket"
	"github.com/gwakko/cex-lab/internal/marketdata"
)

// Client is a minimal market data client, used by the demo and tests so they
// do not depend on the WebSocket library directly.
type Client struct {
	c *websocket.Conn
}

// Dial connects to a /ws endpoint (ws:// or wss:// URL).
func Dial(ctx context.Context, url string) (*Client, error) {
	c, _, err := websocket.Dial(ctx, url, nil)
	if err != nil {
		return nil, err
	}
	c.SetReadLimit(1 << 20)
	return &Client{c: c}, nil
}

// Subscribe asks for a channel (subscribing again to a book channel resyncs).
func (cl *Client) Subscribe(ctx context.Context, channel string) error {
	return cl.request(ctx, "subscribe", channel)
}

// Unsubscribe stops a channel.
func (cl *Client) Unsubscribe(ctx context.Context, channel string) error {
	return cl.request(ctx, "unsubscribe", channel)
}

func (cl *Client) request(ctx context.Context, op, channel string) error {
	b, err := json.Marshal(marketdata.Request{Op: op, Channel: channel})
	if err != nil {
		return err
	}
	return cl.SendRaw(ctx, b)
}

// SendRaw sends an arbitrary text frame.
func (cl *Client) SendRaw(ctx context.Context, data []byte) error {
	return cl.c.Write(ctx, websocket.MessageText, data)
}

// Read returns the next server message, decoded and raw.
func (cl *Client) Read(ctx context.Context) (marketdata.Envelope, []byte, error) {
	_, data, err := cl.c.Read(ctx)
	if err != nil {
		return marketdata.Envelope{}, nil, err
	}
	var e marketdata.Envelope
	return e, data, json.Unmarshal(data, &e)
}

// Close closes the connection normally.
func (cl *Client) Close() error { return cl.c.Close(websocket.StatusNormalClosure, "") }

// CloseStatus extracts the close code from a Read error, or -1.
func CloseStatus(err error) int { return int(websocket.CloseStatus(err)) }

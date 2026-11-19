// Package ws is the WebSocket transport for the marketdata module. It is the
// only package that imports the WebSocket library. Each connection is one
// marketdata.Session: a reader goroutine feeds client frames to the session,
// and a writer loop sends queued messages, pings the client, and closes the
// connection with a status that says why (slow consumer, shutdown).
package ws

import (
	"cmp"
	"context"
	"errors"
	"log/slog"
	"net/http"
	"time"

	"github.com/coder/websocket"
	"github.com/gwakko/cex-lab/internal/marketdata"
)

// Close status codes clients can see.
const (
	StatusNormalClosure   = int(websocket.StatusNormalClosure)
	StatusGoingAway       = int(websocket.StatusGoingAway)
	StatusPolicyViolation = int(websocket.StatusPolicyViolation)
	StatusMessageTooBig   = int(websocket.StatusMessageTooBig)
)

// Config tunes the transport. Zero values get defaults.
type Config struct {
	PingInterval   time.Duration // default 15s
	WriteTimeout   time.Duration // per message and per ping; default 5s
	ReadLimit      int64         // max client frame size in bytes; default 4096
	OriginPatterns []string      // allowed cross-origin hosts; empty means same origin only
	Logger         *slog.Logger
}

// Handler upgrades HTTP requests on /ws to market data sessions.
type Handler struct {
	hub *marketdata.Hub
	cfg Config
}

// NewHandler serves hub over WebSocket.
func NewHandler(hub *marketdata.Hub, cfg Config) *Handler {
	cfg.PingInterval = cmp.Or(cfg.PingInterval, 15*time.Second)
	cfg.WriteTimeout = cmp.Or(cfg.WriteTimeout, 5*time.Second)
	cfg.ReadLimit = cmp.Or(cfg.ReadLimit, 4096)
	if cfg.Logger == nil {
		cfg.Logger = slog.New(slog.DiscardHandler)
	}
	return &Handler{hub: hub, cfg: cfg}
}

func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	sess, err := h.hub.Connect()
	if err != nil {
		code := http.StatusServiceUnavailable
		if !errors.Is(err, marketdata.ErrTooManyConnections) && !errors.Is(err, marketdata.ErrClosed) {
			code = http.StatusInternalServerError
		}
		http.Error(w, err.Error(), code)
		return
	}
	defer sess.Close()
	c, err := websocket.Accept(w, r, &websocket.AcceptOptions{OriginPatterns: h.cfg.OriginPatterns})
	if err != nil {
		return // Accept already wrote the HTTP error
	}
	c.SetReadLimit(h.cfg.ReadLimit)

	// The connection outlives the request context once hijacked.
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() {
		defer cancel()
		for {
			_, data, err := c.Read(ctx)
			if err != nil {
				return
			}
			sess.Handle(data)
		}
	}()

	code, reason := h.writeLoop(ctx, c, sess)
	h.cfg.Logger.Info("marketdata connection closed", "code", code, "reason", reason)
	c.Close(code, reason)
}

func (h *Handler) writeLoop(ctx context.Context, c *websocket.Conn, sess *marketdata.Session) (websocket.StatusCode, string) {
	ping := time.NewTicker(h.cfg.PingInterval)
	defer ping.Stop()
	for {
		select {
		case m := <-sess.Out():
			wctx, cancel := context.WithTimeout(ctx, h.cfg.WriteTimeout)
			err := c.Write(wctx, websocket.MessageText, m.Data)
			cancel()
			if err != nil {
				return websocket.StatusPolicyViolation, "write timeout"
			}
			sess.Delivered(m)
		case <-sess.Done():
			switch sess.Reason() {
			case marketdata.ReasonSlowConsumer:
				return websocket.StatusPolicyViolation, "slow consumer"
			case marketdata.ReasonShutdown:
				return websocket.StatusGoingAway, "server shutdown"
			}
			return websocket.StatusNormalClosure, ""
		case <-ping.C:
			pctx, cancel := context.WithTimeout(ctx, h.cfg.WriteTimeout)
			err := c.Ping(pctx)
			cancel()
			if err != nil {
				return websocket.StatusPolicyViolation, "ping timeout"
			}
		case <-ctx.Done():
			return websocket.StatusNormalClosure, ""
		}
	}
}

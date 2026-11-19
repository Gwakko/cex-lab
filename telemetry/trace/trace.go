// Package trace carries a correlation id through context, so one order can be
// followed in logs from the API call to the engine command, the ledger
// transaction and the outbox event.
package trace

import (
	"context"
	"crypto/rand"
	"encoding/hex"
)

type key struct{}

// WithID returns ctx carrying id.
func WithID(ctx context.Context, id string) context.Context {
	return context.WithValue(ctx, key{}, id)
}

// ID returns the id in ctx, or "".
func ID(ctx context.Context) string {
	id, _ := ctx.Value(key{}).(string)
	return id
}

// NewID returns a random 16-hex-character id.
func NewID() string {
	var b [8]byte
	_, _ = rand.Read(b[:]) // crypto/rand.Read never fails on supported platforms
	return hex.EncodeToString(b[:])
}

// Ensure returns ctx with an id, creating one if ctx has none.
func Ensure(ctx context.Context) (context.Context, string) {
	if id := ID(ctx); id != "" {
		return ctx, id
	}
	id := NewID()
	return WithID(ctx, id), id
}

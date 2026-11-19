// Package watch watches a blockchain for deposits to exchange addresses and
// credits them only after enough confirmations, surviving reorgs.
package watch

import (
	"context"
	"errors"
	"fmt"
)

var ErrNotFound = errors.New("chain: not found")

// Header identifies a block and links it to its parent.
type Header struct {
	Number uint64
	Hash   string
	Parent string
}

// Transfer is a token transfer log. (TxHash, LogIndex) is unique on chain.
// Amounts are int64 minimal units, which is a lab simplification: 18-decimal
// tokens would need big.Int or a scaled unit.
type Transfer struct {
	TxHash   string
	LogIndex uint32
	To       string
	Asset    string
	Amount   int64
}

// Key is the idempotency key for crediting this transfer.
func (t Transfer) Key() string { return fmt.Sprintf("%s:%d", t.TxHash, t.LogIndex) }

// ChainClient is the minimal read API the watcher needs from a node.
type ChainClient interface {
	LatestBlock(ctx context.Context) (uint64, error)
	HeaderByNumber(ctx context.Context, n uint64) (Header, error)
	TransfersTo(ctx context.Context, blockHash string, addrs []string) ([]Transfer, error)
}

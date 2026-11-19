// Package fakechain is a deterministic in-memory chain for tests and the
// demo. Reorg replaces the tip with a new branch whose hashes differ.
package fakechain

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"slices"
	"sync"

	"github.com/gwakko/cex-lab/internal/custody"
)

// Chain is the fake chain. It implements custody.ChainClient.
type Chain struct {
	mu        sync.Mutex
	canon     []custody.Header
	transfers map[string][]custody.Transfer // by block hash, including orphaned blocks
	fork      int
}

// New returns a chain containing only the genesis block (number 0).
func New() *Chain {
	c := &Chain{transfers: map[string][]custody.Transfer{}}
	c.append(nil)
	return c
}

// Mine appends a block with the given transfers.
func (c *Chain) Mine(ts ...custody.Transfer) custody.Header {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.append(ts)
}

// MineEmpty appends n empty blocks.
func (c *Chain) MineEmpty(n int) {
	for range n {
		c.Mine()
	}
}

// Reorg drops the top depth blocks and appends one new block per branch entry.
func (c *Chain) Reorg(depth int, branch ...[]custody.Transfer) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if depth <= 0 || depth >= len(c.canon) {
		return fmt.Errorf("fake chain: cannot reorg %d of %d blocks", depth, len(c.canon))
	}
	c.canon = c.canon[:len(c.canon)-depth]
	c.fork++
	for _, ts := range branch {
		c.append(ts)
	}
	return nil
}

func (c *Chain) append(ts []custody.Transfer) custody.Header {
	n := uint64(len(c.canon))
	var parent string
	if n > 0 {
		parent = c.canon[n-1].Hash
	}
	sum := sha256.Sum256(fmt.Appendf(nil, "%d|%s|%d|%v", n, parent, c.fork, ts))
	h := custody.Header{Number: n, Hash: hex.EncodeToString(sum[:8]), Parent: parent}
	c.canon = append(c.canon, h)
	c.transfers[h.Hash] = slices.Clone(ts)
	return h
}

var _ custody.ChainClient = (*Chain)(nil)

func (c *Chain) LatestBlock(context.Context) (uint64, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return uint64(len(c.canon) - 1), nil
}

func (c *Chain) HeaderByNumber(_ context.Context, n uint64) (custody.Header, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if n >= uint64(len(c.canon)) {
		return custody.Header{}, fmt.Errorf("%w: block %d", custody.ErrNotFound, n)
	}
	return c.canon[n], nil
}

func (c *Chain) TransfersTo(_ context.Context, blockHash string, addrs []string) ([]custody.Transfer, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	ts, ok := c.transfers[blockHash]
	if !ok {
		return nil, fmt.Errorf("%w: block %s", custody.ErrNotFound, blockHash)
	}
	var out []custody.Transfer
	for _, t := range ts {
		if slices.Contains(addrs, t.To) {
			out = append(out, t)
		}
	}
	return out, nil
}

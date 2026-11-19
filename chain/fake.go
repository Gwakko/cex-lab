package chain

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"slices"
	"sync"
)

// FakeChain is a deterministic in-memory chain for tests. Reorg replaces the
// tip with a new branch whose hashes differ from the old one.
type FakeChain struct {
	mu        sync.Mutex
	canon     []Header
	transfers map[string][]Transfer // by block hash, including orphaned blocks
	fork      int
}

// NewFakeChain returns a chain containing only the genesis block (number 0).
func NewFakeChain() *FakeChain {
	c := &FakeChain{transfers: map[string][]Transfer{}}
	c.append(nil)
	return c
}

// Mine appends a block with the given transfers.
func (c *FakeChain) Mine(ts ...Transfer) Header {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.append(ts)
}

// MineEmpty appends n empty blocks.
func (c *FakeChain) MineEmpty(n int) {
	for range n {
		c.Mine()
	}
}

// Reorg drops the top depth blocks and appends one new block per branch entry.
func (c *FakeChain) Reorg(depth int, branch ...[]Transfer) error {
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

func (c *FakeChain) append(ts []Transfer) Header {
	n := uint64(len(c.canon))
	var parent string
	if n > 0 {
		parent = c.canon[n-1].Hash
	}
	sum := sha256.Sum256(fmt.Appendf(nil, "%d|%s|%d|%v", n, parent, c.fork, ts))
	h := Header{Number: n, Hash: hex.EncodeToString(sum[:8]), Parent: parent}
	c.canon = append(c.canon, h)
	c.transfers[h.Hash] = slices.Clone(ts)
	return h
}

func (c *FakeChain) LatestBlock(context.Context) (uint64, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return uint64(len(c.canon) - 1), nil
}

func (c *FakeChain) HeaderByNumber(_ context.Context, n uint64) (Header, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if n >= uint64(len(c.canon)) {
		return Header{}, fmt.Errorf("%w: block %d", ErrNotFound, n)
	}
	return c.canon[n], nil
}

func (c *FakeChain) TransfersTo(_ context.Context, blockHash string, addrs []string) ([]Transfer, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	ts, ok := c.transfers[blockHash]
	if !ok {
		return nil, fmt.Errorf("%w: block %s", ErrNotFound, blockHash)
	}
	var out []Transfer
	for _, t := range ts {
		if slices.Contains(addrs, t.To) {
			out = append(out, t)
		}
	}
	return out, nil
}

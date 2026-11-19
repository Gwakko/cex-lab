package chain

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"maps"
	"slices"
)

var (
	ErrReorgTooDeep            = errors.New("chain: reorg deeper than the tracked hash window")
	ErrConfirmedDepositReorged = errors.New("chain: reorg removed a block with credited deposits")
)

// ReorgAlert is returned when a reorg orphans blocks that held deposits
// already credited, i.e. the confirmation threshold was too low. The watcher
// does not revert credits on its own and halts until an operator intervenes.
type ReorgAlert struct {
	Ancestor uint64
	Deposits []Deposit
}

func (a *ReorgAlert) Error() string {
	return fmt.Sprintf("%v: common ancestor %d, %d credited deposit(s) orphaned", ErrConfirmedDepositReorged, a.Ancestor, len(a.Deposits))
}

func (a *ReorgAlert) Unwrap() error { return ErrConfirmedDepositReorged }

// Crediter books a confirmed deposit. It must be idempotent by Transfer.Key:
// after a crash the watcher may credit the same deposit again.
type Crediter func(Deposit) error

// Config tunes the watcher.
type Config struct {
	Confirmations uint64   // a deposit in block b is credited once tip >= b + Confirmations - 1
	Watched       []string // exchange deposit addresses
	StartBlock    uint64   // first block to scan when there is no saved state
	HashWindow    uint64   // how many recent block hashes to keep; default 128
}

// Watcher polls a Client, tracks pending deposits and credits confirmed ones.
type Watcher struct {
	client Client
	store  StateStore
	credit Crediter
	cfg    Config
	st     State
}

// NewWatcher resumes from the saved state, or starts at cfg.StartBlock.
func NewWatcher(c Client, store StateStore, credit Crediter, cfg Config) (*Watcher, error) {
	if cfg.HashWindow == 0 {
		cfg.HashWindow = 128
	}
	if cfg.Confirmations == 0 || cfg.HashWindow <= cfg.Confirmations {
		return nil, fmt.Errorf("chain: need 0 < confirmations (%d) < hash window (%d)", cfg.Confirmations, cfg.HashWindow)
	}
	st, ok, err := store.Load()
	if err != nil {
		return nil, fmt.Errorf("chain: load state: %w", err)
	}
	if !ok {
		st = State{Next: cfg.StartBlock}
	}
	st.init()
	return &Watcher{client: c, store: store, credit: credit, cfg: cfg, st: st}, nil
}

// Poll handles any reorg, scans new blocks, credits confirmed deposits and
// saves the cursor.
func (w *Watcher) Poll(ctx context.Context) error {
	latest, err := w.client.LatestBlock(ctx)
	if err != nil {
		return err
	}
	if err := w.handleReorg(ctx, latest); err != nil {
		return err
	}
	if err := w.scan(ctx, latest); err != nil {
		return err
	}
	if err := w.confirm(); err != nil {
		return err
	}
	w.prune()
	return w.store.Save(w.st)
}

// handleReorg walks back from our tip until our recorded hash matches the
// chain, then rewinds to that common ancestor.
func (w *Watcher) handleReorg(ctx context.Context, latest uint64) error {
	if len(w.st.Hashes) == 0 {
		return nil
	}
	tip := w.st.Next - 1
	for n := tip; ; n-- {
		ours, ok := w.st.Hashes[n]
		if !ok {
			return fmt.Errorf("%w: no common ancestor above block %d", ErrReorgTooDeep, n)
		}
		if n <= latest {
			h, err := w.client.HeaderByNumber(ctx, n)
			if err != nil {
				return err
			}
			if h.Hash == ours {
				if n == tip {
					return nil
				}
				return w.rewind(n)
			}
		}
		if n == 0 {
			return fmt.Errorf("%w: genesis differs", ErrReorgTooDeep)
		}
	}
}

func (w *Watcher) rewind(ancestor uint64) error {
	var orphaned []Deposit
	for _, d := range sorted(w.st.Credited) {
		if d.Block > ancestor {
			orphaned = append(orphaned, d)
		}
	}
	if len(orphaned) > 0 {
		return &ReorgAlert{Ancestor: ancestor, Deposits: orphaned}
	}
	maps.DeleteFunc(w.st.Pending, func(_ string, d Deposit) bool { return d.Block > ancestor })
	maps.DeleteFunc(w.st.Hashes, func(n uint64, _ string) bool { return n > ancestor })
	w.st.Next = ancestor + 1
	return nil
}

func (w *Watcher) scan(ctx context.Context, latest uint64) error {
	for n := w.st.Next; n <= latest; n++ {
		h, err := w.client.HeaderByNumber(ctx, n)
		if err != nil {
			return err
		}
		if len(w.st.Hashes) == 0 && n > 0 {
			w.st.Hashes[n-1] = h.Parent // anchor so a reorg of the first scanned block is detectable
		}
		if prev, ok := w.st.Hashes[n-1]; ok && n > 0 && h.Parent != prev {
			return nil // the chain moved under us; the next Poll rewinds
		}
		ts, err := w.client.TransfersTo(ctx, h.Hash, w.cfg.Watched)
		if err != nil {
			return err
		}
		for _, t := range ts {
			if _, done := w.st.Credited[t.Key()]; !done {
				w.st.Pending[t.Key()] = Deposit{Transfer: t, Block: n, BlockHash: h.Hash}
			}
		}
		w.st.Hashes[n] = h.Hash
		w.st.Next = n + 1
	}
	return nil
}

func (w *Watcher) confirm() error {
	tip := w.st.Next - 1
	for _, d := range sorted(w.st.Pending) {
		if tip+1 < d.Block+w.cfg.Confirmations {
			continue
		}
		if err := w.credit(d); err != nil {
			return fmt.Errorf("chain: credit %s: %w", d.Key(), err)
		}
		delete(w.st.Pending, d.Key())
		w.st.Credited[d.Key()] = d
	}
	return nil
}

func (w *Watcher) prune() {
	if w.st.Next <= w.cfg.HashWindow {
		return
	}
	floor := w.st.Next - w.cfg.HashWindow
	maps.DeleteFunc(w.st.Hashes, func(n uint64, _ string) bool { return n < floor })
}

// Pending returns deposits seen but not yet confirmed, ordered by block.
func (w *Watcher) Pending() []Deposit { return sorted(w.st.Pending) }

// Credited returns deposits already credited, ordered by block.
func (w *Watcher) Credited() []Deposit { return sorted(w.st.Credited) }

func sorted(m map[string]Deposit) []Deposit {
	out := slices.Collect(maps.Values(m))
	slices.SortFunc(out, func(a, b Deposit) int {
		return cmp.Or(cmp.Compare(a.Block, b.Block), cmp.Compare(a.TxHash, b.TxHash), cmp.Compare(a.LogIndex, b.LogIndex))
	})
	return out
}

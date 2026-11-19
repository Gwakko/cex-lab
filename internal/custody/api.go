// Package custody is the custody module: it watches a chain for deposits to
// exchange addresses and credits them through DepositCreditor once they have
// enough confirmations, handling reorgs and restarts.
//
// This file is the module's public API. The implementation lives in
// internal/watch.
package custody

import "github.com/gwakko/cex-lab/internal/custody/internal/watch"

// Watcher polls a ChainClient. Methods: Poll, Pending, Credited.
type Watcher = watch.Watcher

// NewWatcher resumes from the saved cursor, or starts at cfg.StartBlock.
func NewWatcher(c ChainClient, cursor CursorStore, credit DepositCreditor, cfg Config) (*Watcher, error) {
	return watch.NewWatcher(c, cursor, credit, cfg)
}

// Configuration, chain data and results.
type (
	Config     = watch.Config
	Header     = watch.Header
	Transfer   = watch.Transfer
	Deposit    = watch.Deposit
	State      = watch.State
	ReorgAlert = watch.ReorgAlert
)

// Errors. ChainClient implementations return ErrNotFound for unknown blocks.
var (
	ErrNotFound                = watch.ErrNotFound
	ErrReorgTooDeep            = watch.ErrReorgTooDeep
	ErrConfirmedDepositReorged = watch.ErrConfirmedDepositReorged
)

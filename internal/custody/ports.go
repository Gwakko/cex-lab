package custody

import "github.com/gwakko/cex-lab/internal/custody/internal/watch"

// Ports. ChainClient reads the chain (adapter: adapters/fakechain), CursorStore
// persists the watcher state (adapters/memcursor, adapters/filecursor),
// DepositCreditor books a confirmed deposit and must be idempotent by
// Transfer.Key (internal/app wires it to the ledger), Metrics is implemented
// by internal/app/instrument.
type (
	ChainClient     = watch.ChainClient
	CursorStore     = watch.CursorStore
	DepositCreditor = watch.DepositCreditor
	Metrics         = watch.Metrics
)

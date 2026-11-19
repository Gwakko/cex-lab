package ledger

import "github.com/gwakko/cex-lab/internal/ledger/internal/core"

// Store persists committed transactions (adapters: adapters/filestore,
// adapters/memstore). Append must be durable before it returns.
type Store = core.Store

// Metrics receives ledger measurements (adapter: internal/app/instrument).
type Metrics = core.Metrics

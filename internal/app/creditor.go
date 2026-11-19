package app

import (
	"context"
	"fmt"

	"github.com/gwakko/cex-lab/internal/custody"
	"github.com/gwakko/cex-lab/internal/ledger"
)

// NewDepositCreditor books confirmed custody deposits as ledger deposits.
// owners maps a watched address to the user it belongs to. The ledger key
// "chain:<txhash>:<logindex>" makes crediting idempotent across restarts.
func NewDepositCreditor(l *ledger.Ledger, owners map[string]string) custody.DepositCreditor {
	return func(ctx context.Context, d custody.Deposit) error {
		user, ok := owners[d.To]
		if !ok {
			return fmt.Errorf("app: deposit to unassigned address %s", d.To)
		}
		_, _, err := l.ApplyContext(ctx, ledger.Deposit{
			Key: "chain:" + d.Key(), User: user, Asset: ledger.Asset(d.Asset), Amount: d.Amount,
		})
		return err
	}
}

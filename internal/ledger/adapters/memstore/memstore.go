// Package memstore is an in-memory ledger Store for tests and the demo.
package memstore

import (
	"sync"

	"github.com/gwakko/cex-lab/internal/ledger"
)

// Store keeps transactions in memory.
type Store struct {
	mu  sync.Mutex
	txs []ledger.Tx
}

var _ ledger.Store = (*Store)(nil)

func (s *Store) Append(tx ledger.Tx) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.txs = append(s.txs, tx)
	return nil
}

func (s *Store) Load() ([]ledger.Tx, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]ledger.Tx(nil), s.txs...), nil
}

// NewLedger opens a ledger on a fresh in-memory store.
func NewLedger(opts ...ledger.Option) *ledger.Ledger {
	l, err := ledger.Open(&Store{}, opts...)
	if err != nil {
		panic(err) // an empty in-memory store has nothing to replay
	}
	return l
}

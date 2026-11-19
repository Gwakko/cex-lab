package core

import "sync"

// memStore is a test-only Store; the real in-memory adapter lives in
// adapters/memstore, which this package cannot import without a cycle.
type memStore struct {
	mu  sync.Mutex
	txs []Tx
}

func (s *memStore) Append(tx Tx) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.txs = append(s.txs, tx)
	return nil
}

func (s *memStore) Load() ([]Tx, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]Tx(nil), s.txs...), nil
}

func newMem(opts ...Option) *Ledger {
	l, err := Open(&memStore{}, opts...)
	if err != nil {
		panic(err)
	}
	return l
}

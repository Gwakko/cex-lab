// Package filestore is the ledger Store adapter backed by an fsynced
// append-only journal file.
package filestore

import (
	"github.com/gwakko/cex-lab/internal/ledger"
	"github.com/gwakko/cex-lab/internal/platform/journal"
)

// Store is an fsynced append-only journal of ledger transactions.
type Store struct {
	j *journal.File
}

var _ ledger.Store = (*Store)(nil)

// Open opens or creates the journal at path. obs may be nil.
func Open(path string, obs journal.Observer) (*Store, error) {
	j, err := journal.Open(path, obs)
	if err != nil {
		return nil, err
	}
	return &Store{j: j}, nil
}

func (s *Store) Append(tx ledger.Tx) error  { return s.j.Append(tx) }
func (s *Store) Load() ([]ledger.Tx, error) { return journal.LoadAll[ledger.Tx](s.j) }
func (s *Store) Close() error               { return s.j.Close() }

// Err reports whether the last append failed (journal not writable).
func (s *Store) Err() error { return s.j.Err() }

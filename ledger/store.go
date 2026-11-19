package ledger

import (
	"sync"

	"github.com/gwakko/cex-lab/internal/journal"
)

// Store persists committed transactions in order. Append must be durable
// before it returns: the ledger only updates memory after a successful Append.
type Store interface {
	Append(Tx) error
	Load() ([]Tx, error)
}

// MemStore keeps transactions in memory (tests, demo).
type MemStore struct {
	mu  sync.Mutex
	txs []Tx
}

func (s *MemStore) Append(tx Tx) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.txs = append(s.txs, tx)
	return nil
}

func (s *MemStore) Load() ([]Tx, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]Tx(nil), s.txs...), nil
}

// FileStore is an fsynced append-only journal of transactions.
type FileStore struct {
	j *journal.File
}

// OpenFileStore opens or creates the journal at path. obs may be nil.
func OpenFileStore(path string, obs journal.Observer) (*FileStore, error) {
	j, err := journal.Open(path, obs)
	if err != nil {
		return nil, err
	}
	return &FileStore{j: j}, nil
}

func (s *FileStore) Append(tx Tx) error  { return s.j.Append(tx) }
func (s *FileStore) Load() ([]Tx, error) { return journal.LoadAll[Tx](s.j) }
func (s *FileStore) Close() error        { return s.j.Close() }

// Err reports whether the last append failed (journal not writable).
func (s *FileStore) Err() error { return s.j.Err() }

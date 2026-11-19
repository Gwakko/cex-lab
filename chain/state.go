package chain

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sync"
)

// Deposit is a transfer seen in a specific block.
type Deposit struct {
	Transfer
	Block     uint64
	BlockHash string
}

// State is the watcher's persisted cursor.
type State struct {
	Next     uint64             // next block to scan
	Hashes   map[uint64]string  // recent scanned block hashes, for reorg detection
	Pending  map[string]Deposit // seen, not yet confirmed
	Credited map[string]Deposit // credited to the ledger
}

func (s *State) init() {
	if s.Hashes == nil {
		s.Hashes = map[uint64]string{}
	}
	if s.Pending == nil {
		s.Pending = map[string]Deposit{}
	}
	if s.Credited == nil {
		s.Credited = map[string]Deposit{}
	}
}

// StateStore persists watcher state between restarts.
type StateStore interface {
	Load() (State, bool, error)
	Save(State) error
}

// MemStateStore stores a JSON copy, so callers cannot share maps with it.
type MemStateStore struct {
	mu   sync.Mutex
	data []byte
}

func (s *MemStateStore) Load() (State, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.data == nil {
		return State{}, false, nil
	}
	var st State
	return st, true, json.Unmarshal(s.data, &st)
}

func (s *MemStateStore) Save(st State) error {
	b, err := json.Marshal(st)
	if err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.data = b
	return nil
}

// FileStateStore writes state atomically: temp file, fsync, rename.
type FileStateStore struct{ Path string }

func (s FileStateStore) Load() (State, bool, error) {
	b, err := os.ReadFile(s.Path)
	if errors.Is(err, os.ErrNotExist) {
		return State{}, false, nil
	}
	if err != nil {
		return State{}, false, err
	}
	var st State
	return st, true, json.Unmarshal(b, &st)
}

func (s FileStateStore) Save(st State) error {
	b, err := json.Marshal(st)
	if err != nil {
		return err
	}
	f, err := os.CreateTemp(filepath.Dir(s.Path), ".watcher-*")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	if _, err := f.Write(b); err != nil {
		f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	return os.Rename(f.Name(), s.Path)
}

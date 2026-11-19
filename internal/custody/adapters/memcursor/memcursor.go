// Package memcursor is an in-memory custody CursorStore for tests and the demo.
package memcursor

import (
	"encoding/json"
	"sync"

	"github.com/gwakko/cex-lab/internal/custody"
)

// Store keeps a JSON copy of the state, so callers cannot share maps with it.
type Store struct {
	mu   sync.Mutex
	data []byte
}

var _ custody.CursorStore = (*Store)(nil)

func (s *Store) Load() (custody.State, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.data == nil {
		return custody.State{}, false, nil
	}
	var st custody.State
	return st, true, json.Unmarshal(s.data, &st)
}

func (s *Store) Save(st custody.State) error {
	b, err := json.Marshal(st)
	if err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.data = b
	return nil
}

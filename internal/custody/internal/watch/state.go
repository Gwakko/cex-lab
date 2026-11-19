package watch

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

// CursorStore persists watcher state between restarts.
type CursorStore interface {
	Load() (State, bool, error)
	Save(State) error
}

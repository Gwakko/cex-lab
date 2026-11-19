package engine

import "errors"

// Log is the ordered command log. A command is appended before it is applied,
// so the log alone is enough to rebuild the book. Adapters live in
// internal/trading/adapters.
type Log interface {
	Append(Command) error
	Load() ([]Command, error)
}

// Replay rebuilds a book from a command log.
func Replay(spec MarketSpec, cmds []Command) (*Book, []Result, error) {
	b, err := NewBook(spec)
	if err != nil {
		return nil, nil, err
	}
	results := make([]Result, 0, len(cmds))
	for _, c := range cmds {
		r := b.Apply(c)
		if errors.Is(r.Err, ErrOutOfOrder) {
			return nil, nil, r.Err
		}
		results = append(results, r)
	}
	return b, results, nil
}

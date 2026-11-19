package engine

import (
	"errors"
	"sync"
)

// memLog is a test-only Log; the real adapters live in
// internal/trading/adapters, which this package cannot import without a cycle.
type memLog struct {
	mu   sync.Mutex
	cmds []Command
}

func (l *memLog) Append(c Command) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.cmds = append(l.cmds, c)
	return nil
}

func (l *memLog) Load() ([]Command, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]Command(nil), l.cmds...), nil
}

type failingLog struct{ memLog }

func (*failingLog) Append(Command) error { return errors.New("disk full") }

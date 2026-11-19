// Package memlog is an in-memory trading command log for tests and the demo.
package memlog

import (
	"sync"

	"github.com/gwakko/cex-lab/internal/trading"
)

// Log keeps commands in memory.
type Log struct {
	mu   sync.Mutex
	cmds []trading.Command
}

var _ trading.CommandLog = (*Log)(nil)

func (l *Log) Append(c trading.Command) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.cmds = append(l.cmds, c)
	return nil
}

func (l *Log) Load() ([]trading.Command, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]trading.Command(nil), l.cmds...), nil
}

// Package filelog is the trading command log backed by an fsynced
// append-only journal file.
package filelog

import (
	"github.com/gwakko/cex-lab/internal/platform/journal"
	"github.com/gwakko/cex-lab/internal/trading"
)

// Log is an fsynced append-only command log.
type Log struct {
	j *journal.File
}

var _ trading.CommandLog = (*Log)(nil)

// Open opens or creates the log at path. obs may be nil.
func Open(path string, obs journal.Observer) (*Log, error) {
	j, err := journal.Open(path, obs)
	if err != nil {
		return nil, err
	}
	return &Log{j: j}, nil
}

func (l *Log) Append(c trading.Command) error   { return l.j.Append(c) }
func (l *Log) Load() ([]trading.Command, error) { return journal.LoadAll[trading.Command](l.j) }
func (l *Log) Close() error                     { return l.j.Close() }

// Err reports whether the last append failed (log not writable).
func (l *Log) Err() error { return l.j.Err() }

package engine

import (
	"errors"
	"sync"

	"github.com/gwakko/cex-lab/internal/journal"
)

// Log is the ordered command log. A command is appended before it is applied,
// so the log alone is enough to rebuild the book.
type Log interface {
	Append(Command) error
	Load() ([]Command, error)
}

// MemLog keeps commands in memory (tests, demo).
type MemLog struct {
	mu   sync.Mutex
	cmds []Command
}

func (l *MemLog) Append(c Command) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.cmds = append(l.cmds, c)
	return nil
}

func (l *MemLog) Load() ([]Command, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]Command(nil), l.cmds...), nil
}

// FileLog is an fsynced append-only command log.
type FileLog struct {
	j *journal.File
}

// OpenFileLog opens or creates the log at path.
func OpenFileLog(path string) (*FileLog, error) {
	j, err := journal.Open(path)
	if err != nil {
		return nil, err
	}
	return &FileLog{j: j}, nil
}

func (l *FileLog) Append(c Command) error   { return l.j.Append(c) }
func (l *FileLog) Load() ([]Command, error) { return journal.LoadAll[Command](l.j) }
func (l *FileLog) Close() error             { return l.j.Close() }

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

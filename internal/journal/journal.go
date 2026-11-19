// Package journal is an append-only JSON-lines file. Append returns only after
// the record is fsynced, so an acknowledged record survives a crash.
package journal

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"sync"
	"time"
)

// Observer receives journal measurements.
type Observer interface {
	Synced(d time.Duration, size int64) // one fsynced append; size is the file size after it
	Truncated(records int)              // torn records dropped during Load
}

type nopObserver struct{}

func (nopObserver) Synced(time.Duration, int64) {}
func (nopObserver) Truncated(int)               {}

// File is an append-only journal.
type File struct {
	mu      sync.Mutex
	f       *os.File
	obs     Observer
	size    int64
	lastErr error
}

// Open opens or creates the journal at path. obs may be nil.
func Open(path string, obs Observer) (*File, error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR|os.O_APPEND, 0o600)
	if err != nil {
		return nil, err
	}
	st, err := f.Stat()
	if err != nil {
		f.Close()
		return nil, err
	}
	if obs == nil {
		obs = nopObserver{}
	}
	return &File{f: f, obs: obs, size: st.Size()}, nil
}

// Err returns the error of the last failed append, or nil once one succeeds.
// Readiness checks use it to stop traffic when the journal is not writable.
func (j *File) Err() error {
	j.mu.Lock()
	defer j.mu.Unlock()
	return j.lastErr
}

// Append writes v as one line and fsyncs.
func (j *File) Append(v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	j.mu.Lock()
	defer j.mu.Unlock()
	start := time.Now()
	n, err := j.f.Write(append(b, '\n'))
	j.size += int64(n)
	if err == nil {
		err = j.f.Sync()
	}
	j.lastErr = err
	if err == nil {
		j.obs.Synced(time.Since(start), j.size)
	}
	return err
}

// Load calls fn with every complete record in order. A trailing record without
// a newline is a torn write that was never acknowledged; it is truncated.
func (j *File) Load(fn func([]byte) error) error {
	j.mu.Lock()
	defer j.mu.Unlock()
	if _, err := j.f.Seek(0, io.SeekStart); err != nil {
		return err
	}
	r := bufio.NewReader(j.f)
	var off int64
	for {
		line, err := r.ReadBytes('\n')
		if errors.Is(err, io.EOF) {
			if len(line) == 0 {
				return nil
			}
			if err := j.f.Truncate(off); err != nil {
				return err
			}
			j.size = off
			j.obs.Truncated(1)
			return j.f.Sync()
		}
		if err != nil {
			return err
		}
		if err := fn(line[:len(line)-1]); err != nil {
			return fmt.Errorf("journal: record at offset %d: %w", off, err)
		}
		off += int64(len(line))
	}
}

// LoadAll decodes every record into T.
func LoadAll[T any](j *File) ([]T, error) {
	var out []T
	err := j.Load(func(b []byte) error {
		var v T
		if err := json.Unmarshal(b, &v); err != nil {
			return err
		}
		out = append(out, v)
		return nil
	})
	return out, err
}

// Close closes the underlying file.
func (j *File) Close() error { return j.f.Close() }

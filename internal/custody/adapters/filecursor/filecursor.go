// Package filecursor is a custody CursorStore that writes state atomically to
// a file: temp file, fsync, rename.
package filecursor

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"

	"github.com/gwakko/cex-lab/internal/custody"
)

// Store persists watcher state at Path.
type Store struct{ Path string }

var _ custody.CursorStore = Store{}

func (s Store) Load() (custody.State, bool, error) {
	b, err := os.ReadFile(s.Path)
	if errors.Is(err, os.ErrNotExist) {
		return custody.State{}, false, nil
	}
	if err != nil {
		return custody.State{}, false, err
	}
	var st custody.State
	return st, true, json.Unmarshal(b, &st)
}

func (s Store) Save(st custody.State) error {
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

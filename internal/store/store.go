// Package store keeps task state on disk, one file per herdr session, with a
// file lock so hooks, the overview, and agents can write at the same time.
package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"syscall"
	"time"

	"github.com/alekkowalczyk/herdr-tasks/internal/task"
)

const PluginID = "herdr-tasks"

type State struct {
	Version int                   `json:"version"`
	Tasks   map[string]*task.Task `json:"tasks"`
	// Space labels hidden in the overview.
	Hidden []string `json:"hidden_spaces,omitempty"`
	// Set by `herdr-tasks remove`, so hooks still in flight leave tabs alone.
	Removed bool `json:"removed,omitempty"`
	// Show stage counts under each space instead of its task list.
	SidebarCounts bool `json:"sidebar_counts,omitempty"`
}

func (s *State) IsHidden(workspaceLabel string) bool {
	for _, h := range s.Hidden {
		if h == workspaceLabel {
			return true
		}
	}
	return false
}

type Store struct {
	Dir     string
	Session string
}

// Dir is where herdr keeps this plugin's state. Hooks get it from herdr;
// commands run in a pane fall back to the same default location.
func Dir() string {
	if d := os.Getenv("HERDR_PLUGIN_STATE_DIR"); d != "" {
		return d
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".local", "state", "herdr", "plugins", PluginID)
}

func New(session string) *Store { return &Store{Dir: Dir(), Session: session} }

// Path is the state file.
func (s *Store) Path() string { return filepath.Join(s.Dir, s.Session+".json") }

func (s *Store) lockPath() string { return filepath.Join(s.Dir, s.Session+".lock") }

// openLock opens the session's lock file. The state folder holds notes, the
// share keys of artifact links, and the board's token, so it's made yours
// only, as is the lock file: also when an older version or herdr made them
// readable by others. That part is best effort, as the lock works either way.
func (s *Store) openLock() (*os.File, error) {
	if err := os.MkdirAll(s.Dir, 0o700); err != nil {
		return nil, err
	}
	_ = os.Chmod(s.Dir, 0o700)
	f, err := os.OpenFile(s.lockPath(), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	_ = f.Chmod(0o600)
	return f, nil
}

func (s *Store) lock(how int) (func(), error) {
	f, err := s.openLock()
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(f.Fd()), how); err != nil {
		f.Close()
		return nil, err
	}
	return func() {
		_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
		f.Close()
	}, nil
}

func (s *Store) read() (*State, error) {
	st := &State{Version: 1, Tasks: map[string]*task.Task{}}
	b, err := os.ReadFile(s.Path())
	if errors.Is(err, os.ErrNotExist) {
		return st, nil
	}
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal(b, st); err != nil {
		return nil, fmt.Errorf("reading %s: %w", s.Path(), err)
	}
	if st.Tasks == nil {
		st.Tasks = map[string]*task.Task{}
	}
	return st, nil
}

// write saves st as a new file, readable by you only (CreateTemp makes it
// 0600), that takes the old one's place. So a state file an older version
// left readable by others is replaced with one that isn't.
func (s *Store) write(st *State) error {
	b, err := json.MarshalIndent(st, "", "  ")
	if err != nil {
		return err
	}
	f, err := os.CreateTemp(s.Dir, s.Session+".*.tmp")
	if err != nil {
		return err
	}
	_, err = f.Write(append(b, '\n'))
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		err = os.Rename(f.Name(), s.Path())
	}
	if err != nil {
		os.Remove(f.Name())
	}
	return err
}

// Load reads the current state.
func (s *Store) Load() (*State, error) {
	unlock, err := s.lock(syscall.LOCK_SH)
	if err != nil {
		return nil, err
	}
	defer unlock()
	return s.read()
}

// LoadContext is Load, but it stops waiting for the lock when ctx ends, and
// returns ctx.Err(). A writer may hold the lock a while, as a sync does while
// it waits on herdr. Load's wait can't be cut short.
func (s *Store) LoadContext(ctx context.Context) (*State, error) {
	f, err := s.openLock()
	if err != nil {
		return nil, err
	}
	defer f.Close()
	for {
		err := syscall.Flock(int(f.Fd()), syscall.LOCK_SH|syscall.LOCK_NB)
		if err == nil {
			break
		}
		if !errors.Is(err, syscall.EWOULDBLOCK) && !errors.Is(err, syscall.EINTR) {
			return nil, err
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(25 * time.Millisecond):
		}
	}
	defer syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
	return s.read()
}

// Update runs fn on the state under an exclusive lock and saves the result.
// Nothing is saved if fn returns an error.
func (s *Store) Update(fn func(*State) error) error {
	unlock, err := s.lock(syscall.LOCK_EX)
	if err != nil {
		return err
	}
	defer unlock()
	st, err := s.read()
	if err != nil {
		return err
	}
	if err := fn(st); err != nil {
		return err
	}
	return s.write(st)
}

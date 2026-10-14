// Package store keeps task state on disk, one file per herdr session, with a
// file lock so hooks, the overview, and agents can write at the same time.
package store

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"syscall"

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

func (s *Store) path() string     { return filepath.Join(s.Dir, s.Session+".json") }
func (s *Store) lockPath() string { return filepath.Join(s.Dir, s.Session+".lock") }

func (s *Store) lock(how int) (func(), error) {
	if err := os.MkdirAll(s.Dir, 0o755); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(s.lockPath(), os.O_CREATE|os.O_RDWR, 0o644)
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
	b, err := os.ReadFile(s.path())
	if errors.Is(err, os.ErrNotExist) {
		return st, nil
	}
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal(b, st); err != nil {
		return nil, fmt.Errorf("reading %s: %w", s.path(), err)
	}
	if st.Tasks == nil {
		st.Tasks = map[string]*task.Task{}
	}
	return st, nil
}

func (s *Store) write(st *State) error {
	b, err := json.MarshalIndent(st, "", "  ")
	if err != nil {
		return err
	}
	tmp := s.path() + ".tmp"
	if err := os.WriteFile(tmp, append(b, '\n'), 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, s.path())
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

// Package store keeps task state on disk, one file per herdr session, with a
// file lock so hooks, the overview, and agents can write at the same time.
package store

import (
	"context"
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
	// Show stage counts under each space instead of its task list.
	SidebarCounts bool `json:"sidebar_counts,omitempty"`
	// PaneAliases maps the ID a pane had before a move to its ID now, so
	// what knows it by the old one, like the env of the process in it,
	// still finds it.
	PaneAliases map[string]string `json:"pane_aliases,omitempty"`
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

func (s *Store) file() File { return File{Dir: s.Dir, Name: s.Session} }

func (s *Store) lockPath() string { return s.file().lockPath() }

// Path is the state file.
func (s *Store) Path() string { return s.file().Path() }

func (s *Store) read() (*State, error) {
	st := &State{Version: 1, Tasks: map[string]*task.Task{}}
	if err := s.file().read(st); err != nil {
		return nil, err
	}
	if st.Tasks == nil {
		st.Tasks = map[string]*task.Task{}
	}
	return st, nil
}

// Load reads the current state.
func (s *Store) Load() (*State, error) {
	unlock, err := s.file().lock(syscall.LOCK_SH)
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
	unlock, err := s.file().lockShared(ctx)
	if err != nil {
		return nil, err
	}
	defer unlock()
	return s.read()
}

// Hold takes the session's lock called name until unlock is called, for
// work that mustn't run twice at once but takes too long to do under the
// state's lock, such as making the assistant's tab and starting Claude in it.
func (s *Store) Hold(name string) (unlock func(), err error) {
	return File{Dir: s.Dir, Name: s.Session + "." + name}.lock(syscall.LOCK_EX)
}

// Update runs fn on the state under an exclusive lock and saves the result.
// Nothing is saved if fn returns an error.
func (s *Store) Update(fn func(*State) error) error {
	unlock, err := s.file().lock(syscall.LOCK_EX)
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
	return s.file().write(st)
}

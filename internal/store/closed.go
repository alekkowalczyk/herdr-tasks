package store

import (
	"crypto/rand"
	"encoding/hex"
	"slices"
	"time"

	"github.com/alekkowalczyk/herdr-tasks/internal/task"
)

// Closed is a task whose tab closed, kept so you can reopen it.
type Closed struct {
	// ID names it among the closed tasks. Its tab's ID doesn't: herdr
	// hands that to another tab.
	ID   string     `json:"id"`
	At   time.Time  `json:"at"`
	Task *task.Task `json:"task"`
}

// MaxClosed is how many closed tasks the state keeps. The oldest go first.
const MaxClosed = 100

// Close keeps t as a task whose tab closed at now, first in the list.
func (s *State) Close(t *task.Task, now time.Time) {
	c := Closed{ID: s.newClosedID(), At: now, Task: t}
	s.Closed = slices.Insert(s.Closed, 0, c)
	if len(s.Closed) > MaxClosed {
		s.Closed = s.Closed[:MaxClosed]
	}
}

// ClosedTask is the closed task id, if the state still has it.
func (s *State) ClosedTask(id string) (Closed, bool) {
	i := slices.IndexFunc(s.Closed, func(c Closed) bool { return c.ID == id })
	if i < 0 {
		return Closed{}, false
	}
	return s.Closed[i], true
}

// Unclose takes the closed task id off the list and returns it.
func (s *State) Unclose(id string) (*task.Task, bool) {
	i := slices.IndexFunc(s.Closed, func(c Closed) bool { return c.ID == id })
	if i < 0 {
		return nil, false
	}
	t := s.Closed[i].Task
	s.Closed = slices.Delete(s.Closed, i, i+1)
	return t, true
}

// newClosedID is an ID no closed task has.
func (s *State) newClosedID() string {
	for {
		var b [4]byte
		if _, err := rand.Read(b[:]); err != nil {
			panic("crypto/rand: " + err.Error())
		}
		id := hex.EncodeToString(b[:])
		if _, taken := s.ClosedTask(id); !taken {
			return id
		}
	}
}

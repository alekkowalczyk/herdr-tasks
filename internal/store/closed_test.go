package store

import (
	"fmt"
	"testing"
	"time"

	"github.com/alekkowalczyk/herdr-tasks/internal/task"
)

func TestClosedTasksKeepTheNewestFirst(t *testing.T) {
	s := &State{Tasks: map[string]*task.Task{}}
	now := time.Date(2026, 11, 2, 9, 0, 0, 0, time.UTC)
	for i := range MaxClosed + 2 {
		s.Close(&task.Task{TabID: "w1:t1", Title: fmt.Sprintf("task %d", i)}, now.Add(time.Duration(i)*time.Minute))
	}
	if len(s.Closed) != MaxClosed {
		t.Fatalf("kept %d, want %d", len(s.Closed), MaxClosed)
	}
	if got := s.Closed[0].Task.Title; got != fmt.Sprintf("task %d", MaxClosed+1) {
		t.Errorf("first = %q, want the newest", got)
	}
	if got := s.Closed[MaxClosed-1].Task.Title; got != "task 2" {
		t.Errorf("last = %q, want the oldest kept", got)
	}
	if !s.Closed[0].At.Equal(now.Add(time.Duration(MaxClosed+1) * time.Minute)) {
		t.Errorf("at = %v", s.Closed[0].At)
	}
	// The same tab closing again is another closed task, with an ID of its own.
	ids := map[string]bool{}
	for _, c := range s.Closed {
		if c.ID == "" || ids[c.ID] {
			t.Fatalf("ID %q is empty or taken", c.ID)
		}
		ids[c.ID] = true
	}
}

func TestReopenTakesAClosedTaskOff(t *testing.T) {
	s := &State{Tasks: map[string]*task.Task{}}
	now := time.Date(2026, 11, 2, 9, 0, 0, 0, time.UTC)
	s.Close(&task.Task{TabID: "w1:t1", Title: "login loop"}, now)
	s.Close(&task.Task{TabID: "w1:t2", Title: "billing export"}, now)
	id := s.Closed[1].ID
	if c, ok := s.ClosedTask(id); !ok || c.Task.Title != "login loop" {
		t.Fatalf("ClosedTask = %+v, %v", c, ok)
	}
	got, ok := s.Unclose(id)
	if !ok || got.Title != "login loop" {
		t.Fatalf("Unclose = %+v, %v", got, ok)
	}
	if len(s.Closed) != 1 || s.Closed[0].Task.Title != "billing export" {
		t.Fatalf("left = %+v", s.Closed)
	}
	if _, ok := s.Unclose(id); ok {
		t.Fatal("reopened twice")
	}
	if _, ok := s.ClosedTask(id); ok {
		t.Fatal("still listed")
	}
}

func TestClosedTasksAreSaved(t *testing.T) {
	st := &Store{Dir: t.TempDir(), Session: "acme"}
	at := time.Date(2026, 11, 2, 9, 0, 0, 0, time.UTC)
	err := st.Update(func(s *State) error {
		s.Close(&task.Task{TabID: "w1:t1", Title: "login loop", Note: "retry after the deploy"}, at)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	s, err := st.Load()
	if err != nil {
		t.Fatal(err)
	}
	if len(s.Closed) != 1 || s.Closed[0].Task.Note != "retry after the deploy" || !s.Closed[0].At.Equal(at) {
		t.Fatalf("loaded %+v", s.Closed)
	}
}

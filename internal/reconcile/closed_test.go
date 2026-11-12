package reconcile

import (
	"testing"
	"time"

	"github.com/alekkowalczyk/herdr-tasks/internal/herdr"
	"github.com/alekkowalczyk/herdr-tasks/internal/store"
	"github.com/alekkowalczyk/herdr-tasks/internal/task"
)

func TestAClosedTabsTaskIsKept(t *testing.T) {
	f, st := newFake(), newStore(t)
	now := time.Date(2026, 11, 2, 9, 0, 0, 0, time.UTC)
	Run(f, st, on, now, func(s *store.State, _ *herdr.Live) error {
		s.Tasks["w1:t2"].Note = "the runners need the new image"
		s.Tasks["w1:t3"].Title = "logs"
		s.Tasks["w1:t3"].SetStage(task.Tool, "", now)
		return nil
	})
	// w1:t1 has nothing but its stage, w1:t2 is a task, and w1:t3 a tool.
	f.tabs = f.tabs[3:]
	later := now.Add(time.Hour)
	Run(f, st, on, later, nil)
	s, _ := st.Load()
	for _, id := range []string{"w1:t1", "w1:t2", "w1:t3"} {
		if _, ok := s.Tasks[id]; ok {
			t.Errorf("%s is still a task", id)
		}
	}
	if len(s.Closed) != 1 {
		t.Fatalf("closed = %+v, want only the task with something in it", s.Closed)
	}
	c := s.Closed[0]
	if c.Task.Title != "arm CI runners" || c.Task.Note != "the runners need the new image" || c.Task.Workspace != "acme ops" || !c.At.Equal(later) {
		t.Fatalf("closed = %+v at %v", c.Task, c.At)
	}
}

func TestATabThatGotANewIDIsntClosed(t *testing.T) {
	f, st := newFake(), newStore(t)
	Run(f, st, on, time.Now(), nil)
	for i := range f.tabs {
		if f.tabs[i].ID == "w1:t2" {
			f.tabs[i].ID = "w1:t9"
		}
	}
	Run(f, st, on, time.Now(), nil)
	if s, _ := st.Load(); len(s.Closed) != 0 {
		t.Fatalf("closed = %+v", s.Closed)
	}
}

const claudeID = "0b6c1d2e-0000-4000-8000-000000000001"

func TestTasksKeepWhereTheirTabsWork(t *testing.T) {
	f, st := newFakeStrips(), newStore(t)
	// w1:t2 has a shell and Claude; w1:t3 a shell.
	f.panes[1].Cwd = "/home/dev/acme/infra"
	f.add("w1:t2", "")
	claude := &f.panes[len(f.panes)-1]
	claude.Cwd, claude.Agent = "/home/dev/acme/ci", "claude"
	claude.Session = &herdr.AgentSession{Source: "herdr:claude", Agent: "claude", Kind: "id", Value: claudeID}
	f.panes[2].Cwd = "/home/dev/acme/web"
	Run(f, st, withStrips, time.Now(), nil)
	s, _ := st.Load()
	if tk := s.Tasks["w1:t2"]; tk.Cwd != "/home/dev/acme/ci" || tk.Claude != claudeID {
		t.Fatalf("w1:t2 = %q %q, want Claude's folder and session", tk.Cwd, tk.Claude)
	}
	if tk := s.Tasks["w1:t3"]; tk.Cwd != "/home/dev/acme/web" || tk.Claude != "" {
		t.Fatalf("w1:t3 = %q %q", tk.Cwd, tk.Claude)
	}

	// The strip's folder doesn't count, and an exited Claude's session
	// stays. What isn't a session ID isn't kept: it goes on a command line.
	for i, p := range f.panes {
		switch {
		case p.TabID == "w1:t3" && p.Label == StripLabel:
			f.panes[i].Cwd = "/opt/herdr-tasks"
		case p.TabID == "w1:t3":
			f.panes[i].Cwd = "/home/dev/acme/api"
		case p.ID == claude.ID:
			f.panes[i].Agent = ""
		}
	}
	f.add("w1:t1", "")
	f.panes[len(f.panes)-1].Session = &herdr.AgentSession{Agent: "claude", Kind: "id", Value: "--help"}
	Run(f, st, withStrips, time.Now(), nil)
	s, _ = st.Load()
	if tk := s.Tasks["w1:t3"]; tk.Cwd != "/home/dev/acme/api" {
		t.Errorf("w1:t3 = %q, want its shell's folder", tk.Cwd)
	}
	if tk := s.Tasks["w1:t2"]; tk.Claude != claudeID {
		t.Errorf("w1:t2 lost its session: %q", tk.Claude)
	}
	if tk := s.Tasks["w1:t1"]; tk.Claude != "" {
		t.Errorf("w1:t1 kept %q", tk.Claude)
	}

	// Once its tab closes, the closed task has them.
	f.CloseTab("w1:t2")
	Run(f, st, withStrips, time.Now(), nil)
	s, _ = st.Load()
	if len(s.Closed) != 1 || s.Closed[0].Task.Claude != claudeID || s.Closed[0].Task.Cwd != "/home/dev/acme/ci" {
		t.Fatalf("closed = %+v", s.Closed)
	}
}

func TestAnAgentsTurnNotesItsSession(t *testing.T) {
	f, st := newFakeStrips(), newStore(t)
	Run(f, st, withStrips, time.Now(), nil)
	// Claude starts in w2:t1 after the last run.
	for i, p := range f.panes {
		if p.TabID == "w2:t1" && p.Label == "" {
			f.panes[i].Cwd, f.panes[i].Agent = "/home/dev/acme/checkout", "claude"
			f.panes[i].Session = &herdr.AgentSession{Agent: "claude", Kind: "id", Value: claudeID}
		}
	}
	if err := NoteWhere(f, st); err != nil {
		t.Fatal(err)
	}
	s, _ := st.Load()
	if tk := s.Tasks["w2:t1"]; tk.Claude != claudeID || tk.Cwd != "/home/dev/acme/checkout" {
		t.Fatalf("w2:t1 = %q %q", tk.Cwd, tk.Claude)
	}
}

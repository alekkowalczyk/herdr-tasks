package assistant

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/alekkowalczyk/herdr-tasks/internal/herdr"
	"github.com/alekkowalczyk/herdr-tasks/internal/store"
	"github.com/alekkowalczyk/herdr-tasks/internal/task"
)

// The brief lists the tasks in the overview's order, each with where it
// stands, and leaves out tools, the assistant among them, and hidden spaces.
func TestBrief(t *testing.T) {
	now := time.Date(2026, 11, 4, 14, 5, 0, 0, time.Local)
	live := &herdr.Live{
		Workspaces: []herdr.Workspace{{ID: "w1", Label: "Acme"}, {ID: "w2", Label: "Infra"}, {ID: "w3", Label: "Old"}, {ID: "w4", Label: "Assistant"}},
		Tabs: []herdr.Tab{
			{ID: "w1:t1", WorkspaceID: "w1", Number: 1},
			{ID: "w1:t2", WorkspaceID: "w1", Number: 2, AgentStatus: "blocked"},
			{ID: "w1:t3", WorkspaceID: "w1", Number: 3, AgentStatus: "working"},
			{ID: "w2:t1", WorkspaceID: "w2", Number: 1},
			{ID: "w2:t2", WorkspaceID: "w2", Number: 2},
			{ID: "w2:t3", WorkspaceID: "w2", Number: 3},
			{ID: "w3:t1", WorkspaceID: "w3", Number: 1},
			{ID: "w4:t1", WorkspaceID: "w4", Number: 1},
		},
	}
	s := &store.State{Hidden: []string{"Old"}, Tasks: map[string]*task.Task{
		"w1:t1": {Title: "Invoice export", Stage: task.Active, Since: now.Add(-2 * time.Hour)},
		"w1:t2": {Title: "Login loop", Stage: task.Active, Since: now.Add(-3 * time.Hour), Priority: true},
		"w1:t3": {Stage: task.Active, Since: now.Add(-20 * time.Minute)},
		"w2:t1": {Title: "Docs", Stage: task.Review, Reason: "approval", Since: now.Add(-25 * time.Hour)},
		"w2:t2": {Title: "Deploy", Stage: task.Paused, Reason: "the vendor\x1b[31m replies", Since: now.Add(-50 * time.Hour)},
		"w2:t3": {Title: "Logs", Stage: task.Tool, Since: now},
		"w3:t1": {Title: "Hidden", Stage: task.Active, Since: now},
		"w4:t1": {Title: "Assistant", Stage: task.Tool, Since: now, Assistant: &task.Assistant{SessionID: "x"}},
	}}
	want := "The tasks in herdr now (Wed Nov 4 14:05). `herdr-tasks context` has their notes, links, and what their agents said.\n" +
		"- Login loop (w1:t2, Acme): asking, its agent waits for an answer · priority · 3h\n" +
		"- Invoice export (w1:t1, Acme): your turn, its agent is done or there's none · 2h\n" +
		"- tab 3 (w1:t3, Acme): working, its agent is running · 20m\n" +
		"- Docs (w2:t1, Infra): in review, waiting for approval · 25h\n" +
		"- Deploy (w2:t2, Infra): paused until the vendor[31m replies · 2d\n"
	if got := Brief(s, live, now); got != want {
		t.Errorf("brief:\n%s\nwant:\n%s", got, want)
	}
}

func TestBriefOfNone(t *testing.T) {
	got := Brief(&store.State{Tasks: map[string]*task.Task{}}, &herdr.Live{}, time.Now())
	if !strings.HasSuffix(got, "\nThere are none.\n") {
		t.Errorf("brief = %q", got)
	}
}

// Past maxBrief tasks, the brief says how many more there are.
func TestBriefOfMany(t *testing.T) {
	now := time.Now()
	live := &herdr.Live{Workspaces: []herdr.Workspace{{ID: "w1", Label: "Acme"}}}
	s := &store.State{Tasks: map[string]*task.Task{}}
	for i := 1; i <= maxBrief+3; i++ {
		id := fmt.Sprintf("w1:t%d", i)
		live.Tabs = append(live.Tabs, herdr.Tab{ID: id, WorkspaceID: "w1", Number: i})
		s.Tasks[id] = &task.Task{Stage: task.Done, Since: now}
	}
	got := Brief(s, live, now)
	if n := strings.Count(got, "\n- "); n != maxBrief || !strings.HasSuffix(got, "\nAnd 3 more, which `herdr-tasks context` lists.\n") {
		t.Errorf("%d tasks listed:\n%s", n, got)
	}
}

package view

import (
	"fmt"
	"testing"
	"time"

	"github.com/alekkowalczyk/herdr-tasks/internal/herdr"
	"github.com/alekkowalczyk/herdr-tasks/internal/store"
	"github.com/alekkowalczyk/herdr-tasks/internal/task"
)

func TestRowsOrder(t *testing.T) {
	now := time.Now()
	live := &herdr.Live{
		Workspaces: []herdr.Workspace{{ID: "w1", Label: "acme"}, {ID: "w2", Label: "side project"}},
		Tabs: []herdr.Tab{
			{ID: "w1:t1", WorkspaceID: "w1", AgentStatus: "idle"},
			{ID: "w1:t2", WorkspaceID: "w1"},
			{ID: "w1:t3", WorkspaceID: "w1"},
			{ID: "w1:t4", WorkspaceID: "w1", AgentStatus: "idle"},
			{ID: "w2:t1", WorkspaceID: "w2", AgentStatus: "blocked"},
		},
	}
	s := &store.State{
		Hidden: []string{"side project"},
		Tasks: map[string]*task.Task{
			// Made first, but last active most recently.
			"w1:t1": {TabID: "w1:t1", Stage: task.Active, Since: now.Add(-3 * time.Hour), Active: now.Add(-10 * time.Minute)},
			// A tool, newest of all, still comes last.
			"w1:t2": {TabID: "w1:t2", Stage: task.Tool, Since: now},
			// Oldest, but a priority.
			"w1:t3": {TabID: "w1:t3", Stage: task.Active, Since: now.Add(-4 * time.Hour), Priority: true},
			"w1:t4": {TabID: "w1:t4", Stage: task.Active, Since: now.Add(-1 * time.Hour)},
			// Asking would come first, but its space is hidden.
			"w2:t1": {TabID: "w2:t1", Stage: task.Active, Since: now},
		},
	}
	rows := Rows(s, live)
	var got []string
	for _, r := range rows {
		got = append(got, fmt.Sprintf("%s %s", r.Tab.ID, r.Group))
	}
	if fmt.Sprint(got) != "[w1:t3 yourturn w1:t1 yourturn w1:t4 yourturn w1:t2 tool]" {
		t.Fatalf("rows = %v", got)
	}
	if r := rows[2]; r.Pos != 4 || r.Workspace.Label != "acme" || r.Task != s.Tasks["w1:t4"] {
		t.Fatalf("row = %+v", r)
	}
}

func TestRowsSkipTabsWithoutATask(t *testing.T) {
	live := &herdr.Live{
		Workspaces: []herdr.Workspace{{ID: "w1", Label: "acme"}},
		Tabs:       []herdr.Tab{{ID: "w1:t1", WorkspaceID: "w1"}, {ID: "w1:t2", WorkspaceID: "w1"}},
	}
	s := &store.State{Tasks: map[string]*task.Task{"w1:t2": {TabID: "w1:t2", Stage: task.Active}}}
	if rows := Rows(s, live); len(rows) != 1 || rows[0].Tab.ID != "w1:t2" || rows[0].Pos != 2 {
		t.Fatalf("rows = %+v", rows)
	}
}

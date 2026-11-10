// Package view puts tasks in the order the overview lists them, so the
// overview and the board agree.
package view

import (
	"sort"

	"github.com/alekkowalczyk/herdr-tasks/internal/herdr"
	"github.com/alekkowalczyk/herdr-tasks/internal/store"
	"github.com/alekkowalczyk/herdr-tasks/internal/task"
)

// Row is one task with its tab.
type Row struct {
	Tab       herdr.Tab
	Task      *task.Task
	Workspace herdr.Workspace
	// The tab's place in its space's tab bar, from 1.
	Pos   int
	Group task.Group
}

// Rows lists tasks in visible spaces the way the overview does: by group,
// priority first, then the last active first (see task.LastActive). Tools
// are included.
func Rows(s *store.State, live *herdr.Live) []Row {
	var rows []Row
	for _, tab := range live.Tabs {
		t, ok := s.Tasks[tab.ID]
		if !ok {
			continue
		}
		ws, _ := live.Workspace(tab.WorkspaceID)
		if s.IsHidden(ws.Label) {
			continue
		}
		rows = append(rows, Row{Tab: tab, Task: t, Workspace: ws, Pos: live.Position(tab.ID), Group: task.GroupOf(t.Stage, tab.AgentStatus)})
	}
	sort.SliceStable(rows, func(i, j int) bool {
		a, b := rows[i], rows[j]
		if a.Group != b.Group {
			return a.Group.Order() < b.Group.Order()
		}
		if a.Task.Priority != b.Task.Priority {
			return a.Task.Priority
		}
		return a.Task.LastActive().After(b.Task.LastActive())
	})
	return rows
}

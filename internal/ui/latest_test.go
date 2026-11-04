package ui

import (
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/alekkowalczyk/herdr-tasks/internal/claudelog"
	"github.com/alekkowalczyk/herdr-tasks/internal/herdr"
	"github.com/alekkowalczyk/herdr-tasks/internal/store"
	"github.com/alekkowalczyk/herdr-tasks/internal/task"
)

// latestOverview is an overview of four tasks and a tool, with Claude
// replying in all but one: one tab's agent works, and one tab has two
// Claude panes. Its looks read the panes and their logs.
func latestOverview(t *testing.T, st *store.Store) *model {
	t.Helper()
	now := time.Now()
	wss := []herdr.Workspace{{ID: "w1", Label: "Acme", Number: 1}}
	tabs := []herdr.Tab{
		{ID: "w1:t1", WorkspaceID: "w1", Number: 1}, {ID: "w1:t2", WorkspaceID: "w1", Number: 2},
		{ID: "w1:t3", WorkspaceID: "w1", Number: 3}, {ID: "w1:t4", WorkspaceID: "w1", Number: 4},
		{ID: "w1:t5", WorkspaceID: "w1", Number: 5},
	}
	m, err := newModel(Env{Client: herdrOf(t, wss, tabs), Store: st, CurrentTab: "w1:t3"})
	if err != nil {
		t.Fatal(err)
	}
	claude := func(id, tab, status string) herdr.Pane {
		return herdr.Pane{ID: id, TabID: tab, Agent: "claude", AgentStatus: status}
	}
	m.look = func() searchData {
		return searchData{
			panes: []herdr.Pane{
				claude("w1:p1", "w1:t1", "idle"), claude("w1:p2", "w1:t4", "done"), claude("w1:p3", "w1:t4", "idle"),
				claude("w1:p4", "w1:t3", "working"), claude("w1:p5", "w1:t5", "idle"),
				{ID: "w1:p6", TabID: "w1:t2", Agent: "codex", AgentStatus: "idle"},
			},
			sums: map[string]claudelog.Summary{
				"w1:p1": {LastReply: "Opened the PR.", ReplyAt: now.Add(-90 * time.Minute)},
				"w1:p2": {LastReply: "The tests\npass.", ReplyAt: now.Add(-time.Minute)},
				"w1:p3": {LastReply: "Earlier.", ReplyAt: now.Add(-20 * time.Minute)},
				"w1:p4": {LastReply: "Running the migration.", ReplyAt: now.Add(-30 * time.Minute)},
				"w1:p5": {LastReply: "Tailing.", ReplyAt: now},
			},
		}
	}
	m.Update(tea.WindowSizeMsg{Width: 140, Height: 40})
	return m
}

// s lists the tasks by Claude's latest reply: those an agent works on,
// then the rest newest reply first, each with its reply and how long ago it
// came, then those it hasn't replied in, then the tools. ↓ follows that
// order. The next overview keeps it, and s goes back to the stages.
func TestOverviewLatestReplyFirst(t *testing.T) {
	now := time.Now()
	st := &store.Store{Dir: t.TempDir(), Session: "test"}
	err := st.Update(func(s *store.State) error {
		for id, tk := range map[string]*task.Task{
			"w1:t1": {Title: "Old reply", Stage: task.Review, Since: now},
			"w1:t2": {Title: "No Claude", Stage: task.Active, Since: now},
			"w1:t3": {Title: "Busy", Stage: task.Active, Since: now},
			"w1:t4": {Title: "Fresh reply", Stage: task.Parked, Since: now.Add(-48 * time.Hour)},
			"w1:t5": {Title: "Logs", Stage: task.Tool, Since: now},
		} {
			tk.TabID, tk.Workspace = id, "Acme"
			s.Tasks[id] = tk
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	m := latestOverview(t, st)
	if footer := plain(m.footer()); !strings.Contains(footer, "s by reply") {
		t.Errorf("the footer doesn't offer s:\n%s", footer)
	}

	look := m.press("s")
	if _, l := lineWith(listColumn(m), "NO REPLY"); !strings.Contains(l, "reading Claude's logs") {
		t.Errorf("before the logs are read, NO REPLY is %q", l)
	}
	m.run(look)
	lines := listColumn(m)
	at := -1
	for _, s := range []string{"WORKING", "Busy", "LATEST REPLY", "Fresh reply", "Old reply", "NO REPLY", "No Claude", "TOOLS", "Logs"} {
		i, _ := lineWith(lines, s)
		if i <= at {
			t.Fatalf("%q isn't after the line before it:\n%s", s, strings.Join(lines, "\n"))
		}
		at = i
	}
	for name, want := range map[string]string{
		"Fresh reply": " 1m The tests pass.",
		"Old reply":   " 1h Opened the PR.",
		"Busy":        " 30m Running the migration.",
	} {
		if _, l := lineWith(lines, name); !strings.HasSuffix(l, want) {
			t.Errorf("%s's line is %q, want it to end %q", name, l, want)
		}
	}
	if footer := plain(m.footer()); !strings.Contains(footer, "s by stage") {
		t.Errorf("the footer doesn't offer going back:\n%s", footer)
	}

	m.press("down")
	if m.sel != "w1:t4" {
		t.Errorf("↓ from Busy went to %s, want w1:t4", m.sel)
	}
	m.press("down")
	if m.sel != "w1:t1" {
		t.Errorf("↓ from Fresh reply went to %s, want w1:t1", m.sel)
	}

	s, err := st.Load()
	if err != nil || !s.OverviewLatest {
		t.Fatalf("the order wasn't kept: %v", err)
	}
	next := latestOverview(t, st)
	looked := false
	for _, msg := range quickMsgs(next.Init()) {
		if _, ok := msg.(searchLooked); ok {
			looked = true
		}
	}
	if !looked {
		t.Error("the next overview doesn't read the logs")
	}
	if i, _ := lineWith(listColumn(next), "WORKING"); i < 0 {
		t.Error("the next overview lists the tasks by stage")
	}

	m.press("s")
	lines = listColumn(m)
	if i, _ := lineWith(lines, "LATEST REPLY"); i >= 0 {
		t.Errorf("s didn't go back to the stages:\n%s", strings.Join(lines, "\n"))
	}
	if i, _ := lineWith(lines, "AGENT RUNNING"); i < 0 {
		t.Errorf("the stages don't show:\n%s", strings.Join(lines, "\n"))
	}
	if s, _ := st.Load(); s.OverviewLatest {
		t.Error("going back wasn't kept")
	}
}

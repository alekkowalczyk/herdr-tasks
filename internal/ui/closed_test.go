package ui

import (
	"slices"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/alekkowalczyk/herdr-tasks/internal/reopen"
	"github.com/alekkowalczyk/herdr-tasks/internal/store"
	"github.com/alekkowalczyk/herdr-tasks/internal/task"
)

// closedOverview is searchOverview with three closed tasks: Login retries,
// closed an hour ago, Old docs, a day ago, both in Acme, and one in a
// hidden space. Their IDs are returned newest first.
func closedOverview(t *testing.T) (*model, []string) {
	t.Helper()
	looks := 0
	m := searchOverview(t, &looks)
	now := time.Now()
	var ids []string
	err := m.env.Store.Update(func(s *store.State) error {
		s.Hidden = []string{"Archive"}
		s.Close(&task.Task{TabID: "w3:t1", Workspace: "Archive", Title: "Archived login work", Stage: task.Done}, now.Add(-48*time.Hour))
		s.Close(&task.Task{TabID: "w1:t7", Workspace: "Acme", Title: "Old docs", Stage: task.Done, Note: "moved to the wiki"}, now.Add(-24*time.Hour))
		s.Close(&task.Task{TabID: "w1:t8", Workspace: "Acme", Title: "Login retries", Stage: task.Done, Note: "retry after the deploy"}, now.Add(-time.Hour))
		for _, c := range s.Closed {
			ids = append(ids, c.ID)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	m.refresh()
	return m, ids
}

// c lists the closed tasks, newest first, but those in hidden spaces, and
// c again lists the tasks.
func TestOverviewListsClosedTasks(t *testing.T) {
	m, ids := closedOverview(t)
	if _, l := lineWith(listColumn(m), "Login retries"); l != "" {
		t.Fatalf("the task list shows a closed task: %q", l)
	}
	m.press("c")
	lines := listColumn(m)
	head, _ := lineWith(lines, "CLOSED")
	retries, retriesLine := lineWith(lines, "Login retries")
	docs, _ := lineWith(lines, "Old docs")
	if head < 0 || retries < head || docs < retries {
		t.Fatalf("want CLOSED, Login retries, Old docs in order:\n%s", strings.Join(lines, "\n"))
	}
	if !strings.Contains(retriesLine, " 1h") {
		t.Errorf("Login retries' row doesn't say it closed 1h ago: %q", retriesLine)
	}
	for _, s := range []string{"Billing", "Archived login work"} {
		if _, l := lineWith(lines, s); l != "" {
			t.Errorf("the closed list shows %q: %q", s, l)
		}
	}
	if !strings.Contains(lines[0], "2 closed tasks") {
		t.Errorf("header = %q", lines[0])
	}
	if m.sel != "closed:"+ids[0] {
		t.Errorf("selected %q, want the newest closed task", m.sel)
	}
	view := plain(m.View())
	for _, s := range []string{"retry after the deploy", "↵ reopen"} {
		if !strings.Contains(view, s) {
			t.Errorf("the view doesn't show %q:\n%s", s, view)
		}
	}

	// Its tab is closed, so nothing changes it but a reopen.
	m.press("d")
	if !strings.Contains(m.msg, "reopens it") {
		t.Errorf("d on a closed task said %q", m.msg)
	}
	s, _ := m.env.Store.Load()
	if c, _ := s.ClosedTask(ids[0]); c.Task.Stage != task.Done || len(s.Tasks) != 5 {
		t.Errorf("d changed something: %+v", s)
	}

	m.press("c")
	if _, l := lineWith(listColumn(m), "Billing"); l == "" {
		t.Error("c again doesn't list the tasks")
	}
	if m.sel == "closed:"+ids[0] {
		t.Error("the task list keeps a closed task selected")
	}
}

// ↵ on a closed task reopens it, and shows its new tab.
func TestOverviewReopensAClosedTask(t *testing.T) {
	m, ids := closedOverview(t)
	var asked []string
	m.act.reopen = func(id string) (reopen.Reopened, error) {
		asked = append(asked, id)
		return reopen.Reopened{Tab: "w1:t1", Resumed: true}, nil
	}
	m.press("c")
	cmd := m.press("enter")
	if !strings.Contains(m.msg, "Reopening Login retries") {
		t.Errorf("message = %q", m.msg)
	}
	if cmd == nil {
		t.Fatal("enter did nothing")
	}
	_, quit := m.Update(cmd())
	if !slices.Equal(asked, []string{ids[0]}) {
		t.Errorf("reopened %q, want %q", asked, ids[:1])
	}
	if quit == nil {
		t.Fatalf("the overview stays after the reopen: %q", m.msg)
	}
	if _, ok := quit().(tea.QuitMsg); !ok {
		t.Errorf("after the reopen, %T, want quit", quit())
	}

	// One that went wrong says why, and stays.
	m, _ = closedOverview(t)
	m.act.reopen = func(string) (reopen.Reopened, error) { return reopen.Reopened{}, reopen.ErrGone }
	m.press("c")
	_, after := m.Update(m.press("enter")())
	if after != nil || !strings.Contains(m.msg, "isn't kept any more") {
		t.Errorf("a failed reopen: %q, %v", m.msg, after)
	}
}

// A search finds closed tasks too, below the others.
func TestOverviewSearchFindsClosedTasksLast(t *testing.T) {
	m, ids := closedOverview(t)
	m.run(m.press("/"))
	m.press("login")
	lines := listColumn(m)
	var at []int
	for _, s := range []string{"NAME", "Login loop", "CONTENT", "Invoice export", "CLOSED", "Login retries"} {
		i, _ := lineWith(lines, s)
		at = append(at, i)
	}
	if slices.Contains(at, -1) || !slices.IsSorted(at) {
		t.Fatalf("found at lines %v, want the closed task last:\n%s", at, strings.Join(lines, "\n"))
	}
	if _, l := lineWith(lines, "Archived login work"); l != "" {
		t.Errorf("found a closed task in a hidden space: %q", l)
	}
	if !strings.Contains(lines[0], "4 of 5 tabs match · 1 closed") {
		t.Errorf("header = %q", lines[0])
	}
	// What you typed finds it by its note too.
	for range 5 {
		m.press("backspace")
	}
	m.press("deploy")
	if _, l := lineWith(listColumn(m), "Login retries"); !strings.Contains(l, "retry after the deploy") {
		t.Errorf("Login retries' row doesn't show where it was found: %q", l)
	}

	// Chosen, esc shows it among the closed tasks.
	for m.sel != "closed:"+ids[0] {
		m.press("down")
	}
	m.press("esc")
	if !m.showClosed || m.sel != "closed:"+ids[0] {
		t.Errorf("after esc: closed list %v, selected %q", m.showClosed, m.sel)
	}
}

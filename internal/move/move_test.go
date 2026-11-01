package move

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/alekkowalczyk/herdr-tasks/internal/herdr"
	"github.com/alekkowalczyk/herdr-tasks/internal/reconcile"
	"github.com/alekkowalczyk/herdr-tasks/internal/store"
	"github.com/alekkowalczyk/herdr-tasks/internal/task"
)

// fake is an in-memory herdr. A moved pane gets a new ID, as in herdr.
type fake struct {
	workspaces []herdr.Workspace
	tabs       []herdr.Tab
	panes      map[string][]string // tab ID -> pane IDs, in order
	next       int
	failMoveAt int // the Nth move (1-based) fails; 0 never
	moves      int
	closed     []string
	nextTo     [][2]string // tab, target of each MovePaneNextTo
}

func newFake() *fake {
	return &fake{
		workspaces: []herdr.Workspace{{ID: "w1", Label: "infra"}, {ID: "w2", Label: "billing"}},
		tabs:       []herdr.Tab{{ID: "w1:t1", WorkspaceID: "w1", Label: "tab 1"}},
		panes:      map[string][]string{"w1:t1": {"w1:p1", "w1:p2", "w1:p3"}},
	}
}

func (f *fake) Live() (*herdr.Live, error) {
	return &herdr.Live{Workspaces: f.workspaces, Tabs: append([]herdr.Tab(nil), f.tabs...)}, nil
}
func (f *fake) RenameTab(id, label string) error {
	for i := range f.tabs {
		if f.tabs[i].ID == id {
			f.tabs[i].Label = label
		}
	}
	return nil
}
func (f *fake) MoveTab(string, int, int) ([]herdr.Tab, error)          { return f.tabs, nil }
func (f *fake) ReportWorkspaceTokens(string, map[string]*string) error { return nil }

func (f *fake) Layout(tabID string) (*herdr.Node, error) {
	ps := f.panes[tabID]
	if len(ps) == 0 {
		return nil, fmt.Errorf("tab %s has no layout", tabID)
	}
	n := &herdr.Node{Type: "pane", PaneID: ps[0]}
	for _, p := range ps[1:] {
		n = &herdr.Node{Type: "split", First: n, Second: &herdr.Node{Type: "pane", PaneID: p}}
	}
	return n, nil
}

func (f *fake) take(pane string) error {
	f.moves++
	if f.moves == f.failMoveAt {
		return &herdr.ErrNotMoved{Pane: pane, Reason: "refused"}
	}
	for tab, ps := range f.panes {
		for i, p := range ps {
			if p == pane {
				f.panes[tab] = append(append([]string(nil), ps[:i]...), ps[i+1:]...)
				return nil
			}
		}
	}
	return fmt.Errorf("no pane %s", pane)
}

func (f *fake) MovePaneToNewTab(pane, ws, label string) (herdr.Moved, error) {
	if err := f.take(pane); err != nil {
		return herdr.Moved{}, err
	}
	f.next++
	tab := fmt.Sprintf("%s:n%d", ws, f.next)
	np := fmt.Sprintf("%s:np%d", ws, f.next)
	f.tabs = append(f.tabs, herdr.Tab{ID: tab, WorkspaceID: ws, Label: label})
	f.panes[tab] = []string{np}
	return herdr.Moved{PaneID: np, TabID: tab}, nil
}

func (f *fake) MovePaneNextTo(pane, tab, target, split string, ratio float64) (herdr.Moved, error) {
	if split != "right" || ratio != 0.5 {
		return herdr.Moved{}, fmt.Errorf("split %q ratio %v", split, ratio)
	}
	if err := f.take(pane); err != nil {
		return herdr.Moved{}, err
	}
	f.nextTo = append(f.nextTo, [2]string{tab, target})
	f.next++
	np := fmt.Sprintf("%s:np%d", tab, f.next)
	f.panes[tab] = append(f.panes[tab], np)
	return herdr.Moved{PaneID: np, TabID: tab}, nil
}

func (f *fake) Panes() ([]herdr.Pane, error) {
	var out []herdr.Pane
	for _, tab := range f.tabs {
		for _, p := range f.panes[tab.ID] {
			out = append(out, herdr.Pane{ID: p, TabID: tab.ID, WorkspaceID: tab.WorkspaceID})
		}
	}
	return out, nil
}

func (f *fake) CloseTab(id string) error {
	f.closed = append(f.closed, id)
	delete(f.panes, id)
	for i, t := range f.tabs {
		if t.ID == id {
			f.tabs = append(f.tabs[:i], f.tabs[i+1:]...)
			break
		}
	}
	return nil
}

func setup(t *testing.T) (*fake, *store.Store, *task.Task) {
	t.Helper()
	t.Setenv("HERDR_PLUGIN_STATE_DIR", t.TempDir())
	f := newFake()
	st := store.New("test")
	tk := &task.Task{
		TabID: "w1:t1", Workspace: "infra", Title: "Reply to Dana", Stage: task.Review,
		Reason: "an answer", Priority: true, Note: "Dana asked about the invoice",
		Log:            []task.Entry{{Who: "claude", Text: "read the thread"}},
		Links:          []task.Link{{URL: "https://acme.slack.com/archives/C0123ABCD/p1793454730123456"}},
		DismissedLinks: []string{"https://example.test/x"},
		Applied:        "old", StripPane: "w1:p3", StripOff: true,
		Slack: &task.SlackAsk{Key: "C0123ABCD/1793454730.123456"},
	}
	if err := st.Update(func(s *store.State) error {
		s.Tasks = map[string]*task.Task{"w1:t1": tk}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	return f, st, tk
}

var now = time.Date(2026, 10, 31, 12, 0, 0, 0, time.UTC)

func TestMoveCarriesTheTask(t *testing.T) {
	f, st, orig := setup(t)
	want := *orig
	tab, err := Task(f, st, reconcile.Options{}, now, "w1:t1", "billing")
	if err != nil {
		t.Fatal(err)
	}
	s, _ := st.Load()
	got, ok := s.Tasks[tab]
	if !ok {
		t.Fatalf("no task at %s: %+v", tab, s.Tasks)
	}
	if _, old := s.Tasks["w1:t1"]; old || len(s.Tasks) != 1 {
		t.Errorf("tasks = %v, want only the new tab", s.Tasks)
	}
	if got.Workspace != "billing" || got.TabID != tab {
		t.Errorf("workspace %q tab %q, want billing and %s", got.Workspace, got.TabID, tab)
	}
	if got.Slack == nil || got.Slack.Key != want.Slack.Key {
		t.Errorf("Slack = %+v, want it kept", got.Slack)
	}
	if got.StripPane != "" || got.StripOff || got.Applied == "old" {
		t.Errorf("strip/applied not cleared: %+v", got)
	}
	// Everything else is as it was.
	g, w := *got, want
	g.TabID, g.Workspace, g.Applied, g.StripPane, g.StripOff = "", "", "", "", false
	w.TabID, w.Workspace, w.Applied, w.StripPane, w.StripOff = "", "", "", "", false
	if !reflect.DeepEqual(g, w) {
		t.Errorf("task = %+v, want %+v", g, w)
	}
	if len(f.panes[tab]) != 2 {
		t.Errorf("new tab panes = %v, want 2", f.panes[tab])
	}
	// The second pane went next to the first pane's NEW id.
	if len(f.nextTo) != 1 || f.nextTo[0][0] != tab || !strings.Contains(f.nextTo[0][1], ":np") {
		t.Errorf("next-to calls = %v, want the new tab and the first pane's new id", f.nextTo)
	}
	if !reflect.DeepEqual(f.closed, []string{"w1:t1"}) {
		t.Errorf("closed = %v, want the old tab", f.closed)
	}
}

func TestMoveByLabelAnyCase(t *testing.T) {
	f, st, _ := setup(t)
	if _, err := Task(f, st, reconcile.Options{}, now, "w1:t1", "BILLING"); err != nil {
		t.Fatal(err)
	}
	if got := f.tabs[len(f.tabs)-1].WorkspaceID; got != "w2" {
		t.Errorf("new tab in %s, want w2", got)
	}
}

func TestMoveRefuses(t *testing.T) {
	f, st, _ := setup(t)
	if _, err := Task(f, st, reconcile.Options{}, now, "w1:t1", "infra"); err == nil || err.Error() != "It's already in infra" {
		t.Errorf("same workspace: %v", err)
	}
	_, err := Task(f, st, reconcile.Options{}, now, "w1:t1", "nope")
	if err == nil || err.Error() != `No workspace "nope"; there are: infra, billing` {
		t.Errorf("unknown: %v", err)
	}
	f.panes["w1:t1"] = []string{"w1:p3"} // only the strip
	if _, err := Task(f, st, reconcile.Options{}, now, "w1:t1", "billing"); err == nil || err.Error() != "That tab has no panes to move" {
		t.Errorf("only a strip: %v", err)
	}
	if f.moves != 0 || len(f.closed) != 0 {
		t.Errorf("refusals moved %d panes and closed %v", f.moves, f.closed)
	}
}

func TestMovePartialFailureKeepsTheOldTab(t *testing.T) {
	f, st, _ := setup(t)
	f.failMoveAt = 2
	tab, err := Task(f, st, reconcile.Options{}, now, "w1:t1", "billing")
	var nm *herdr.ErrNotMoved
	if err == nil || !strings.HasPrefix(err.Error(), "Moved 1 of 2 panes; the rest are still in the old tab") || !errors.As(err, &nm) {
		t.Fatalf("err = %v, want Moved 1 of 2 wrapping ErrNotMoved", err)
	}
	if len(f.closed) != 0 {
		t.Errorf("closed %v, want the old tab kept", f.closed)
	}
	s, _ := st.Load()
	if tab == "" || s.Tasks[tab] == nil || s.Tasks[tab].Slack == nil {
		t.Errorf("task not re-keyed to %q: %+v", tab, s.Tasks)
	}
}

func TestMoveKeepsAnUntitledTaskUntitled(t *testing.T) {
	f, st, _ := setup(t)
	if err := st.Update(func(s *store.State) error { s.Tasks["w1:t1"].Title = ""; return nil }); err != nil {
		t.Fatal(err)
	}
	tab, err := Task(f, st, reconcile.Options{}, now, "w1:t1", "billing")
	if err != nil {
		t.Fatal(err)
	}
	s, _ := st.Load()
	if got := s.Tasks[tab].Title; got != "" {
		t.Errorf("title = %q, want it still untitled", got)
	}
	// Reconcile labels an untitled tab with its number in the new workspace.
	live, _ := f.Live()
	if tb, _ := live.Tab(tab); tb.Label != "1" {
		t.Errorf("tab label = %q, want \"1\"", tb.Label)
	}
}

// stripFake is a herdr that shows strips, as strip.Host does. A strip runs
// in its tab, so when the strip moves its own task, closing the old tab ends
// the move; CloseTab records what had been done by then.
type stripFake struct {
	*fake
	st     *store.Store
	strips map[string]string // tab ID -> its strip pane
	opened []string          // tabs given a strip
	// At the old tab's close: the moved task as saved, and the strips opened.
	savedAtClose  map[string]*task.Task
	openedAtClose []string
}

func (f *stripFake) Panes() ([]herdr.Pane, error) {
	var out []herdr.Pane
	for _, tab := range f.tabs {
		for _, p := range f.panes[tab.ID] {
			label := ""
			if f.strips[tab.ID] == p {
				label = reconcile.StripLabel
			}
			out = append(out, herdr.Pane{ID: p, TabID: tab.ID, WorkspaceID: tab.WorkspaceID, Label: label})
		}
	}
	return out, nil
}

func (f *stripFake) Open(tab string) (string, error) {
	f.next++
	p := fmt.Sprintf("%s:strip%d", tab, f.next)
	f.panes[tab] = append(f.panes[tab], p)
	f.strips[tab] = p
	f.opened = append(f.opened, tab)
	return p, nil
}

func (f *stripFake) Close(pane string) error           { return fmt.Errorf("closed strip %s", pane) }
func (f *stripFake) Relaunch(string) error             { return nil }
func (f *stripFake) Rename(paneID, label string) error { return nil }

func (f *stripFake) CloseTab(id string) error {
	// A close inside the store's transaction would wait on its lock.
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if s, err := f.st.LoadContext(ctx); err == nil {
		f.savedAtClose = s.Tasks
	}
	f.openedAtClose = append([]string(nil), f.opened...)
	return f.fake.CloseTab(id)
}

func TestMoveSavesAndAddsTheStripBeforeClosingTheOldTab(t *testing.T) {
	base, st, _ := setup(t)
	f := &stripFake{fake: base, st: st, strips: map[string]string{"w1:t1": "w1:p3"}}
	tab, err := Task(f, st, reconcile.Options{Strip: true}, now, "w1:t1", "billing")
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(f.closed, []string{"w1:t1"}) {
		t.Fatalf("closed = %v, want the old tab once", f.closed)
	}
	// When the old tab closed, the task was saved under its new tab...
	got, ok := f.savedAtClose[tab]
	if !ok || got.Workspace != "billing" || got.Slack == nil {
		t.Fatalf("saved at close = %+v, want the task under %s", f.savedAtClose, tab)
	}
	if _, old := f.savedAtClose["w1:t1"]; old || len(f.savedAtClose) != 1 {
		t.Errorf("saved at close = %v, want only the new tab", f.savedAtClose)
	}
	// ...and reconcile had given the new tab its strip.
	if !reflect.DeepEqual(f.openedAtClose, []string{tab}) || got.StripPane == "" || got.StripPane != f.strips[tab] {
		t.Errorf("at close: strips opened %v, task's strip %q; want %s's strip, saved", f.openedAtClose, got.StripPane, tab)
	}
	s, _ := st.Load()
	if len(s.Tasks) != 1 || s.Tasks[tab] == nil {
		t.Errorf("tasks = %v, want only %s", s.Tasks, tab)
	}
}

func TestMoveAfterRemoveStillCloses(t *testing.T) {
	f, st, _ := setup(t)
	if err := st.Update(func(s *store.State) error { s.Removed = true; return nil }); err != nil {
		t.Fatal(err)
	}
	if _, err := Task(f, st, reconcile.Options{}, now, "w1:t1", "billing"); err != nil {
		t.Fatalf("err = %v, want none, as before a remove", err)
	}
	if !reflect.DeepEqual(f.closed, []string{"w1:t1"}) {
		t.Errorf("closed = %v, want the old tab", f.closed)
	}
}

// A move keeps each pane's old ID, so what knows a pane by it, like Claude's
// hook, still finds it.
func TestMoveKeepsThePanesOldIDs(t *testing.T) {
	f, st, _ := setup(t)
	// w0:p7 became w1:p1 in an earlier move; w0:p8's pane is gone.
	if err := st.Update(func(s *store.State) error {
		s.PaneAliases = map[string]string{"w0:p7": "w1:p1", "w0:p8": "w0:gone"}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	tab, err := Task(f, st, reconcile.Options{}, now, "w1:t1", "billing")
	if err != nil {
		t.Fatal(err)
	}
	moved := f.panes[tab]
	if len(moved) != 2 {
		t.Fatalf("new tab panes = %v, want 2", moved)
	}
	s, _ := st.Load()
	want := map[string]string{"w1:p1": moved[0], "w1:p2": moved[1], "w0:p7": moved[0]}
	if !reflect.DeepEqual(s.PaneAliases, want) {
		t.Errorf("aliases = %v, want %v", s.PaneAliases, want)
	}
}

func TestMoveKeepsAtMost200PaneIDs(t *testing.T) {
	f, st, _ := setup(t)
	f.tabs = append(f.tabs, herdr.Tab{ID: "w2:t9", WorkspaceID: "w2", Label: "tab 9"})
	aliases := map[string]string{}
	for i := range 250 {
		p := fmt.Sprintf("w2:q%d", i)
		f.panes["w2:t9"] = append(f.panes["w2:t9"], p)
		aliases[fmt.Sprintf("w0:a%d", i)] = p
	}
	if err := st.Update(func(s *store.State) error { s.PaneAliases = aliases; return nil }); err != nil {
		t.Fatal(err)
	}
	tab, err := Task(f, st, reconcile.Options{}, now, "w1:t1", "billing")
	if err != nil {
		t.Fatal(err)
	}
	s, _ := st.Load()
	if len(s.PaneAliases) != 200 {
		t.Errorf("%d aliases, want 200", len(s.PaneAliases))
	}
	if s.PaneAliases["w1:p1"] != f.panes[tab][0] || s.PaneAliases["w1:p2"] != f.panes[tab][1] {
		t.Errorf("w1:p1 -> %q, w1:p2 -> %q; want this move's panes kept, %v", s.PaneAliases["w1:p1"], s.PaneAliases["w1:p2"], f.panes[tab])
	}
}

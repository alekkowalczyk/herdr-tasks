package reconcile

import (
	"fmt"
	"testing"
	"time"

	"github.com/alekkowalczyk/herdr-tasks/internal/herdr"
	"github.com/alekkowalczyk/herdr-tasks/internal/store"
	"github.com/alekkowalczyk/herdr-tasks/internal/task"
)

// fakeHerdr behaves like herdr for tab renames, moves, and sidebar tokens.
type fakeHerdr struct {
	ws      []herdr.Workspace
	tabs    []herdr.Tab
	renames int
	moves   int
	tokens  map[string]map[string]*string
}

func newFake() *fakeHerdr {
	return &fakeHerdr{
		ws: []herdr.Workspace{{ID: "w1", Label: "jam sql", Focused: true}, {ID: "w2", Label: "munu MWP"}},
		tabs: []herdr.Tab{
			{ID: "w1:t1", WorkspaceID: "w1", Label: "1"},
			{ID: "w1:t2", WorkspaceID: "w1", Label: "arm CI runners"},
			{ID: "w1:t3", WorkspaceID: "w1", Label: "3"},
			{ID: "w2:t1", WorkspaceID: "w2", Label: "checkout"},
		},
		tokens: map[string]map[string]*string{},
	}
}

func (f *fakeHerdr) Live() (*herdr.Live, error) {
	return &herdr.Live{
		Workspaces: append([]herdr.Workspace(nil), f.ws...),
		Tabs:       append([]herdr.Tab(nil), f.tabs...),
	}, nil
}

func (f *fakeHerdr) RenameTab(id, label string) error {
	for i := range f.tabs {
		if f.tabs[i].ID == id {
			f.tabs[i].Label = label
			f.renames++
			return nil
		}
	}
	return fmt.Errorf("no tab %s", id)
}

// MoveTab follows herdr: insert_index counts positions before removal.
func (f *fakeHerdr) MoveTab(id string, from, to int) ([]herdr.Tab, error) {
	var ws string
	for _, t := range f.tabs {
		if t.ID == id {
			ws = t.WorkspaceID
		}
	}
	var in, out []herdr.Tab
	for _, t := range f.tabs {
		if t.WorkspaceID == ws {
			in = append(in, t)
		} else {
			out = append(out, t)
		}
	}
	if in[from].ID != id {
		return nil, fmt.Errorf("tab %s isn't at %d", id, from)
	}
	moving := in[from]
	rest := append(append([]herdr.Tab(nil), in[:from]...), in[from+1:]...)
	in = append(rest[:to:to], append([]herdr.Tab{moving}, rest[to:]...)...)
	f.tabs = append(out, in...)
	f.moves++
	// Return in workspace order like herdr does.
	var all []herdr.Tab
	for _, w := range f.ws {
		for _, t := range f.tabs {
			if t.WorkspaceID == w.ID {
				all = append(all, t)
			}
		}
	}
	f.tabs = all
	return in, nil
}

func (f *fakeHerdr) ReportWorkspaceTokens(ws string, tokens map[string]*string) error {
	f.tokens[ws] = tokens
	return nil
}

func (f *fakeHerdr) labels(ws string) []string {
	var out []string
	for _, t := range f.tabs {
		if t.WorkspaceID == ws {
			out = append(out, t.Label)
		}
	}
	return out
}

func newStore(t *testing.T) *store.Store {
	return &store.Store{Dir: t.TempDir(), Session: "test"}
}

var on = Options{Markers: true, SortTabs: true}

func setStage(id string, s task.Stage) Mutate {
	return func(st *store.State, _ *herdr.Live) error {
		st.Tasks[id].SetStage(s, "", time.Now())
		return nil
	}
}

func TestFirstRunTracksAndMarksEveryTab(t *testing.T) {
	f, st := newFake(), newStore(t)
	if _, err := Run(f, st, on, time.Now(), nil); err != nil {
		t.Fatal(err)
	}
	if got := fmt.Sprint(f.labels("w1")); got != "[● 1 ● arm CI runners ● 3]" {
		t.Fatalf("labels = %s", got)
	}
	s, _ := st.Load()
	if len(s.Tasks) != 4 || s.Tasks["w1:t1"].Title != "" || s.Tasks["w1:t2"].Title != "arm CI runners" {
		t.Fatalf("tasks = %+v", s.Tasks)
	}
	if v := f.tokens["w1"]["tasks"]; v == nil || *v != "● 3" {
		t.Fatalf("tasks token = %v", v)
	}
}

func TestStagesReorderTabsAndRenumberUnnamedOnes(t *testing.T) {
	f, st := newFake(), newStore(t)
	Run(f, st, on, time.Now(), nil)
	if _, err := Run(f, st, on, time.Now(), setStage("w1:t1", task.Parked)); err != nil {
		t.Fatal(err)
	}
	if _, err := Run(f, st, on, time.Now(), setStage("w1:t2", task.Paused)); err != nil {
		t.Fatal(err)
	}
	// Unnamed tabs keep showing their current position.
	if got := fmt.Sprint(f.labels("w1")); got != "[● 1 ‖ arm CI runners ○ 3]" {
		t.Fatalf("labels = %s", got)
	}
	var order []string
	for _, tab := range f.tabs {
		if tab.WorkspaceID == "w1" {
			order = append(order, tab.ID)
		}
	}
	if fmt.Sprint(order) != "[w1:t3 w1:t2 w1:t1]" {
		t.Fatalf("order = %v", order)
	}
	tok := f.tokens["w1"]
	if *tok["tasks"] != "● 1 · ‖ 1 · ○ 1" || tok["tasks_done"] != nil {
		t.Fatalf("tokens = %v / %v", *tok["tasks"], tok["tasks_done"])
	}
}

func TestSecondRunChangesNothing(t *testing.T) {
	f, st := newFake(), newStore(t)
	Run(f, st, on, time.Now(), setStage("w1:t1", task.Done))
	Run(f, st, on, time.Now(), nil)
	r, m := f.renames, f.moves
	if _, err := Run(f, st, on, time.Now(), nil); err != nil {
		t.Fatal(err)
	}
	if f.renames != r || f.moves != m {
		t.Fatalf("idle run renamed %d and moved %d tabs", f.renames-r, f.moves-m)
	}
}

func TestYourRenameBecomesTheTitle(t *testing.T) {
	f, st := newFake(), newStore(t)
	Run(f, st, on, time.Now(), setStage("w2:t1", task.Paused))
	f.RenameTab("w2:t1", "checkout refactor")
	Run(f, st, on, time.Now(), nil)
	if got := f.labels("w2")[0]; got != "‖ checkout refactor" {
		t.Fatalf("label = %q", got)
	}
	s, _ := st.Load()
	if s.Tasks["w2:t1"].Title != "checkout refactor" || s.Tasks["w2:t1"].Stage != task.Paused {
		t.Fatalf("task = %+v", s.Tasks["w2:t1"])
	}
}

func TestClosedTabsAreDroppedAndNewTabsTracked(t *testing.T) {
	f, st := newFake(), newStore(t)
	Run(f, st, on, time.Now(), nil)
	f.tabs = f.tabs[1:]
	f.tabs = append(f.tabs, herdr.Tab{ID: "w2:t2", WorkspaceID: "w2", Label: "2"})
	Run(f, st, on, time.Now(), nil)
	s, _ := st.Load()
	if _, ok := s.Tasks["w1:t1"]; ok {
		t.Fatal("closed tab still tracked")
	}
	if tk, ok := s.Tasks["w2:t2"]; !ok || tk.Stage != task.Active {
		t.Fatalf("new tab = %+v", tk)
	}
}

func TestTaskFollowsANamedTabToANewID(t *testing.T) {
	f, st := newFake(), newStore(t)
	Run(f, st, on, time.Now(), func(s *store.State, _ *herdr.Live) error {
		s.Tasks["w1:t2"].Note = "keep me"
		s.Tasks["w1:t2"].SetStage(task.Paused, "review", time.Now())
		return nil
	})
	// After a restart herdr hands out a new ID; the label still carries the marker.
	for i := range f.tabs {
		if f.tabs[i].ID == "w1:t2" {
			f.tabs[i].ID = "w1:t9"
		}
	}
	Run(f, st, on, time.Now(), nil)
	s, _ := st.Load()
	tk, ok := s.Tasks["w1:t9"]
	if !ok || tk.Note != "keep me" || tk.Stage != task.Paused {
		t.Fatalf("task didn't follow the tab: %+v", tk)
	}
}

func TestMarkersOffPutsNamesBack(t *testing.T) {
	f, st := newFake(), newStore(t)
	Run(f, st, on, time.Now(), nil)
	Run(f, st, Options{Markers: false, SortTabs: true}, time.Now(), nil)
	if got := fmt.Sprint(f.labels("w1")); got != "[1 arm CI runners 3]" {
		t.Fatalf("labels = %s", got)
	}
	r := f.renames
	Run(f, st, Options{Markers: false, SortTabs: true}, time.Now(), nil)
	if f.renames != r {
		t.Fatal("markers off should leave tabs alone once names are back")
	}
}

func TestTokens(t *testing.T) {
	tok := Tokens([]task.Stage{task.Active, task.Active, task.Done})
	if *tok["tasks"] != "● 2" || *tok["tasks_active"] != "● 2" || *tok["tasks_done"] != "✓ 1" || tok["tasks_paused"] != nil {
		t.Fatalf("tokens = %v", tok)
	}
	if tok := Tokens(nil); tok["tasks"] != nil {
		t.Fatal("empty space should clear $tasks")
	}
}

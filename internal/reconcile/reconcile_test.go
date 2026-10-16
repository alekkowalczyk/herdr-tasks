package reconcile

import (
	"fmt"
	"strings"
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

// ReportWorkspaceTokens patches tokens like herdr: nil clears a key.
func (f *fakeHerdr) ReportWorkspaceTokens(ws string, tokens map[string]*string) error {
	if len(tokens) > 16 {
		return fmt.Errorf("%d tokens in one report", len(tokens))
	}
	if f.tokens[ws] == nil {
		f.tokens[ws] = map[string]*string{}
	}
	for k, v := range tokens {
		f.tokens[ws][k] = v
	}
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

// fakeStrips adds panes and strips to fakeHerdr.
type fakeStrips struct {
	*fakeHerdr
	panes []herdr.Pane
	next  int
}

func newFakeStrips() *fakeStrips {
	f := &fakeStrips{fakeHerdr: newFake()}
	for _, t := range f.tabs {
		f.add(t.ID, "")
	}
	return f
}

func (f *fakeStrips) add(tabID, label string) string {
	f.next++
	id := fmt.Sprintf("%s:p%d", tabID[:2], f.next)
	f.panes = append(f.panes, herdr.Pane{ID: id, TabID: tabID, Label: label})
	return id
}

func (f *fakeStrips) Panes() ([]herdr.Pane, error) { return append([]herdr.Pane(nil), f.panes...), nil }
func (f *fakeStrips) Open(tabID string) (string, error) {
	return f.add(tabID, StripLabel), nil
}
func (f *fakeStrips) Close(paneID string) error {
	for i, p := range f.panes {
		if p.ID == paneID {
			f.panes = append(f.panes[:i], f.panes[i+1:]...)
			return nil
		}
	}
	return fmt.Errorf("no pane %s", paneID)
}
func (f *fakeStrips) CloseTab(tabID string) error {
	var keep []herdr.Pane
	for _, p := range f.panes {
		if p.TabID != tabID {
			keep = append(keep, p)
		}
	}
	f.panes = keep
	for i, t := range f.tabs {
		if t.ID == tabID {
			f.tabs = append(f.tabs[:i], f.tabs[i+1:]...)
			break
		}
	}
	return nil
}
func (f *fakeStrips) Relaunch(string) error { return nil }
func (f *fakeStrips) Rename(paneID, label string) error {
	for i, p := range f.panes {
		if p.ID == paneID {
			f.panes[i].Label = label
			return nil
		}
	}
	return fmt.Errorf("no pane %s", paneID)
}

func (f *fakeStrips) strips() map[string]bool {
	out := map[string]bool{}
	for _, p := range f.panes {
		if p.Label == StripLabel {
			out[p.TabID] = true
		}
	}
	return out
}

var withStrips = Options{Markers: true, SortTabs: true, Strip: true}

func TestStripsFromBeforeTheRenameAreKeptAndRenamed(t *testing.T) {
	f, st := newFakeStrips(), newStore(t)
	old := f.add("w1:t2", "Task")
	if _, err := Run(f, st, withStrips, time.Now(), nil); err != nil {
		t.Fatal(err)
	}
	n := 0
	for _, p := range f.panes {
		if p.TabID == "w1:t2" && (p.Label == "Task" || p.Label == StripLabel) {
			n++
		}
	}
	s, _ := st.Load()
	if n != 1 || s.Tasks["w1:t2"].StripPane != old {
		t.Fatalf("the old strip wasn't kept: %d strips, state has %q", n, s.Tasks["w1:t2"].StripPane)
	}
	for _, p := range f.panes {
		if p.ID == old && p.Label != "Task strip" {
			t.Fatalf("old strip is still labeled %q", p.Label)
		}
	}
}

func TestEveryTaskGetsAStripButToolsDont(t *testing.T) {
	f, st := newFakeStrips(), newStore(t)
	if _, err := Run(f, st, withStrips, time.Now(), nil); err != nil {
		t.Fatal(err)
	}
	if len(f.strips()) != 4 {
		t.Fatalf("strips = %v", f.strips())
	}
	// Marking a tab as a tool takes its strip away and moves it last.
	Run(f, st, withStrips, time.Now(), setStage("w1:t1", task.Tool))
	if f.strips()["w1:t1"] {
		t.Fatal("a tool tab kept its strip")
	}
	if got := f.labels("w1"); got[len(got)-1] != "◇ 3" {
		t.Fatalf("labels = %v", got)
	}
	if tok := f.tokens["w1"]; *tok["tasks"] != "● 2" || tok["tasks_tool"] != nil {
		t.Fatalf("tool counted as work: %v", *tok["tasks"])
	}
	// You can still turn a strip on for a tool.
	Run(f, st, withStrips, time.Now(), func(s *store.State, _ *herdr.Live) error {
		s.Tasks["w1:t1"].SetStrip(true)
		return nil
	})
	if !f.strips()["w1:t1"] {
		t.Fatal("strip didn't come back on a tool you turned it on for")
	}
}

func TestAStripYouCloseStaysClosed(t *testing.T) {
	f, st := newFakeStrips(), newStore(t)
	Run(f, st, withStrips, time.Now(), nil)
	s, _ := st.Load()
	f.Close(s.Tasks["w2:t1"].StripPane)
	Run(f, st, withStrips, time.Now(), nil)
	if f.strips()["w2:t1"] {
		t.Fatal("strip came back after you closed it")
	}
}

func TestATabWithOnlyItsStripLeftCloses(t *testing.T) {
	f, st := newFakeStrips(), newStore(t)
	Run(f, st, withStrips, time.Now(), nil)
	var keep []herdr.Pane
	for _, p := range f.panes {
		if p.TabID != "w2:t1" || p.Label == StripLabel {
			keep = append(keep, p)
		}
	}
	f.panes = keep
	Run(f, st, withStrips, time.Now(), nil)
	for _, tab := range f.tabs {
		if tab.ID == "w2:t1" {
			t.Fatal("tab with only its strip left is still open")
		}
	}
}

func val(m map[string]*string, k string) string {
	if v := m[k]; v != nil {
		return *v
	}
	return ""
}

func TestSidebarListsTasksMostUrgentFirst(t *testing.T) {
	f, st := newFake(), newStore(t)
	f.tabs = append(f.tabs,
		herdr.Tab{ID: "w1:t4", WorkspaceID: "w1", Label: "refund webhooks", AgentStatus: "working"},
		herdr.Tab{ID: "w1:t5", WorkspaceID: "w1", Label: "date picker", AgentStatus: "blocked"},
		herdr.Tab{ID: "w1:t6", WorkspaceID: "w1", Label: "release notes", AgentStatus: "done"},
		herdr.Tab{ID: "w1:t7", WorkspaceID: "w1", Label: "lazygit"},
	)
	Run(f, st, on, time.Now(), func(s *store.State, _ *herdr.Live) error {
		now := time.Now()
		s.Tasks["w1:t1"].SetStage(task.Parked, "", now)
		s.Tasks["w1:t2"].SetStage(task.Paused, "PR review", now)
		s.Tasks["w1:t3"].SetStage(task.Review, "QA", now)
		s.Tasks["w1:t7"].SetStage(task.Tool, "", now)
		return nil
	})
	f.tabs = append(f.tabs, herdr.Tab{ID: "w1:t8", WorkspaceID: "w1", Label: "backups"})
	Run(f, st, on, time.Now(), setStage("w1:t8", task.Done))
	tok := f.tokens["w1"]
	got := []string{val(tok, "t1_ask"), val(tok, "t2_you"), val(tok, "t3_run"), val(tok, "t4")}
	want := []string{"? date picker", "● release notes", "↻ refund webhooks", "◐ tab 4"}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("rows = %q, want %q", got, want)
	}
	// Under the list, counts name their stage so "○ 1" can't pass for a tab named 1.
	if val(tok, "t5") != "" || val(tok, "tasks_paused") != "1 paused" || val(tok, "tasks_parked") != "1 parked" || val(tok, "tasks_done") != "1 done" || val(tok, "tasks_active") != "" {
		t.Fatalf("hidden stages should only show as counts: %v", tok)
	}

	// Switching to counts clears the rows and brings back every count.
	Run(f, st, on, time.Now(), func(s *store.State, _ *herdr.Live) error { s.SidebarCounts = true; return nil })
	tok = f.tokens["w1"]
	if val(tok, "t1_ask") != "" || val(tok, "tasks_active") != "● 3" || val(tok, "tasks_review") != "◐ 1" || val(tok, "tasks_parked") != "○ 1" {
		t.Fatalf("counts mode: %v", tok)
	}
}

func TestSidebarListsPriorityTasksFirstWhateverTheirStage(t *testing.T) {
	f, st := newFake(), newStore(t)
	f.tabs = append(f.tabs,
		herdr.Tab{ID: "w2:t2", WorkspaceID: "w2", Label: "legal review"},
		herdr.Tab{ID: "w2:t3", WorkspaceID: "w2", Label: "fix login", AgentStatus: "blocked"},
		herdr.Tab{ID: "w2:t4", WorkspaceID: "w2", Label: "old spike"},
		herdr.Tab{ID: "w2:t5", WorkspaceID: "w2", Label: "hotfix", AgentStatus: "blocked"},
		herdr.Tab{ID: "w2:t6", WorkspaceID: "w2", Label: "deploy", AgentStatus: "working"},
	)
	Run(f, st, on, time.Now(), func(s *store.State, _ *herdr.Live) error {
		now := time.Now()
		s.Tasks["w2:t1"].SetStage(task.Parked, "", now)
		s.Tasks["w2:t2"].SetStage(task.Paused, "legal", now)
		s.Tasks["w2:t4"].SetStage(task.Parked, "", now)
		for _, id := range []string{"w2:t1", "w2:t2", "w2:t5", "w2:t6"} {
			if err := s.Tasks[id].SetPriority(true); err != nil {
				return err
			}
		}
		return nil
	})
	tok := f.tokens["w2"]
	// herdr puts " · " between two tokens, so the ! shares its row's token:
	// $tN_pri (red) for a priority row, $tN_ask when its agent is asking.
	row := func(i int) string {
		var parts []string
		for _, k := range []string{"_pri", "_ask", "_you", ""} {
			if v := val(tok, fmt.Sprintf("t%d%s", i, k)); v != "" {
				parts = append(parts, k+"="+v)
			}
		}
		return strings.Join(parts, " ")
	}
	// A working agent gets ↻, since a priority row's color can't show it.
	got := []string{row(1), row(2), row(3), row(4), row(5), row(6)}
	want := []string{
		"_ask=! ? hotfix",
		"_pri=! ↻ deploy",
		"_pri=! ‖ legal review",
		"_pri=! ○ checkout",
		"_ask=? fix login",
		"",
	}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("rows = %q, want %q", got, want)
	}
	// Listed tasks aren't counted again under the list.
	if val(tok, "tasks_parked") != "1 parked" || val(tok, "tasks_paused") != "" {
		t.Fatalf("counts: parked %q, paused %q", val(tok, "tasks_parked"), val(tok, "tasks_paused"))
	}
}

func TestSidebarListOverflow(t *testing.T) {
	f, st := newFake(), newStore(t)
	for i := 4; i <= 12; i++ {
		f.tabs = append(f.tabs, herdr.Tab{ID: fmt.Sprintf("w1:t%d", i), WorkspaceID: "w1", Label: fmt.Sprintf("task %d", i)})
	}
	Run(f, st, on, time.Now(), nil)
	if got := val(f.tokens["w1"], "t7"); got != "+6 more" {
		t.Fatalf("last row = %q", got)
	}
}

func TestAStripThatMakesItsTabAToolSavesFirst(t *testing.T) {
	f, st := newFakeStrips(), newStore(t)
	Run(f, st, withStrips, time.Now(), nil)
	s, _ := st.Load()
	self := s.Tasks["w2:t1"].StripPane

	// The strip saves "tool" for its own tab: the save must not close it.
	opt := withStrips
	opt.Self = self
	if _, err := Run(f, st, opt, time.Now(), setStage("w2:t1", task.Tool)); err != nil {
		t.Fatal(err)
	}
	if !f.strips()["w2:t1"] {
		t.Fatal("the saving strip was closed under itself")
	}
	if s, _ := st.Load(); s.Tasks["w2:t1"].Stage != task.Tool {
		t.Fatal("stage not saved")
	}

	// Then it exits; that isn't you hiding it.
	f.Close(self)
	Run(f, st, withStrips, time.Now(), nil)
	if s, _ := st.Load(); s.Tasks["w2:t1"].StripOff || s.Tasks["w2:t1"].StripPane != "" {
		t.Fatalf("a strip closing for a tool counted as hidden: %+v", s.Tasks["w2:t1"])
	}

	// Back to active, the strip returns.
	Run(f, st, withStrips, time.Now(), setStage("w2:t1", task.Active))
	if !f.strips()["w2:t1"] {
		t.Fatal("strip didn't come back when the tool became a task again")
	}
}

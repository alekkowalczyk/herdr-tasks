package ui

import (
	"slices"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"

	"github.com/alekkowalczyk/herdr-tasks/internal/claudelog"
	"github.com/alekkowalczyk/herdr-tasks/internal/herdr"
	"github.com/alekkowalczyk/herdr-tasks/internal/store"
	"github.com/alekkowalczyk/herdr-tasks/internal/task"
	"github.com/alekkowalczyk/herdr-tasks/internal/view"
)

// searchOverview is an overview of five tasks in two spaces, four of which
// have "login" somewhere: one in its name, one in a pane's name, one in a
// recap and one in its note. Its search reads the panes and the recap from
// looks, which counts how often it's asked.
func searchOverview(t *testing.T, looks *int) *model {
	t.Helper()
	now := time.Now()
	wss := []herdr.Workspace{{ID: "w1", Label: "Acme", Number: 1}, {ID: "w2", Label: "Infra", Number: 2}}
	tabs := []herdr.Tab{
		{ID: "w1:t1", WorkspaceID: "w1", Number: 1}, {ID: "w1:t2", WorkspaceID: "w1", Number: 2},
		{ID: "w1:t5", WorkspaceID: "w1", Number: 3},
		{ID: "w2:t3", WorkspaceID: "w2", Number: 1}, {ID: "w2:t4", WorkspaceID: "w2", Number: 2},
	}
	tasks := map[string]*task.Task{
		"w1:t1": {Workspace: "Acme", Title: "Invoice export", Stage: task.Active, Since: now, Note: "Blocked on the\nlogin provider's sandbox"},
		"w1:t2": {Workspace: "Acme", Title: "Login loop", Stage: task.Paused, Since: now.Add(-time.Hour)},
		"w1:t5": {Workspace: "Acme", Title: "Billing", Stage: task.Active, Since: now.Add(-2 * time.Hour)},
		"w2:t3": {Workspace: "Infra", Title: "Deploy", Stage: task.Active, Since: now.Add(-3 * time.Hour)},
		"w2:t4": {Workspace: "Infra", Title: "Docs", Stage: task.Review, Since: now.Add(-4 * time.Hour)},
	}
	st := &store.Store{Dir: t.TempDir(), Session: "test"}
	if err := st.Update(func(s *store.State) error {
		for id, tk := range tasks {
			tk.TabID = id
			s.Tasks[id] = tk
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	m, err := newModel(Env{Client: herdrOf(t, wss, tabs), Store: st, CurrentTab: "w1:t1"})
	if err != nil {
		t.Fatal(err)
	}
	m.look = func() searchData {
		*looks++
		return searchData{
			panes: []herdr.Pane{
				{ID: "w2:p1", TabID: "w2:t3", Label: "login server"},
				{ID: "w2:p2", TabID: "w2:t4", Agent: "claude"},
				{ID: "w1:p3", TabID: "w1:t5", Label: "Task strip"},
			},
			sums: map[string]claudelog.Summary{"w2:p2": {Recap: "You're documenting the login flow for new users."}},
		}
	}
	m.Update(tea.WindowSizeMsg{Width: 140, Height: 40})
	return m
}

func (m *model) press(k string) tea.Cmd {
	msg := tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(k)}
	switch k {
	case "esc":
		msg = tea.KeyMsg{Type: tea.KeyEsc}
	case "down":
		msg = tea.KeyMsg{Type: tea.KeyDown}
	case "backspace":
		msg = tea.KeyMsg{Type: tea.KeyBackspace}
	}
	_, cmd := m.Update(msg)
	return cmd
}

// run runs cmd, and hands the overview what it said.
func (m *model) run(cmd tea.Cmd) {
	if cmd != nil {
		m.Update(cmd())
	}
}

// quickMsgs runs cmd, or each command of a batch, and returns what they say
// within 200ms: the refresh's tick waits longer.
func quickMsgs(cmd tea.Cmd) []tea.Msg {
	if cmd == nil {
		return nil
	}
	cmds := []tea.Cmd{cmd}
	if b, ok := cmd().(tea.BatchMsg); ok {
		cmds = b
	} else {
		return []tea.Msg{cmd()}
	}
	got := make(chan tea.Msg, len(cmds))
	for _, c := range cmds {
		go func() { got <- c() }()
	}
	var out []tea.Msg
	timeout := time.After(200 * time.Millisecond)
	for range cmds {
		select {
		case msg := <-got:
			out = append(out, msg)
		case <-timeout:
			return out
		}
	}
	return out
}

// listColumn is the list part of each line of the overview, as you'd read it.
func listColumn(m *model) []string {
	var out []string
	for _, l := range strings.Split(m.View(), "\n") {
		l = ansi.Strip(l)
		if i := strings.Index(l, "│"); i >= 0 {
			l = l[:i]
		}
		out = append(out, strings.Join(strings.Fields(l), " "))
	}
	return out
}

// lineWith is the first line of lines holding s, or "".
func lineWith(lines []string, s string) (int, string) {
	i := slices.IndexFunc(lines, func(l string) bool { return strings.Contains(l, s) })
	if i < 0 {
		return -1, ""
	}
	return i, lines[i]
}

// / searches the tasks: first those with the words in their name, then in
// a pane's name, then in a recap, then anywhere else. Each says where it
// was found, and esc goes back to the list, on the task you chose.
func TestOverviewSearch(t *testing.T) {
	looks := 0
	m := searchOverview(t, &looks)
	if _, l := lineWith(listColumn(m), "Billing"); l == "" {
		t.Fatal("the list doesn't show Billing before a search")
	}
	if !strings.Contains(plain(m.View()), "/ search") {
		t.Error("the keys don't list / search")
	}

	m.run(m.press("/"))
	if m.mode != modeSearch {
		t.Fatalf("mode = %v after /, want the search", m.mode)
	}
	if looks != 1 {
		t.Errorf("the search read herdr's panes %d times, want once", looks)
	}
	m.press("login")
	lines := listColumn(m)
	var at []int
	for _, s := range []string{"NAME", "Login loop", "PANE", "Deploy", "RECAP", "Docs", "CONTENT", "Invoice export"} {
		i, _ := lineWith(lines, s)
		at = append(at, i)
	}
	if slices.Contains(at, -1) || !slices.IsSorted(at) {
		t.Errorf("found at lines %v, want name, pane, recap, content in order:\n%s", at, strings.Join(lines, "\n"))
	}
	if _, l := lineWith(lines, "Billing"); l != "" {
		t.Errorf("Billing was found: %q", l)
	}
	for task, text := range map[string]string{"Deploy": "login server", "Docs": "the login flow", "Invoice export": "the login provider"} {
		if _, l := lineWith(lines, task); !strings.Contains(l, text) {
			t.Errorf("%s's row doesn't show where it was found, %q: %q", task, text, l)
		}
	}
	if !strings.Contains(lines[0], "4 of 5 tabs match") {
		t.Errorf("header = %q, want 4 of 5 tabs match", lines[0])
	}
	if m.sel != "w1:t2" {
		t.Errorf("selected %s, want the first found, w1:t2", m.sel)
	}
	if !strings.Contains(plain(m.View()), "/ login") {
		t.Error("the footer doesn't show what you typed")
	}

	m.press("down")
	if m.sel != "w2:t3" {
		t.Errorf("down selected %s, want the next found, w2:t3", m.sel)
	}
	// A refresh reads the panes again, and keeps the search.
	_, cmd := m.Update(refreshMsg{})
	for _, msg := range quickMsgs(cmd) {
		m.Update(msg)
	}
	if looks != 2 {
		t.Errorf("the refresh read herdr's panes %d times in all, want twice", looks)
	}
	if m.mode != modeSearch || m.sel != "w2:t3" {
		t.Errorf("after a refresh: mode %v, selected %s", m.mode, m.sel)
	}

	m.press("esc")
	if m.mode != modeList || m.sel != "w2:t3" {
		t.Errorf("after esc: mode %v, selected %s, want the list on w2:t3", m.mode, m.sel)
	}
	if _, l := lineWith(listColumn(m), "Billing"); l == "" {
		t.Error("esc didn't bring the whole list back")
	}

	m.press("/")
	m.press("zzz")
	if _, l := lineWith(listColumn(m), "No task matches zzz"); l == "" {
		t.Errorf("a search that finds nothing doesn't say so:\n%s", strings.Join(listColumn(m), "\n"))
	}
	if m.sel != "w2:t3" {
		t.Errorf("a search that finds nothing moved the selection to %s", m.sel)
	}
	for range 3 {
		m.press("backspace")
	}
	if _, l := lineWith(listColumn(m), "Billing"); l == "" {
		t.Error("an empty search doesn't show the whole list")
	}
}

// A click on a row a search found selects it.
func TestOverviewSearchClick(t *testing.T) {
	looks := 0
	m := searchOverview(t, &looks)
	m.run(m.press("/"))
	m.press("login")
	m.View()
	y := -1
	for at, id := range m.rowAt {
		if id == "w2:t4" {
			y = at
		}
	}
	if y < 0 {
		t.Fatalf("Docs has no row to click: %v", m.rowAt)
	}
	m.Update(tea.MouseMsg{X: 5, Y: y, Action: tea.MouseActionPress, Button: tea.MouseButtonLeft})
	if m.sel != "w2:t4" || m.mode != modeSearch {
		t.Errorf("after the click: selected %s, mode %v", m.sel, m.mode)
	}
}

// A row shows where a search found its task from a little before the word
// that found it, starting at a word.
func TestFoundCellShowsTheWord(t *testing.T) {
	h := view.Search(view.Words("login"), view.Fields{Recaps: []string{"You're documenting the login flow for new users."}})
	for w, want := range map[int]string{
		60: "You're documenting the login flow for new users.",
		30: "…the login flow for new users.",
		12: "…login flow…",
	} {
		got := ansi.Strip(foundCell(h, w, lipgloss.NewStyle()))
		if strings.TrimRight(got, " ") != want || ansi.StringWidth(got) != w {
			t.Errorf("%d columns: %q, want %q", w, got, want)
		}
	}
}

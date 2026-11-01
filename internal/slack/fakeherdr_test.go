package slack

import (
	"fmt"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/alekkowalczyk/herdr-tasks/internal/herdr"
	"github.com/alekkowalczyk/herdr-tasks/internal/reconcile"
)

// fakeHerdr is herdr in memory: workspaces, tabs and their layouts, the panes
// in them and each pane's foreground processes, strips, and agents. It
// records every call in calls, as "Method arg …", so a test can check what
// was asked of herdr and what wasn't. FocusTab and SendText, which Slack
// work must never call, are here so a call to them shows.
type fakeHerdr struct {
	mu      sync.Mutex
	ws      []herdr.Workspace
	tabs    []herdr.Tab // in workspace order, as herdr lists them
	panes   []herdr.Pane
	layouts map[string]*herdr.Node     // tab ID → its layout
	procs   map[string][]herdr.Process // pane ID → its foreground processes
	created []herdr.Created            // what CreateWorkspace and CreateTab made
	starts  []agentStart               // every StartAgent call
	// What the next StartAgent calls return, in turn. Once it's empty they
	// work, and claude runs in their pane.
	startErrs []error
	openErr   error // what adding a strip returns, when it's set
	// fail, when set, is asked about every call ("Method arg …") before it's
	// made, and a call it gives an error fails with it. It runs with the
	// fake locked: it may read the fields, but not call methods.
	fail func(call string) error
	// onStart, when set, runs at the start of each StartAgent call, with
	// the fake unlocked, so it may call into herdr-tasks again.
	onStart func(agentStart)
	calls   []string
	pid     int
}

// agentStart is one StartAgent call.
type agentStart struct {
	Name, Kind, Pane string
	Args             []string
	Timeout          time.Duration
}

var (
	_ Herdr            = (*fakeHerdr)(nil)
	_ reconcile.Strips = (*fakeHerdr)(nil)
)

func newFakeHerdr() *fakeHerdr {
	return &fakeHerdr{layouts: map[string]*herdr.Node{}, procs: map[string][]herdr.Process{}, pid: 4000}
}

// record records a call, and returns what fail says it returns.
func (f *fakeHerdr) record(method string, args ...any) error {
	parts := []string{method}
	for _, a := range args {
		parts = append(parts, fmt.Sprint(a))
	}
	call := strings.Join(parts, " ")
	f.calls = append(f.calls, call)
	if f.fail != nil {
		return f.fail(call)
	}
	return nil
}

// callsTo are the recorded calls to method.
func (f *fakeHerdr) callsTo(method string) []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []string
	for _, c := range f.calls {
		if c == method || strings.HasPrefix(c, method+" ") {
			out = append(out, c)
		}
	}
	return out
}

// methods are the methods called, each once, in the order first called.
func (f *fakeHerdr) methods() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []string
	for _, c := range f.calls {
		m, _, _ := strings.Cut(c, " ")
		if !slices.Contains(out, m) {
			out = append(out, m)
		}
	}
	return out
}

func (f *fakeHerdr) taken(id string) bool {
	return slices.ContainsFunc(f.ws, func(w herdr.Workspace) bool { return w.ID == id }) ||
		slices.ContainsFunc(f.tabs, func(t herdr.Tab) bool { return t.ID == id }) ||
		slices.ContainsFunc(f.panes, func(p herdr.Pane) bool { return p.ID == id })
}

// newID is the first of prefix1, prefix2, … that isn't taken.
func (f *fakeHerdr) newID(prefix string) string {
	for i := 1; ; i++ {
		if id := prefix + strconv.Itoa(i); !f.taken(id) {
			return id
		}
	}
}

func leaf(paneID string) *herdr.Node { return &herdr.Node{Type: "pane", PaneID: paneID} }

// addWorkspace adds a workspace without recording a call.
func (f *fakeHerdr) addWorkspace(id, label string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.ws = append(f.ws, herdr.Workspace{ID: id, Label: label, Number: len(f.ws) + 1})
}

// addTab adds a tab with one pane, at the end of its workspace, without
// recording a call, and returns the pane.
func (f *fakeHerdr) addTab(wsID, tabID, label, cwd string) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.insertTab(wsID, tabID, label, cwd)
}

func (f *fakeHerdr) insertTab(wsID, tabID, label, cwd string) string {
	at := len(f.tabs)
	for i, t := range f.tabs {
		if t.WorkspaceID == wsID {
			at = i + 1
		}
	}
	f.tabs = slices.Insert(f.tabs, at, herdr.Tab{ID: tabID, WorkspaceID: wsID, Label: label})
	pane := f.newID(wsID + ":p")
	f.panes = append(f.panes, herdr.Pane{ID: pane, TabID: tabID, WorkspaceID: wsID, Cwd: cwd})
	f.layouts[tabID] = leaf(pane)
	return pane
}

func (f *fakeHerdr) hasWorkspace(id string) bool {
	return slices.ContainsFunc(f.ws, func(w herdr.Workspace) bool { return w.ID == id })
}

func (f *fakeHerdr) pane(id string) (herdr.Pane, bool) {
	i := slices.IndexFunc(f.panes, func(p herdr.Pane) bool { return p.ID == id })
	if i < 0 {
		return herdr.Pane{}, false
	}
	return f.panes[i], true
}

// tabOf is the tab a pane is in.
func (f *fakeHerdr) tabOf(paneID string) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	p, _ := f.pane(paneID)
	return p.TabID
}

// label is a tab's label.
func (f *fakeHerdr) label(tabID string) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, t := range f.tabs {
		if t.ID == tabID {
			return t.Label
		}
	}
	return ""
}

func (f *fakeHerdr) Live() (*herdr.Live, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.record("Live"); err != nil {
		return nil, err
	}
	return &herdr.Live{Workspaces: slices.Clone(f.ws), Tabs: slices.Clone(f.tabs)}, nil
}

func (f *fakeHerdr) RenameTab(id, label string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.record("RenameTab", id, label); err != nil {
		return err
	}
	for i := range f.tabs {
		if f.tabs[i].ID == id {
			f.tabs[i].Label = label
			return nil
		}
	}
	return fmt.Errorf("no tab %s", id)
}

// MoveTab follows herdr: from and to are positions in the tab's workspace.
func (f *fakeHerdr) MoveTab(id string, from, to int) ([]herdr.Tab, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.record("MoveTab", id, from, to); err != nil {
		return nil, err
	}
	i := slices.IndexFunc(f.tabs, func(t herdr.Tab) bool { return t.ID == id })
	if i < 0 {
		return nil, fmt.Errorf("no tab %s", id)
	}
	ws := f.tabs[i].WorkspaceID
	var in, out []herdr.Tab
	for _, t := range f.tabs {
		if t.WorkspaceID == ws {
			in = append(in, t)
		} else {
			out = append(out, t)
		}
	}
	if from >= len(in) || in[from].ID != id {
		return nil, fmt.Errorf("tab %s isn't at %d", id, from)
	}
	moving := in[from]
	in = slices.Delete(in, from, from+1)
	in = slices.Insert(in, to, moving)
	var all []herdr.Tab
	for _, w := range f.ws {
		if w.ID == ws {
			all = append(all, in...)
			continue
		}
		for _, t := range out {
			if t.WorkspaceID == w.ID {
				all = append(all, t)
			}
		}
	}
	f.tabs = all
	return slices.Clone(in), nil
}

func (f *fakeHerdr) ReportWorkspaceTokens(workspaceID string, _ map[string]*string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.record("ReportWorkspaceTokens", workspaceID); err != nil {
		return err
	}
	return nil
}

// CreateWorkspace makes a workspace with one tab, as herdr does, and leaves
// the focus where it was.
func (f *fakeHerdr) CreateWorkspace(label, cwd string) (herdr.Created, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.record("CreateWorkspace", label, cwd); err != nil {
		return herdr.Created{}, err
	}
	ws := f.newID("w")
	f.ws = append(f.ws, herdr.Workspace{ID: ws, Label: label, Number: len(f.ws) + 1})
	tab := f.newID(ws + ":t")
	c := herdr.Created{WorkspaceID: ws, TabID: tab, PaneID: f.insertTab(ws, tab, "1", cwd)}
	f.created = append(f.created, c)
	return c, nil
}

// CreateTab makes a tab at the end of a workspace and leaves the focus where
// it was.
func (f *fakeHerdr) CreateTab(workspaceID, label, cwd string) (herdr.Created, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.record("CreateTab", workspaceID, label, cwd); err != nil {
		return herdr.Created{}, err
	}
	if !f.hasWorkspace(workspaceID) {
		return herdr.Created{}, fmt.Errorf("no workspace %s", workspaceID)
	}
	tab := f.newID(workspaceID + ":t")
	c := herdr.Created{WorkspaceID: workspaceID, TabID: tab, PaneID: f.insertTab(workspaceID, tab, label, cwd)}
	f.created = append(f.created, c)
	return c, nil
}

func cloneNode(n *herdr.Node) *herdr.Node {
	if n == nil {
		return nil
	}
	c := *n
	c.First, c.Second = cloneNode(n.First), cloneNode(n.Second)
	return &c
}

func (f *fakeHerdr) Layout(tabID string) (*herdr.Node, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.record("Layout", tabID); err != nil {
		return nil, err
	}
	n, ok := f.layouts[tabID]
	if !ok {
		return nil, fmt.Errorf("tab %s has no layout", tabID)
	}
	return cloneNode(n), nil
}

func (f *fakeHerdr) StartAgent(name, kind, paneID string, args []string, timeout time.Duration) error {
	start := agentStart{Name: name, Kind: kind, Pane: paneID, Args: slices.Clone(args), Timeout: timeout}
	f.mu.Lock()
	hook := f.onStart
	f.mu.Unlock()
	if hook != nil {
		hook(start)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.record("StartAgent", name, kind, paneID); err != nil {
		return err
	}
	f.starts = append(f.starts, start)
	if len(f.startErrs) > 0 {
		err := f.startErrs[0]
		f.startErrs = f.startErrs[1:]
		if err != nil {
			return err
		}
	}
	if _, ok := f.pane(paneID); !ok {
		return fmt.Errorf("no pane %s", paneID)
	}
	f.pid++
	f.procs[paneID] = append(f.procs[paneID], herdr.Process{PID: f.pid, Name: kind, Argv: append([]string{kind}, args...)})
	return nil
}

func (f *fakeHerdr) Processes(paneID string) ([]herdr.Process, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.record("Processes", paneID); err != nil {
		return nil, err
	}
	if _, ok := f.pane(paneID); !ok {
		return nil, fmt.Errorf("no pane %s", paneID)
	}
	return slices.Clone(f.procs[paneID]), nil
}

// The strips, as reconcile adds them: a pane split off the bottom of a tab.

func (f *fakeHerdr) Panes() ([]herdr.Pane, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.record("Panes"); err != nil {
		return nil, err
	}
	return slices.Clone(f.panes), nil
}

func (f *fakeHerdr) Open(tabID string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.record("Open", tabID); err != nil {
		return "", err
	}
	if f.openErr != nil {
		return "", f.openErr
	}
	root, ok := f.layouts[tabID]
	if !ok {
		return "", fmt.Errorf("no tab %s", tabID)
	}
	ws, _, _ := strings.Cut(tabID, ":")
	pane := f.newID(ws + ":p")
	f.panes = append(f.panes, herdr.Pane{ID: pane, TabID: tabID, WorkspaceID: ws, Label: reconcile.StripLabel})
	f.layouts[tabID] = &herdr.Node{Type: "split", Direction: "down", Ratio: 0.85, First: root, Second: leaf(pane)}
	return pane, nil
}

// without is n with the pane gone; its split is replaced by the other side.
func without(n *herdr.Node, paneID string) *herdr.Node {
	switch {
	case n == nil:
		return nil
	case !n.IsSplit():
		if n.PaneID == paneID {
			return nil
		}
		return n
	}
	first, second := without(n.First, paneID), without(n.Second, paneID)
	switch {
	case first == nil:
		return second
	case second == nil:
		return first
	}
	c := *n
	c.First, c.Second = first, second
	return &c
}

func (f *fakeHerdr) Close(paneID string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.record("Close", paneID); err != nil {
		return err
	}
	p, ok := f.pane(paneID)
	if !ok {
		return fmt.Errorf("no pane %s", paneID)
	}
	f.panes = slices.DeleteFunc(f.panes, func(o herdr.Pane) bool { return o.ID == paneID })
	delete(f.procs, paneID)
	f.layouts[p.TabID] = without(f.layouts[p.TabID], paneID)
	return nil
}

func (f *fakeHerdr) CloseTab(tabID string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.record("CloseTab", tabID); err != nil {
		return err
	}
	f.tabs = slices.DeleteFunc(f.tabs, func(t herdr.Tab) bool { return t.ID == tabID })
	f.panes = slices.DeleteFunc(f.panes, func(p herdr.Pane) bool {
		if p.TabID == tabID {
			delete(f.procs, p.ID)
		}
		return p.TabID == tabID
	})
	delete(f.layouts, tabID)
	return nil
}

func (f *fakeHerdr) Relaunch(paneID string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.record("Relaunch", paneID); err != nil {
		return err
	}
	return nil
}

func (f *fakeHerdr) Rename(paneID, label string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.record("Rename", paneID, label); err != nil {
		return err
	}
	for i := range f.panes {
		if f.panes[i].ID == paneID {
			f.panes[i].Label = label
			return nil
		}
	}
	return fmt.Errorf("no pane %s", paneID)
}

// What Slack work must never call.

func (f *fakeHerdr) FocusTab(id string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.record("FocusTab", id); err != nil {
		return err
	}
	return nil
}

func (f *fakeHerdr) SendText(paneID, text string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.record("SendText", paneID, text); err != nil {
		return err
	}
	return nil
}

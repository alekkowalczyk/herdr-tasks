package assistant

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/alekkowalczyk/herdr-tasks/internal/herdr"
	"github.com/alekkowalczyk/herdr-tasks/internal/reconcile"
	"github.com/alekkowalczyk/herdr-tasks/internal/slack"
	"github.com/alekkowalczyk/herdr-tasks/internal/store"
	"github.com/alekkowalczyk/herdr-tasks/internal/strip"
	"github.com/alekkowalczyk/herdr-tasks/internal/task"
)

// fakeHerdr is herdr in memory: workspaces, tabs of one pane each, and what
// runs in each pane. It has no strips. It records every call, as "Method
// arg …".
type fakeHerdr struct {
	mu    sync.Mutex
	ws    []herdr.Workspace
	tabs  []herdr.Tab
	panes []herdr.Pane
	// What the next StartAgent calls return, in turn. Once it's empty they
	// work, and claude runs in their pane.
	startErrs []error
	starts    [][]string // the args of each StartAgent call
	calls     []string
}

var _ Herdr = (*fakeHerdr)(nil)

func (f *fakeHerdr) record(method string, args ...any) {
	parts := []string{method}
	for _, a := range args {
		parts = append(parts, fmt.Sprint(a))
	}
	f.calls = append(f.calls, strings.Join(parts, " "))
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

func (f *fakeHerdr) addWorkspace(id, label string) {
	f.ws = append(f.ws, herdr.Workspace{ID: id, Label: label, Number: len(f.ws) + 1})
}

// addTab adds a tab with one pane, at the end of its workspace, and returns
// the pane.
func (f *fakeHerdr) addTab(ws, tab, label string) string {
	at := len(f.tabs)
	for i, t := range f.tabs {
		if t.WorkspaceID == ws {
			at = i + 1
		}
	}
	f.tabs = slices.Insert(f.tabs, at, herdr.Tab{ID: tab, WorkspaceID: ws, Label: label})
	pane := ws + ":p" + strconv.Itoa(len(f.panes)+1)
	f.panes = append(f.panes, herdr.Pane{ID: pane, TabID: tab, WorkspaceID: ws})
	return pane
}

// setPane changes what runs in a pane.
func (f *fakeHerdr) setPane(id, agent, status string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for i := range f.panes {
		if f.panes[i].ID == id {
			f.panes[i].Agent, f.panes[i].AgentStatus = agent, status
		}
	}
}

func (f *fakeHerdr) Live() (*herdr.Live, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.record("Live")
	return &herdr.Live{Workspaces: slices.Clone(f.ws), Tabs: slices.Clone(f.tabs)}, nil
}

func (f *fakeHerdr) RenameTab(id, label string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.record("RenameTab", id, label)
	for i := range f.tabs {
		if f.tabs[i].ID == id {
			f.tabs[i].Label = label
		}
	}
	return nil
}

func (f *fakeHerdr) MoveTab(id string, from, to int) ([]herdr.Tab, error) {
	return nil, errors.New("tabs aren't sorted in these tests")
}

func (f *fakeHerdr) ReportWorkspaceTokens(string, map[string]*string) error { return nil }

func (f *fakeHerdr) CreateWorkspace(label, cwd string) (herdr.Created, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.record("CreateWorkspace", label, cwd)
	ws := "w" + strconv.Itoa(len(f.ws)+1)
	f.addWorkspace(ws, label)
	tab := ws + ":t1"
	return herdr.Created{WorkspaceID: ws, TabID: tab, PaneID: f.addTab(ws, tab, "1")}, nil
}

func (f *fakeHerdr) CreateTab(ws, label, cwd string) (herdr.Created, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.record("CreateTab", ws, label, cwd)
	tab := ws + ":t" + strconv.Itoa(len(f.tabs)+1)
	return herdr.Created{WorkspaceID: ws, TabID: tab, PaneID: f.addTab(ws, tab, label)}, nil
}

func (f *fakeHerdr) CloseTab(tab string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.record("CloseTab", tab)
	f.tabs = slices.DeleteFunc(f.tabs, func(t herdr.Tab) bool { return t.ID == tab })
	f.panes = slices.DeleteFunc(f.panes, func(p herdr.Pane) bool { return p.TabID == tab })
	return nil
}

func (f *fakeHerdr) Layout(tab string) (*herdr.Node, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, p := range f.panes {
		if p.TabID == tab {
			return &herdr.Node{Type: "pane", PaneID: p.ID}, nil
		}
	}
	return nil, fmt.Errorf("no tab %s", tab)
}

func (f *fakeHerdr) Panes() ([]herdr.Pane, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.panes), nil
}

func (f *fakeHerdr) StartAgent(name, kind, pane string, args []string, _ time.Duration) error {
	f.mu.Lock()
	f.record("StartAgent", name, kind, pane)
	f.starts = append(f.starts, slices.Clone(args))
	var err error
	if len(f.startErrs) > 0 {
		err, f.startErrs = f.startErrs[0], f.startErrs[1:]
	}
	f.mu.Unlock()
	if err == nil {
		f.setPane(pane, kind, "idle")
	}
	return err
}

func (f *fakeHerdr) FocusTab(id string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.record("FocusTab", id)
	return nil
}

func (f *fakeHerdr) TypeText(pane, text string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.record("TypeText", pane, text)
	return nil
}

// setting is a herdr with two tasks in Acme, w1:t1 and w1:t2 "Login loop",
// saved in a state folder, and the assistant's config for it. ClaudeDir
// has no session logs.
func setting(t *testing.T) (*fakeHerdr, *store.Store, Config) {
	t.Helper()
	old := slack.RetryDelay
	slack.RetryDelay = time.Millisecond
	t.Cleanup(func() { slack.RetryDelay = old })
	f := &fakeHerdr{}
	f.addWorkspace("w1", "Acme")
	f.addTab("w1", "w1:t1", "1")
	f.addTab("w1", "w1:t2", "Login loop")
	dir := t.TempDir()
	st := &store.Store{Dir: dir, Session: "test"}
	if _, err := reconcile.Run(f, st, reconcile.Options{}, time.Now(), nil); err != nil {
		t.Fatal(err)
	}
	cfg := Config{Exe: "/opt/herdr tasks/bin/herdr-tasks", Dir: dir, Cwd: "/home/dana/acme", ClaudeDir: t.TempDir()}
	return f, st, cfg
}

// record is the assistant's record, by its tab.
func record(t *testing.T, st *store.Store, tab string) *task.Task {
	t.Helper()
	s, err := st.Load()
	if err != nil {
		t.Fatal(err)
	}
	tk, ok := s.Tasks[tab]
	if !ok {
		t.Fatalf("no record for %s", tab)
	}
	return tk
}

var uuid = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`)

// The first open makes a workspace called Assistant, with the assistant's
// tab: a tool, with a new Claude session, which starts with the guide and
// the settings.
func TestOpenMakesTheTab(t *testing.T) {
	f, st, cfg := setting(t)
	o, err := Open(f, st, reconcile.Options{}, cfg, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if !o.New || !o.Started || o.Tab != "w2:t1" || o.Pane == "" {
		t.Errorf("opened %+v, want a new tab w2:t1, with Claude started", o)
	}
	if got := f.callsTo("CreateWorkspace"); !slices.Equal(got, []string{"CreateWorkspace Assistant /home/dana/acme"}) {
		t.Errorf("CreateWorkspace calls = %q", got)
	}
	if got := f.callsTo("RenameTab"); !slices.Contains(got, "RenameTab w2:t1 Assistant") {
		t.Errorf("the tab wasn't named Assistant: %q", got)
	}
	tk := record(t, st, o.Tab)
	if tk.Stage != task.Tool || tk.Title != "Assistant" || tk.Assistant == nil || !uuid.MatchString(tk.Assistant.SessionID) {
		t.Fatalf("record = %+v, want the Assistant tool with a session ID", tk)
	}
	if got := f.callsTo("StartAgent"); !slices.Equal(got, []string{"StartAgent assistant claude " + o.Pane}) {
		t.Errorf("StartAgent calls = %q", got)
	}
	guide := filepath.Join(cfg.Dir, "assistant", "guide.md")
	settings := filepath.Join(cfg.Dir, "assistant", "settings.json")
	want := Args(tk.Assistant.SessionID, false, guide, settings)
	if !slices.Equal(f.starts[0], want) || want[0] != "--session-id" {
		t.Errorf("claude's args = %q, want %q", f.starts[0], want)
	}
	for path, want := range map[string]string{guide: Guide, settings: string(Settings(cfg.Exe))} {
		b, err := os.ReadFile(path)
		if err != nil || string(b) != want {
			t.Errorf("%s = %q, %v", path, b, err)
		}
		if fi, err := os.Stat(path); err != nil || fi.Mode().Perm() != 0o600 {
			t.Errorf("%s is %v, want readable by you only", path, fi.Mode())
		}
	}
}

// A workspace called Assistant, in any case, gets the tab.
func TestOpenInTheAssistantWorkspace(t *testing.T) {
	f, st, cfg := setting(t)
	f.addWorkspace("w2", "assistant")
	o, err := Open(f, st, reconcile.Options{}, cfg, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if got := f.callsTo("CreateTab"); !slices.Equal(got, []string{"CreateTab w2 Assistant /home/dana/acme"}) || len(f.callsTo("CreateWorkspace")) > 0 {
		t.Errorf("calls = %q", f.calls)
	}
	if record(t, st, o.Tab).Assistant == nil {
		t.Error("the new tab isn't the assistant")
	}
}

// Opened again, the assistant is found, and Claude, running, is left alone.
// Opened after Claude exits, the session starts again: resumed once it has
// a log, and new before then. Another program there is left alone.
func TestOpenAgain(t *testing.T) {
	f, st, cfg := setting(t)
	first, err := Open(f, st, reconcile.Options{}, cfg, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	o, err := Open(f, st, reconcile.Options{}, cfg, time.Now())
	if err != nil || o.New || o.Started || o != (Opened{Tab: first.Tab, Pane: first.Pane}) {
		t.Errorf("opened again: %+v, %v, want the same tab and pane, as they were", o, err)
	}
	if n := len(f.callsTo("StartAgent")); n != 1 || len(f.callsTo("CreateWorkspace")) != 1 {
		t.Errorf("Claude was started %d times", n)
	}

	id := record(t, st, first.Tab).Assistant.SessionID
	f.setPane(first.Pane, "", "")
	if _, err := Open(f, st, reconcile.Options{}, cfg, time.Now()); err != nil {
		t.Fatal(err)
	}
	if got := f.starts[1]; got[0] != "--session-id" || got[1] != id {
		t.Errorf("with no log, claude got %q, want the same session, new", got)
	}

	log := filepath.Join(cfg.ClaudeDir, "projects", "-home-dana-acme", id+".jsonl")
	if err := os.MkdirAll(filepath.Dir(log), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(log, []byte("{}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	f.setPane(first.Pane, "", "")
	if _, err := Open(f, st, reconcile.Options{}, cfg, time.Now()); err != nil {
		t.Fatal(err)
	}
	if got := f.starts[2]; got[0] != "--resume" || got[1] != id {
		t.Errorf("with a log, claude got %q, want --resume %s", got, id)
	}

	f.setPane(first.Pane, "codex", "idle")
	if o, err := Open(f, st, reconcile.Options{}, cfg, time.Now()); err != nil || o.Started {
		t.Errorf("with codex there: %+v, %v", o, err)
	}
	if n := len(f.starts); n != 3 {
		t.Errorf("Claude was started %d times, want 3", n)
	}
}

// Two opens at once make one tab.
func TestOpensAtOnce(t *testing.T) {
	f, st, cfg := setting(t)
	var wg sync.WaitGroup
	tabs := make([]string, 4)
	for i := range tabs {
		wg.Add(1)
		go func() {
			defer wg.Done()
			o, err := Open(f, st, reconcile.Options{}, cfg, time.Now())
			if err != nil {
				t.Error(err)
			}
			tabs[i] = o.Tab
		}()
	}
	wg.Wait()
	if n := len(f.callsTo("CreateWorkspace")); n != 1 || len(f.starts) != 1 {
		t.Errorf("%d workspaces made, Claude started %d times, want one each", n, len(f.starts))
	}
	for _, tab := range tabs {
		if tab != tabs[0] {
			t.Errorf("opened %q", tabs)
		}
	}
}

// When Claude doesn't start, the tab stays, and the error says why. When
// herdr says it launched Claude but didn't get it ready, it may be starting.
func TestOpenWhenClaudeDoesntStart(t *testing.T) {
	f, st, cfg := setting(t)
	no := errors.New("the pane isn't at a shell prompt")
	f.startErrs = []error{no, no, no, no, no}
	o, err := Open(f, st, reconcile.Options{}, cfg, time.Now())
	if !errors.Is(err, no) || !strings.HasPrefix(err.Error(), "Claude didn't start") || o.Tab == "" || o.Started {
		t.Errorf("opened %+v, %v", o, err)
	}
	if record(t, st, o.Tab).Assistant == nil {
		t.Error("the tab lost its record")
	}

	f.startErrs = []error{&herdr.Error{Code: "timeout", Message: "not ready"}}
	o, err = Open(f, st, reconcile.Options{}, cfg, time.Now())
	if err == nil || !strings.Contains(err.Error(), "may not have finished starting") || !o.Started {
		t.Errorf("opened %+v, %v", o, err)
	}
	if n := len(f.starts); n != 6 {
		t.Errorf("Claude was started %d times, want 5 tries, then 1", n)
	}
}

// Ask types a line about a task into the assistant, for you to finish, but
// not while Claude asks you something, and not about its own tab.
func TestAsk(t *testing.T) {
	f, st, cfg := setting(t)
	o, err := Ask(f, st, reconcile.Options{}, cfg, "w1:t2", time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if got := f.callsTo("TypeText"); !slices.Equal(got, []string{"TypeText " + o.Pane + " About Login loop (w1:t2): "}) {
		t.Errorf("typed %q", got)
	}
	if len(f.callsTo("FocusTab")) > 0 {
		t.Error("Ask showed the tab in herdr")
	}
	if _, err := Ask(f, st, reconcile.Options{}, cfg, "w1:t1", time.Now()); err != nil {
		t.Fatal(err)
	}
	if got := f.callsTo("TypeText"); got[len(got)-1] != "TypeText "+o.Pane+" About tab 1 (w1:t1): " {
		t.Errorf("typed %q", got)
	}

	if _, err := Ask(f, st, reconcile.Options{}, cfg, o.Tab, time.Now()); !errors.Is(err, ErrOwnTab) {
		t.Errorf("asked about its own tab: %v", err)
	}
	if _, err := Ask(f, st, reconcile.Options{}, cfg, "w9:t9", time.Now()); !errors.Is(err, reconcile.ErrNoTab) {
		t.Errorf("asked about a tab that isn't there: %v", err)
	}
	f.setPane(o.Pane, "claude", "blocked")
	if _, err := Ask(f, st, reconcile.Options{}, cfg, "w1:t2", time.Now()); !errors.Is(err, ErrAsking) {
		t.Errorf("asked while Claude asks: %v", err)
	}
	if n := len(f.callsTo("TypeText")); n != 2 {
		t.Errorf("typed %d times, want 2", n)
	}
}

// Show asks, then shows the assistant, which goes back to the tab you were
// in; it shows it too when Claude asks something, so you can answer.
func TestShow(t *testing.T) {
	f, st, cfg := setting(t)
	if err := Show(f, st, reconcile.Options{}, cfg, "w1:t2", "w1:t1", time.Now()); err != nil {
		t.Fatal(err)
	}
	a := f.callsTo("TypeText")
	if len(a) != 1 || !strings.HasSuffix(a[0], " About Login loop (w1:t2): ") {
		t.Errorf("typed %q", a)
	}
	tab := strings.TrimPrefix(f.callsTo("FocusTab")[0], "FocusTab ")
	if back := record(t, st, tab).Assistant.Back; back != "w1:t1" {
		t.Errorf("back = %q", back)
	}
	p, _ := Pane(tab, f.panes)
	f.setPane(p.ID, "claude", "blocked")
	if err := Show(f, st, reconcile.Options{}, cfg, "w1:t2", "w1:t2", time.Now()); !errors.Is(err, ErrAsking) {
		t.Errorf("shown while Claude asks: %v", err)
	}
	if got := f.callsTo("FocusTab"); !slices.Equal(got, []string{"FocusTab " + tab, "FocusTab " + tab}) {
		t.Errorf("FocusTab calls = %q", got)
	}
	if _, err := Toggle(f, st, reconcile.Options{}, cfg, tab, time.Now()); err != nil || record(t, st, tab).Assistant.Back != "w1:t2" {
		t.Errorf("back = %q, %v", record(t, st, tab).Assistant.Back, err)
	}
	if err := Show(f, st, reconcile.Options{}, cfg, "w9:t9", "w1:t1", time.Now()); !errors.Is(err, reconcile.ErrNoTab) {
		t.Errorf("shown for a tab that isn't there: %v", err)
	}
}

// Ask types nothing into a Claude that may not be ready.
func TestAskWhenClaudeIsntReady(t *testing.T) {
	f, st, cfg := setting(t)
	f.startErrs = []error{&herdr.Error{Code: "agent_not_ready", Message: "a dialog is open"}}
	if _, err := Ask(f, st, reconcile.Options{}, cfg, "w1:t2", time.Now()); err == nil {
		t.Error("no error")
	}
	if got := f.callsTo("TypeText"); len(got) > 0 {
		t.Errorf("typed %q", got)
	}
}

// Toggle shows the assistant, and from there the tab you came from.
func TestToggle(t *testing.T) {
	f, st, cfg := setting(t)
	tab, err := Toggle(f, st, reconcile.Options{}, cfg, "w1:t2", time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if record(t, st, tab).Assistant.Back != "w1:t2" {
		t.Errorf("back = %q", record(t, st, tab).Assistant.Back)
	}
	back, err := Toggle(f, st, reconcile.Options{}, cfg, tab, time.Now())
	if err != nil || back != "w1:t2" {
		t.Errorf("back to %q, %v", back, err)
	}
	if got := f.callsTo("FocusTab"); !slices.Equal(got, []string{"FocusTab " + tab, "FocusTab w1:t2"}) {
		t.Errorf("FocusTab calls = %q", got)
	}

	if err := f.CloseTab("w1:t2"); err != nil {
		t.Fatal(err)
	}
	if _, err := Toggle(f, st, reconcile.Options{}, cfg, tab, time.Now()); !errors.Is(err, ErrNoBack) {
		t.Errorf("back to a closed tab: %v", err)
	}
}

// Toggle shows the tab when Claude didn't start there, so you see why.
func TestToggleWhenClaudeDoesntStart(t *testing.T) {
	f, st, cfg := setting(t)
	no := errors.New("the pane isn't at a shell prompt")
	f.startErrs = []error{no, no, no, no, no}
	tab, err := Toggle(f, st, reconcile.Options{}, cfg, "w1:t1", time.Now())
	if !errors.Is(err, no) || tab == "" {
		t.Errorf("toggled to %q, %v", tab, err)
	}
	if got := f.callsTo("FocusTab"); !slices.Equal(got, []string{"FocusTab " + tab}) {
		t.Errorf("FocusTab calls = %q", got)
	}
}

// The settings run the hook with herdr-tasks quoted for sh, and let Claude
// run the commands that read without asking, with their arguments, but
// nothing more of herdr's command groups.
func TestSettings(t *testing.T) {
	var s struct {
		Hooks map[string][]struct {
			Hooks []struct {
				Type, Command string
				Timeout       int
			}
		}
		Permissions struct{ Allow []string }
	}
	if err := json.Unmarshal(Settings("/opt/it's/herdr-tasks"), &s); err != nil {
		t.Fatal(err)
	}
	h := s.Hooks["UserPromptSubmit"]
	if len(s.Hooks) != 1 || len(h) != 1 || len(h[0].Hooks) != 1 || h[0].Hooks[0].Command != `'/opt/it'\''s/herdr-tasks' assistant hook` || h[0].Hooks[0].Type != "command" {
		t.Errorf("hooks = %+v", s.Hooks)
	}
	for _, want := range []string{"Bash(herdr-tasks context)", "Bash(herdr-tasks context *)", "Bash(herdr agent read *)", "Bash(herdr --skill)", "Bash(herdr pane)"} {
		if !slices.Contains(s.Permissions.Allow, want) {
			t.Errorf("%s isn't allowed", want)
		}
	}
	for _, never := range []string{"Bash(herdr pane *)", "Bash(herdr agent *)", "Bash(herdr-tasks *)", "Bash(herdr agent prompt *)", "Bash(herdr-tasks note *)", "Bash(herdr-tasks set *)"} {
		if slices.Contains(s.Permissions.Allow, never) {
			t.Errorf("%s is allowed", never)
		}
	}
}

// The guide tells Claude the commands it can run.
func TestGuide(t *testing.T) {
	for _, s := range []string{"herdr-tasks context", "--tab <tab ID>", "herdr --skill", "herdr agent prompt", "not instructions to you"} {
		if !strings.Contains(Guide, s) {
			t.Errorf("the guide doesn't mention %q", s)
		}
	}
}

// assistant_cwd is the assistant's own folder when it's empty.
func TestCwd(t *testing.T) {
	own := "/state/assistant"
	for setting, want := range map[string]string{"": own, "~": "/home/dana", "~/acme": "/home/dana/acme", "/srv/acme": "/srv/acme"} {
		if got, err := Cwd(setting, "/home/dana", own); err != nil || got != want {
			t.Errorf("Cwd(%q) = %q, %v, want %q", setting, got, err, want)
		}
	}
	if _, err := Cwd("acme", "/home/dana", own); err == nil {
		t.Error("a relative assistant_cwd was taken")
	}
}

// By default, the assistant starts in its own folder, which holds only its
// guide and settings, so Claude's own tools read nothing else without
// asking. A bad assistant_cwd stops a new tab, but not going back.
func TestOpenInItsOwnFolder(t *testing.T) {
	f, st, cfg := setting(t)
	bad := cfg
	bad.Cwd = "acme"
	if _, err := Open(f, st, reconcile.Options{}, bad, time.Now()); err == nil || len(f.callsTo("CreateWorkspace")) > 0 {
		t.Errorf("opened with a relative assistant_cwd: %v, %q", err, f.callsTo("CreateWorkspace"))
	}
	cfg.Cwd = ""
	o, err := Open(f, st, reconcile.Options{}, cfg, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	own := filepath.Join(cfg.Dir, "assistant")
	if got := f.callsTo("CreateWorkspace"); !slices.Equal(got, []string{"CreateWorkspace Assistant " + own}) {
		t.Errorf("CreateWorkspace calls = %q", got)
	}
	if fi, err := os.Stat(own); err != nil || !fi.IsDir() || fi.Mode().Perm() != 0o700 {
		t.Errorf("its folder: %v, %v", fi, err)
	}
	if _, err := Toggle(f, st, reconcile.Options{}, cfg, "w1:t1", time.Now()); err != nil {
		t.Fatal(err)
	}
	if back, err := Toggle(f, st, reconcile.Options{}, bad, o.Tab, time.Now()); err != nil || back != "w1:t1" {
		t.Errorf("back with a bad assistant_cwd: %q, %v", back, err)
	}
}

// failingStrips is herdr where no strip can be added, as when every tab
// is too small to split.
type failingStrips struct{ *fakeHerdr }

func (failingStrips) Open(string) (string, error) {
	return "", errors.New("the tab is too small to split")
}
func (failingStrips) Close(string) error          { return nil }
func (failingStrips) Relaunch(string) error       { return nil }
func (failingStrips) Rename(string, string) error { return nil }

// A strip that can't be added to another tab doesn't stop the assistant:
// its tab stays, and Claude starts there.
func TestOpenWhenAStripFails(t *testing.T) {
	f, st, cfg := setting(t)
	o, err := Open(failingStrips{f}, st, reconcile.Options{Strip: true}, cfg, time.Now())
	if err != nil || !o.New || !o.Started {
		t.Fatalf("opened %+v, %v", o, err)
	}
	if got := f.callsTo("CloseTab"); len(got) > 0 {
		t.Errorf("CloseTab calls = %q", got)
	}
	if record(t, st, o.Tab).Assistant == nil {
		t.Error("the assistant's record is gone")
	}
}

// Pane is the one Claude runs in, when the tab has more.
func TestPanePrefersClaude(t *testing.T) {
	panes := []herdr.Pane{
		{ID: "w2:p1", TabID: "w2:t1", Label: strip.Label},
		{ID: "w2:p2", TabID: "w2:t1"},
		{ID: "w2:p3", TabID: "w2:t1", Agent: "claude"},
		{ID: "w1:p1", TabID: "w1:t1", Agent: "claude"},
	}
	if p, ok := Pane("w2:t1", panes); !ok || p.ID != "w2:p3" {
		t.Errorf("pane = %v, %v", p.ID, ok)
	}
	if p, ok := Pane("w2:t1", panes[:2]); !ok || p.ID != "w2:p2" {
		t.Errorf("without Claude, pane = %v, %v", p.ID, ok)
	}
	if _, ok := Pane("w2:t1", panes[:1]); ok {
		t.Error("the strip is a pane")
	}
}

// The session Claude says it runs, as after /clear, is the one to resume.
// What isn't a session ID is left out.
func TestKeepSession(t *testing.T) {
	f, st, cfg := setting(t)
	o, err := Open(f, st, reconcile.Options{}, cfg, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	live, _ := f.Live()
	id := "0b6f8a52-3c1d-4e2f-9a7b-5c4d3e2f1a0b"
	if err := KeepSession(st, live, id); err != nil || record(t, st, o.Tab).Assistant.SessionID != id {
		t.Errorf("session = %q, %v", record(t, st, o.Tab).Assistant.SessionID, err)
	}
	for _, bad := range []string{"", "x; rm -rf ~", "0B6F8A52-3C1D-4E2F-9A7B-5C4D3E2F1A0B", id + " "} {
		if err := KeepSession(st, live, bad); err != nil || record(t, st, o.Tab).Assistant.SessionID != id {
			t.Errorf("%q: session = %q, %v", bad, record(t, st, o.Tab).Assistant.SessionID, err)
		}
	}
}

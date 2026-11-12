package reopen

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/alekkowalczyk/herdr-tasks/internal/herdr"
	"github.com/alekkowalczyk/herdr-tasks/internal/reconcile"
	"github.com/alekkowalczyk/herdr-tasks/internal/slack"
	"github.com/alekkowalczyk/herdr-tasks/internal/store"
	"github.com/alekkowalczyk/herdr-tasks/internal/task"
)

// fakeHerdr is herdr in memory: workspaces, and tabs of one pane each. It
// has no strips. It records every call that changes something, as
// "Method arg …".
type fakeHerdr struct {
	ws    []herdr.Workspace
	tabs  []herdr.Tab
	panes []herdr.Pane
	// What the next StartAgent call returns.
	startErr error
	starts   [][]string // the args of each StartAgent call
	calls    []string
}

var _ Herdr = (*fakeHerdr)(nil)

func (f *fakeHerdr) record(method string, args ...any) {
	parts := []string{method}
	for _, a := range args {
		parts = append(parts, fmt.Sprint(a))
	}
	f.calls = append(f.calls, strings.Join(parts, " "))
}

func (f *fakeHerdr) addTab(ws, tab, label string) string {
	f.tabs = append(f.tabs, herdr.Tab{ID: tab, WorkspaceID: ws, Label: label})
	pane := ws + ":p" + strconv.Itoa(len(f.panes)+1)
	f.panes = append(f.panes, herdr.Pane{ID: pane, TabID: tab, WorkspaceID: ws})
	return pane
}

func (f *fakeHerdr) Live() (*herdr.Live, error) {
	return &herdr.Live{Workspaces: slices.Clone(f.ws), Tabs: slices.Clone(f.tabs)}, nil
}

func (f *fakeHerdr) RenameTab(id, label string) error {
	f.record("RenameTab", id, label)
	for i := range f.tabs {
		if f.tabs[i].ID == id {
			f.tabs[i].Label = label
		}
	}
	return nil
}

func (f *fakeHerdr) MoveTab(string, int, int) ([]herdr.Tab, error) {
	return nil, errors.New("tabs aren't sorted in these tests")
}

func (f *fakeHerdr) ReportWorkspaceTokens(string, map[string]*string) error { return nil }

func (f *fakeHerdr) CreateWorkspace(label, cwd string) (herdr.Created, error) {
	f.record("CreateWorkspace", label, cwd)
	ws := "w" + strconv.Itoa(len(f.ws)+1)
	f.ws = append(f.ws, herdr.Workspace{ID: ws, Label: label})
	tab := ws + ":t1"
	return herdr.Created{WorkspaceID: ws, TabID: tab, PaneID: f.addTab(ws, tab, "1")}, nil
}

func (f *fakeHerdr) CreateTab(ws, label, cwd string) (herdr.Created, error) {
	f.record("CreateTab", ws, label, cwd)
	tab := ws + ":t" + strconv.Itoa(len(f.tabs)+1)
	return herdr.Created{WorkspaceID: ws, TabID: tab, PaneID: f.addTab(ws, tab, label)}, nil
}

func (f *fakeHerdr) CloseTab(tab string) error {
	f.record("CloseTab", tab)
	f.tabs = slices.DeleteFunc(f.tabs, func(t herdr.Tab) bool { return t.ID == tab })
	f.panes = slices.DeleteFunc(f.panes, func(p herdr.Pane) bool { return p.TabID == tab })
	return nil
}

func (f *fakeHerdr) StartAgent(name, kind, pane string, args []string, _ time.Duration) error {
	f.record("StartAgent", name, kind, pane)
	f.starts = append(f.starts, slices.Clone(args))
	return f.startErr
}

// callsTo are the recorded calls to method.
func (f *fakeHerdr) callsTo(method string) []string {
	var out []string
	for _, c := range f.calls {
		if strings.HasPrefix(c, method+" ") {
			out = append(out, c)
		}
	}
	return out
}

const session = "0b6c1d2e-0000-4000-8000-000000000001"

// setting is herdr with one task in Acme, w1:t1 "Deploy", and a closed
// task, "Login loop", that ran Claude session session in project. Its ID
// is the second value. claudeDir has that session's log.
type setup struct {
	f         *fakeHerdr
	st        *store.Store
	id        string
	project   string
	claudeDir string
	now       time.Time
}

func setting(t *testing.T, closed *task.Task) setup {
	t.Helper()
	old := slack.RetryDelay
	slack.RetryDelay = time.Millisecond
	t.Cleanup(func() { slack.RetryDelay = old })
	f := &fakeHerdr{ws: []herdr.Workspace{{ID: "w1", Label: "Acme"}}}
	f.addTab("w1", "w1:t1", "Deploy")
	st := &store.Store{Dir: t.TempDir(), Session: "test"}
	u := setup{f: f, st: st, project: t.TempDir(), claudeDir: t.TempDir(), now: time.Date(2026, 11, 2, 9, 0, 0, 0, time.UTC)}
	if closed.Cwd == "" {
		closed.Cwd = u.project
	}
	log := filepath.Join(u.claudeDir, "projects", "-acme-login", session+".jsonl")
	if err := os.MkdirAll(filepath.Dir(log), 0o755); err != nil {
		t.Fatal(err)
	}
	line := fmt.Sprintf(`{"type":"user","cwd":%q,"sessionId":%q}`+"\n", u.project, session)
	if err := os.WriteFile(log, []byte(line), 0o600); err != nil {
		t.Fatal(err)
	}
	err := st.Update(func(s *store.State) error {
		s.Close(closed, u.now.Add(-time.Hour))
		u.id = s.Closed[0].ID
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := reconcile.Run(f, st, reconcile.Options{}, u.now, nil); err != nil {
		t.Fatal(err)
	}
	return u
}

func loginLoop() *task.Task {
	return &task.Task{TabID: "w1:t2", Workspace: "Acme", Title: "Login loop", Stage: task.Done,
		Note: "retry after the deploy", Claude: session,
		Links: []task.Link{{URL: "https://linear.app/acme/issue/ACM-12/login-loop"}}}
}

func (u setup) reopen(t *testing.T) (Reopened, error) {
	t.Helper()
	return Task(u.f, u.st, reconcile.Options{}, u.claudeDir, u.id, u.now)
}

func TestReopenMakesATabWithTheTaskAsItWas(t *testing.T) {
	u := setting(t, loginLoop())
	r, err := u.reopen(t)
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"CreateTab w1 Login loop " + u.project}; !slices.Equal(u.f.callsTo("CreateTab"), want) {
		t.Fatalf("CreateTab = %q, want %q", u.f.callsTo("CreateTab"), want)
	}
	s, _ := u.st.Load()
	tk, ok := s.Tasks[r.Tab]
	if !ok || r.Tab == "w1:t1" {
		t.Fatalf("no task in the new tab %q", r.Tab)
	}
	if tk.TabID != r.Tab || tk.Stage != task.Done || tk.Note != "retry after the deploy" || len(tk.Links) != 1 || tk.Workspace != "Acme" {
		t.Fatalf("task = %+v", tk)
	}
	if last := tk.Log[len(tk.Log)-1]; last.Text != "Reopened" || last.Who != "you" || !last.At.Equal(u.now) {
		t.Errorf("log = %+v", tk.Log)
	}
	// Reopening it is something done in it.
	if !tk.LastActive().Equal(u.now) {
		t.Errorf("last active %v, want the reopen, %v", tk.LastActive(), u.now)
	}
	if len(s.Closed) != 0 {
		t.Errorf("still closed: %+v", s.Closed)
	}
	// Claude resumes its session, in the tab's pane.
	if !r.Resumed || len(u.f.starts) != 1 || !slices.Equal(u.f.starts[0], []string{"--resume", session}) {
		t.Fatalf("resumed %v with %q", r.Resumed, u.f.starts)
	}
	if got := u.f.callsTo("StartAgent"); len(got) != 1 || !strings.HasSuffix(got[0], " claude w1:p2") {
		t.Errorf("StartAgent = %q", got)
	}
}

func TestReopenMakesItsWorkspaceWhenItsGone(t *testing.T) {
	closed := loginLoop()
	closed.Workspace = "Billing"
	u := setting(t, closed)
	r, err := u.reopen(t)
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"CreateWorkspace Billing " + u.project}; !slices.Equal(u.f.callsTo("CreateWorkspace"), want) {
		t.Fatalf("CreateWorkspace = %q", u.f.callsTo("CreateWorkspace"))
	}
	if !slices.Contains(u.f.callsTo("RenameTab"), "RenameTab "+r.Tab+" Login loop") {
		t.Errorf("RenameTab = %q", u.f.callsTo("RenameTab"))
	}
	s, _ := u.st.Load()
	if tk := s.Tasks[r.Tab]; tk == nil || tk.Workspace != "Billing" || tk.Title != "Login loop" {
		t.Fatalf("task = %+v", tk)
	}
}

func TestReopenWithoutASessionLogOpensItsFolder(t *testing.T) {
	closed := loginLoop()
	closed.Claude = "1d2c3b4a-0000-4000-8000-000000000002" // never got a message
	u := setting(t, closed)
	r, err := u.reopen(t)
	if err != nil {
		t.Fatal(err)
	}
	if r.Resumed || len(u.f.starts) != 0 {
		t.Fatalf("resumed %v: %q", r.Resumed, u.f.starts)
	}
	if want := []string{"CreateTab w1 Login loop " + u.project}; !slices.Equal(u.f.callsTo("CreateTab"), want) {
		t.Fatalf("CreateTab = %q", u.f.callsTo("CreateTab"))
	}
}

func TestReopenWhenItsFolderIsGone(t *testing.T) {
	u := setting(t, loginLoop())
	if err := os.RemoveAll(u.project); err != nil {
		t.Fatal(err)
	}
	r, err := u.reopen(t)
	if err != nil {
		t.Fatal(err)
	}
	if r.Gone != u.project || r.Resumed || len(u.f.starts) != 0 {
		t.Fatalf("reopened %+v, starts %q", r, u.f.starts)
	}
	if want := []string{"CreateTab w1 Login loop "}; !slices.Equal(u.f.callsTo("CreateTab"), want) {
		t.Fatalf("CreateTab = %q", u.f.callsTo("CreateTab"))
	}
}

func TestReopenTwiceSaysItsGone(t *testing.T) {
	u := setting(t, loginLoop())
	if _, err := u.reopen(t); err != nil {
		t.Fatal(err)
	}
	if _, err := u.reopen(t); !errors.Is(err, ErrGone) {
		t.Fatalf("second reopen = %v", err)
	}
	if n := len(u.f.callsTo("CreateTab")); n != 1 {
		t.Errorf("%d tabs made", n)
	}
}

func TestReopenWhenClaudeDoesntStart(t *testing.T) {
	u := setting(t, loginLoop())
	u.f.startErr = &herdr.Error{Code: "not_at_prompt", Message: "the pane isn't at a shell prompt"}
	r, err := u.reopen(t)
	if err == nil || r.Tab == "" || r.Resumed {
		t.Fatalf("reopen = %+v, %v; want the tab and the error", r, err)
	}
	s, _ := u.st.Load()
	if _, ok := s.Tasks[r.Tab]; !ok {
		t.Fatal("the task isn't reopened")
	}
}

func TestReopenContinuesLockedResearch(t *testing.T) {
	closed := loginLoop()
	closed.Slack = &task.SlackAsk{Key: "C0123ABCD/1793450000.000100", Channel: "C0123ABCD", Place: "#billing", SessionID: session, Research: task.ResearchLocked}
	u := setting(t, closed)
	r, err := u.reopen(t)
	if err != nil {
		t.Fatal(err)
	}
	s, _ := u.st.Load()
	if got := s.Tasks[r.Tab].Slack.Research; got != task.ResearchOpen {
		t.Fatalf("research = %q, want open: it runs with your tools now", got)
	}
}

func TestSay(t *testing.T) {
	for _, c := range []struct {
		r    Reopened
		want string
	}{
		{Reopened{Tab: "w1:t3", Resumed: true}, "Reopened Login loop and resumed its Claude session"},
		{Reopened{Tab: "w1:t3"}, "Reopened Login loop"},
		{Reopened{Tab: "w1:t3", Gone: "/home/dev/acme"}, "Reopened Login loop. Its folder /home/dev/acme is gone, so its Claude session wasn't resumed"},
	} {
		if got := c.r.Say("Login loop"); got != c.want {
			t.Errorf("Say = %q, want %q", got, c.want)
		}
	}
}

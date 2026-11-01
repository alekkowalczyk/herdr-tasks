package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/alekkowalczyk/herdr-tasks/internal/store"
	"github.com/alekkowalczyk/herdr-tasks/internal/task"
)

// mainArgs is set in a child test process that runs main with these
// arguments, as JSON. See runMain.
const mainArgs = "HERDR_TASKS_TEST_MAIN_ARGS"

func TestMain(m *testing.M) {
	if v, ok := os.LookupEnv(mainArgs); ok {
		var args []string
		if err := json.Unmarshal([]byte(v), &args); err != nil {
			fmt.Fprintln(os.Stderr, "bad", mainArgs, err)
			os.Exit(100)
		}
		os.Args = append([]string{"herdr-tasks"}, args...)
		main()
		os.Exit(0)
	}
	os.Exit(m.Run())
}

// runMain runs herdr-tasks with args and stdin in a child process, the test
// binary, and returns how it exited and what it printed.
func runMain(t *testing.T, stdin string, args ...string) (code int, stdout, stderr string) {
	t.Helper()
	b, err := json.Marshal(args)
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(os.Args[0])
	cmd.Env = append(os.Environ(), mainArgs+"="+string(b), "HOME="+t.TempDir(),
		"HERDR_PLUGIN_STATE_DIR="+t.TempDir(), "HERDR_PLUGIN_CONFIG_DIR="+t.TempDir())
	cmd.Stdin = strings.NewReader(stdin)
	var out, errOut bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errOut
	err = cmd.Run()
	var exit *exec.ExitError
	if err != nil && !errors.As(err, &exit) {
		t.Fatal(err)
	}
	return cmd.ProcessState.ExitCode(), out.String(), errOut.String()
}

func TestSlackGuardCommandExitsTwo(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "acme-harness")
	code, stdout, stderr := runMain(t, "not json", "slack-guard", "research", dir)
	if code != 2 || stdout != "" || !strings.HasPrefix(stderr, "herdr-tasks: ") {
		t.Errorf("bad input: exit %d, stdout %q, stderr %q; want exit 2 and the error on stderr", code, stdout, stderr)
	}
	code, _, stderr = runMain(t, `{"tool_name":"Bash","tool_input":{"command":"ls"}}`, "slack-guard", "research")
	if code != 2 || stderr == "" {
		t.Errorf("no folder: exit %d, stderr %q; want exit 2 and the error", code, stderr)
	}

	// A call it can read is decided, and the command succeeds.
	code, stdout, _ = runMain(t, `{"tool_name":"Bash","tool_input":{"command":"ls"}}`, "slack-guard", "research", dir)
	if code != 0 || !strings.Contains(stdout, `"permissionDecision":"deny"`) {
		t.Errorf("Bash: exit %d, stdout %q; want 0 and a deny", code, stdout)
	}
}

// onTask returns a change func that edits tk, as a.change edits the target
// tab's task.
func onTask(tk *task.Task) func(func(*task.Task) error) error {
	return func(fn func(*task.Task) error) error { return fn(tk) }
}

func TestAgentLinkAddOfARemovedIssueSaysSo(t *testing.T) {
	tk := &task.Task{DismissedIssues: []string{"ABC-12"}}
	var out bytes.Buffer
	if err := linkAdd(onTask(tk), &out, "https://linear.app/acme/issue/ABC-12", "", "", "claude"); err != nil {
		t.Fatal(err)
	}
	if len(tk.Links) != 0 {
		t.Errorf("links = %+v, want none", tk.Links)
	}
	if s := out.String(); !strings.Contains(s, "ABC-12 was removed from this task") || !strings.Contains(s, "herdr-tasks link add https://linear.app/acme/issue/ABC-12") {
		t.Errorf("said %q, want a note that ABC-12 stays removed", s)
	}

	// You can add it back, and nothing is said.
	out.Reset()
	if err := linkAdd(onTask(tk), &out, "https://linear.app/acme/issue/ABC-12", "", "", ""); err != nil {
		t.Fatal(err)
	}
	if len(tk.Links) != 1 || out.Len() > 0 {
		t.Errorf("links = %+v, said %q; want the link and nothing said", tk.Links, out.String())
	}
}

func TestAgentLinkAddOfARemovedPageSaysSo(t *testing.T) {
	tk := &task.Task{}
	tk.AddLink("http://localhost:4100/showcase", "", "", time.Now())
	if _, err := tk.RemoveLink("1"); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	// Another form of the same page.
	if err := linkAdd(onTask(tk), &out, "http://localhost:4100/showcase/", "", "", "claude"); err != nil {
		t.Fatal(err)
	}
	if len(tk.Links) != 0 {
		t.Errorf("links = %+v, want none", tk.Links)
	}
	if s := out.String(); !strings.Contains(s, "localhost:4100/showcase was removed from this task") || !strings.Contains(s, "herdr-tasks link add http://localhost:4100/showcase/") {
		t.Errorf("said %q, want a note that the page stays removed", s)
	}
}

func TestLinkAddTakesALocalPath(t *testing.T) {
	dir := t.TempDir()
	page := filepath.Join(dir, "docs", "plan.html")
	if err := os.MkdirAll(filepath.Dir(page), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(page, []byte("<!doctype html><title>Acme plan</title>\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	tk := &task.Task{}
	var out bytes.Buffer
	// Relative to where link add runs, then as an absolute path, which is
	// the same link.
	for _, target := range []string{"docs/plan.html", page, "acme.github.io/demo/", "docs/missing.html"} {
		if err := linkAdd(onTask(tk), &out, target, dir, "", ""); err != nil {
			t.Fatal(err)
		}
	}
	var got []string
	for _, l := range tk.Links {
		got = append(got, l.URL)
	}
	want := []string{task.FileURL(page), "https://acme.github.io/demo/", "https://docs/missing.html"}
	if !slices.Equal(got, want) || out.Len() > 0 {
		t.Errorf("links = %q, said %q; want %q and nothing said", got, out.String(), want)
	}
}

func TestLinkAddOfALinkedIssueSaysNothing(t *testing.T) {
	tk := &task.Task{Links: []task.Link{{URL: "https://linear.app/acme/issue/ABC-12"}}}
	var out bytes.Buffer
	if err := linkAdd(onTask(tk), &out, "https://linear.app/acme/issue/ABC-12/fix-login", "", "", "claude"); err != nil {
		t.Fatal(err)
	}
	if len(tk.Links) != 1 || out.Len() > 0 {
		t.Errorf("links = %+v, said %q; want one link and nothing said", tk.Links, out.String())
	}
}

func TestShowPrintsAgentTextCleaned(t *testing.T) {
	tk := &task.Task{
		TabID: "w1:t1", Workspace: "Acme\x1b[2J", Stage: task.Paused, Since: time.Now(),
		Title:  "Checkout\x1b]0;owned\x07",
		Reason: "QA \x1b[2J\x07sign-off",
		Note:   "hi \x1b]0;owned\x07 there\nsecond\tline",
		Log:    []task.Entry{{Who: "claude\x1b]52;c;aGVsbG8=\x07", At: time.Now(), Text: "x \x1b[2J y\u009b"}},
		Links: []task.Link{
			{URL: "https://acme.github.io/demo/", Label: "Demo\x1b[2J", By: "claude\x1b]0;x\x07"},
			{URL: "https://evil.example/\x1b]52;c;aGVsbG8=\x07"},
			{URL: "https://evil.example/%1b]0;x", Label: "Plan"},
		},
	}
	var out bytes.Buffer
	printTask(&out, tk, tk.TabID, "", 1, time.Now())
	s := out.String()
	for _, bad := range []string{"\x1b", "\x07", "\u009b", "evil.example"} {
		if strings.Contains(s, bad) {
			t.Errorf("show printed %q:\n%q", bad, s)
		}
	}
	for _, want := range []string{
		"Checkout]0;owned  (Acme[2J · w1:t1)",
		"until QA [2Jsign-off",
		"  hi ]0;owned there\n  second line\n",
		"claude]52;c;aGVsbG8=",
		"x [2J y\n",
		"1. Demo[2J  https://acme.github.io/demo/  (via claude]0;x)\n",
		"2. unsafe link\n",
		"3. Plan  unsafe link\n",
	} {
		if !strings.Contains(s, want) {
			t.Errorf("show didn't print %q:\n%s", want, s)
		}
	}
}

func TestShowNumbersLinksAsLinkRmTakesThem(t *testing.T) {
	tk := &task.Task{TabID: "w1:t1", Stage: task.Active, Since: time.Now(), Links: []task.Link{
		{URL: "http://localhost:4100/showcase"},
		{URL: "https://linear.app/acme/issue/ABC-12"},
	}}
	var out bytes.Buffer
	printTask(&out, tk, tk.TabID, "", 1, time.Now())
	if s := out.String(); !strings.Contains(s, "1. ABC-12  ") || !strings.Contains(s, "2. localhost:4100/showcase  ") {
		t.Errorf("show numbers the links in stored order:\n%s", s)
	}
	if l, err := tk.RemoveLink("1"); err != nil || l.URL != "https://linear.app/acme/issue/ABC-12" {
		t.Errorf("link rm 1 = %+v, %v; want ABC-12, shown as 1", l, err)
	}
}

func TestLinkAddRefusesWhatItCantSave(t *testing.T) {
	for _, target := range []string{"https://example.com/100%done", "https://example.com/a#100%", "https://example.com/%0A", "https://example.com/\x1b[2J", "foo bar"} {
		for _, by := range []string{"", "claude"} {
			tk := &task.Task{}
			var out bytes.Buffer
			err := linkAdd(onTask(tk), &out, target, "", "", by)
			if err == nil {
				t.Errorf("link add %q as %q: no error", target, by)
			} else if strings.ContainsAny(err.Error(), "\x1b\n") {
				t.Errorf("link add %q: the error has a control character: %q", target, err)
			}
			if len(tk.Links) != 0 {
				t.Errorf("link add %q as %q: links = %+v, want none", target, by, tk.Links)
			}
		}
	}
	// A % in the query is fine, as url.Parse leaves it alone.
	tk := &task.Task{}
	if err := linkAdd(onTask(tk), io.Discard, "https://example.com/?q=50%off", "", "", ""); err != nil || len(tk.Links) != 1 {
		t.Errorf("link add with a %% in the query = %v, links %+v; want it added", err, tk.Links)
	}
}

// A command in a moved pane has the pane's and tab's IDs from before the
// move; it applies to the tab the pane is in now.
func TestTargetTabFollowsAMovedPane(t *testing.T) {
	tab, st, c := movedTask(t, &task.Task{TabID: "w1:t1", Workspace: "infra", Title: "Reply to Dana"})
	t.Setenv("HERDR_PANE_ID", "w1:p2")
	t.Setenv("HERDR_TAB_ID", "w1:t1")
	t.Setenv("HERDR_ACTIVE_TAB_ID", "")
	a := &app{client: c, store: st}
	if got, err := a.targetTab(); err != nil || got != tab {
		t.Errorf("targetTab = %q, %v; want %s", got, err, tab)
	}
}

// The tab bar previews the link the strip and show list first: an issue
// before a page added earlier.
func TestStatusLinePreviewsTheFirstLinkShown(t *testing.T) {
	t.Setenv("HERDR_PLUGIN_STATE_DIR", t.TempDir())
	t.Setenv("HERDR_ACTIVE_TAB_ID", "w1:t1")
	st := seed(t, map[string]*task.Task{"w1:t1": {TabID: "w1:t1", Stage: task.Active, Links: []task.Link{
		{URL: "http://localhost:4100/demo", Label: "Demo", By: "claude"},
		{URL: "https://linear.app/acme/issue/ABC-12/fix-login"},
	}}})
	a := &app{store: st}
	line, err := a.statusLine()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasSuffix(line, "↗ ABC-12") {
		t.Errorf("status line = %q, want it to end in ↗ ABC-12", line)
	}
}

func TestMoveCommand(t *testing.T) {
	code, stdout, stderr := runMain(t, "", "move")
	if code == 0 || stdout != "" || !strings.Contains(stderr, "move <workspace>") {
		t.Errorf("move with no name: exit %d, stdout %q, stderr %q; want a failure naming move <workspace>", code, stdout, stderr)
	}
}

func TestMoveSaysWhereItWent(t *testing.T) {
	t.Setenv("HERDR_PLUGIN_STATE_DIR", t.TempDir())
	st := store.New("test")
	if err := st.Update(func(s *store.State) error {
		s.Tasks = map[string]*task.Task{"w2:n1": {TabID: "w2:n1", Workspace: "billing"}}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if err := moveAndSay(&out, st, "BILLING", func() (string, error) { return "w2:n1", nil }); err != nil || out.String() != "herdr-tasks: moved to billing\n" {
		t.Errorf("said %q, err %v; want the confirmation with the stored label", out.String(), err)
	}
	// A partial move still says where it went, then returns the error.
	out.Reset()
	boom := errors.New("Moved 1 of 2 panes")
	if err := moveAndSay(&out, st, "billing", func() (string, error) { return "w2:n1", boom }); err != boom || out.Len() == 0 {
		t.Errorf("said %q, err %v; want the confirmation and the error", out.String(), err)
	}
	// Nothing moved: no confirmation.
	out.Reset()
	if err := moveAndSay(&out, st, "billing", func() (string, error) { return "", boom }); err != boom || out.Len() != 0 {
		t.Errorf("said %q, err %v; want only the error", out.String(), err)
	}
}

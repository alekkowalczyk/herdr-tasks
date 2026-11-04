package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"net"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/alekkowalczyk/herdr-tasks/internal/claudehook"
	"github.com/alekkowalczyk/herdr-tasks/internal/herdr"
	"github.com/alekkowalczyk/herdr-tasks/internal/move"
	"github.com/alekkowalczyk/herdr-tasks/internal/reconcile"
	"github.com/alekkowalczyk/herdr-tasks/internal/slack"
	"github.com/alekkowalczyk/herdr-tasks/internal/store"
	"github.com/alekkowalczyk/herdr-tasks/internal/task"
)

// withConfig points the plugin's settings at a temp dir holding config.toml,
// or no file when config is "".
func withConfig(t *testing.T, config string) {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("HERDR_PLUGIN_CONFIG_DIR", dir)
	if config != "" {
		if err := os.WriteFile(filepath.Join(dir, "config.toml"), []byte(config), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

// hookInput is one of claudehook's invented hook payloads.
func hookInput(t *testing.T, name string) io.Reader {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("..", "..", "internal", "claudehook", "testdata", name))
	if err != nil {
		t.Fatal(err)
	}
	return bytes.NewReader(b)
}

func inHerdr(name string) string {
	if name == "HERDR_PANE_ID" {
		return "w1:p1"
	}
	return ""
}

func outsideHerdr(string) string { return "" }

type linked struct {
	pane string
	f    found
}

// recorder returns a link func that records its calls in got.
func recorder(got *[]linked) func(string, found) error {
	return func(pane string, f found) error {
		*got = append(*got, linked{pane, f})
		return nil
	}
}

// issue is what a Linear tool call finds.
func issue(id, url string) found { return found{issue: &claudehook.Issue{ID: id, URL: url}} }

// pages is what a hook call that finds links gives.
func pages(links ...claudehook.Link) found { return found{links: links} }

const anArtifact = "https://claude.ai/code/artifact/00000000-0000-4000-8000-000000000001"

func TestClaudeHookIgnoresPanesOutsideHerdr(t *testing.T) {
	withConfig(t, "claude_links = true\n")
	var got []linked
	if err := claudeHook(hookInput(t, "get_issue.json"), outsideHerdr, t.TempDir(), recorder(&got)); err != nil || len(got) > 0 {
		t.Errorf("claudeHook = %v, linked %v; want nothing outside herdr", err, got)
	}
}

func TestClaudeHookOffByDefault(t *testing.T) {
	withConfig(t, "")
	var got []linked
	if err := claudeHook(hookInput(t, "get_issue.json"), inHerdr, t.TempDir(), recorder(&got)); err != nil || len(got) > 0 {
		t.Errorf("claudeHook = %v, linked %v; want nothing while claude_links is off", err, got)
	}
}

func TestClaudeHookLinksTheIssue(t *testing.T) {
	withConfig(t, "claude_links = true\n")
	var got []linked
	if err := claudeHook(hookInput(t, "get_issue.json"), inHerdr, t.TempDir(), recorder(&got)); err != nil {
		t.Fatal(err)
	}
	want := claudehook.Issue{ID: "ABC-12", URL: "https://linear.app/acme/issue/ABC-12/fix-login"}
	if len(got) != 1 || got[0].pane != "w1:p1" || got[0].f.issue == nil || *got[0].f.issue != want || len(got[0].f.links) > 0 {
		t.Errorf("linked %+v, want %+v in w1:p1", got, want)
	}
}

func TestClaudeHookAddsSlackAndPages(t *testing.T) {
	withConfig(t, "claude_links = true\n")
	var got []linked
	if err := claudeHook(hookInput(t, "links/prompt.json"), inHerdr, t.TempDir(), recorder(&got)); err != nil {
		t.Fatal(err)
	}
	want := []claudehook.Link{
		{URL: "https://acme.slack.com/archives/C0123ABCD/p1700000000123456?thread_ts=1700000000.000001&cid=C0123ABCD"},
		{URL: "https://acme.enterprise.slack.com/archives/C0123ABCD"},
		{URL: "https://linear.app/acme/issue/ABC-12/fix-login"},
		{URL: "http://localhost:4100/showcase/"},
		{URL: "http://[::1]"},
		{URL: anArtifact + "?sk=abc"},
		{URL: "https://acme.github.io/demo/"},
		{URL: "https://github.com/acme/x/pull/1"},
	}
	if len(got) != 1 || got[0].pane != "w1:p1" || got[0].f.issue != nil || !slices.Equal(got[0].f.links, want) {
		t.Errorf("linked %+v, want once with %+v", got, want)
	}
}

// searchCall is a Slack search's PostToolUse, whose result's text is results
// as Slack's connector gives them: a JSON object holding the markdown.
func searchCall(t *testing.T, tool, results string) io.Reader {
	t.Helper()
	text, err := json.Marshal(map[string]string{"results": results, "pagination_info": "No more results."})
	if err != nil {
		t.Fatal(err)
	}
	b, err := json.Marshal(map[string]any{
		"hook_event_name": "PostToolUse",
		"cwd":             "/home/u/acme",
		"tool_name":       tool,
		"tool_input":      map[string]any{"keywords": []string{"invoice"}, "response_format": "detailed"},
		"tool_response":   []map[string]string{{"type": "text", "text": string(text)}},
	})
	if err != nil {
		t.Fatal(err)
	}
	return bytes.NewReader(b)
}

// searchResults are three messages, as a detailed search shows them: two in
// #billing and a DM.
const searchResults = `# Search Results

### Result 1 of 4
Channel: #billing (ID: C0123ABCD)
From: Dana Reed <dana@acme.test> (ID: U0DANA001)
Time: 2026-10-31 14:32:10 CET
Message_ts: 1793454730.123456
Permalink: [link](https://acme.slack.com/archives/C0123ABCD/p1793454730123456?thread_ts=1793450000.000100&cid=C0123ABCD)
Text: <@U0ME00001> can the invoice fix ship Friday?

---

### Result 2 of 4
Channel: #billing (ID: C0123ABCD)
From: Rui Costa <rui@acme.test> (ID: U0RUI0001)
Time: 2026-10-31 14:20:00 CET
Message_ts: 1793454000.000100
Permalink: [link](https://acme.slack.com/archives/C0123ABCD/p1793454000000100)
Text: is the refund note in?

---

### Result 3 of 4
Channel: #billing (ID: C0123ABCD)
From: Dana Reed <dana@acme.test> (ID: U0DANA001)
Time: 2026-10-31 14:36:40 CET
Message_ts: 1793455000.000300
Permalink: [link](https://acme.slack.com/archives/C0123ABCD/p1793455000000300?thread_ts=1793450000.000100&cid=C0123ABCD)
Text: and the refund
note too?

---

### Result 4 of 4
Channel: DM (ID: D0RUI0001)
Participants: Rui Costa (ID: U0RUI0001), You (ID: U0ME00001)
From: Rui Costa <rui@acme.test> (ID: U0RUI0001)
Time: 2026-10-31 14:33:20 CET
Message_ts: 1793454800.000200
Permalink: [link](https://acme.slack.com/archives/D0RUI0001/p1793454800000200)
Text: quick q: is ABC-12 merged?

---
`

func TestSearchResultLabelsLinks(t *testing.T) {
	withConfig(t, "claude_links = true\n")
	const (
		dana  = "https://acme.slack.com/archives/C0123ABCD/p1793454730123456"
		rui   = "https://acme.slack.com/archives/C0123ABCD/p1793454000000100"
		yours = "https://acme.enterprise.slack.com/archives/C0123ABCD/p1793455000000300"
	)
	st := seed(t, map[string]*task.Task{"w1:t1": {TabID: "w1:t1", Links: []task.Link{
		{URL: dana, By: "claude"},
		{URL: rui, Label: "the refund question", By: "claude"},
		{URL: yours},
		{URL: "https://acme.slack.com/archives/C0123ABCD", By: "claude"},
	}}})
	link := func(_ string, f found) error { return addFound(st, "w1:t1", f) }
	if err := claudeHook(searchCall(t, "mcp__plugin_slack_slack__slack_search_public_and_private", searchResults), inHerdr, st.Dir, link); err != nil {
		t.Fatal(err)
	}
	want := []string{
		dana + " |  | #billing · Dana Reed: can the invoice fix ship Friday? | claude",
		rui + " |  | the refund question | claude",
		yours + " |  | #billing · Dana Reed: and the refund note too? | ",
		"https://acme.slack.com/archives/C0123ABCD |  |  | claude",
	}
	if got := summary(linksOf(t, st, "w1:t1")); !slices.Equal(got, want) {
		t.Errorf("links =\n%q\nwant\n%q", got, want)
	}
	names, err := slack.Channels(st.Dir)
	if err != nil {
		t.Fatal(err)
	}
	if want := map[string]string{"C0123ABCD": "#billing"}; !maps.Equal(names, want) {
		t.Errorf("channels = %v, want %v", names, want)
	}

	// A result that doesn't parse does nothing, and says nothing.
	for name, in := range map[string]io.Reader{
		"not JSON": bytes.NewReader([]byte(`{"hook_event_name":"PostToolUse","tool_name":"mcp__claude_ai_Slack__slack_search_public","tool_response":[{"type":"text","text":"Search failed"}]}`)),
		"layout":   searchCall(t, "mcp__claude_ai_Slack__slack_search_public", "Message_ts: 1793454730.123456\nText: hi"),
		"empty":    searchCall(t, "mcp__claude_ai_Slack__slack_search_public", "No results."),
	} {
		var got []linked
		dir := t.TempDir()
		quietly(dir, func() error { return claudeHook(in, inHerdr, dir, recorder(&got)) })
		if len(got) > 0 {
			t.Errorf("%s: linked %+v, want nothing", name, got)
		}
		if entries, _ := os.ReadDir(dir); len(entries) > 0 {
			t.Errorf("%s: wrote %s to the state folder, want nothing", name, entries[0].Name())
		}
	}
}

// A link from your prompt is labelled with its channel's name alone. A
// search's label takes its place; any other label stays.
func TestSearchLabelReplacesAChannelOnlyLabel(t *testing.T) {
	withConfig(t, "claude_links = true\n")
	const (
		dana  = "https://acme.slack.com/archives/C0123ABCD/p1793454730123456"
		rui   = "https://acme.slack.com/archives/C0123ABCD/p1793454000000100"
		later = "https://acme.slack.com/archives/C0123ABCD/p1793455000000300"
		dm    = "https://acme.slack.com/archives/D0RUI0001/p1793454800000200"
	)
	st := seed(t, map[string]*task.Task{"w1:t1": {TabID: "w1:t1", Links: []task.Link{
		{URL: dana, Label: "#billing", By: "claude"},
		{URL: rui, Label: "Invoice thread", By: "claude"},
		{URL: later, Label: "billing", By: "claude"},
		{URL: dm, Label: "#billing", By: "claude"},
	}}})
	link := func(_ string, f found) error { return addFound(st, "w1:t1", f) }
	if err := claudeHook(searchCall(t, "mcp__plugin_slack_slack__slack_search_public_and_private", searchResults), inHerdr, st.Dir, link); err != nil {
		t.Fatal(err)
	}
	want := []string{
		dana + " |  | #billing · Dana Reed: can the invoice fix ship Friday? | claude",
		rui + " |  | Invoice thread | claude",
		later + " |  | billing | claude",
		dm + " |  | #billing | claude",
	}
	if got := summary(linksOf(t, st, "w1:t1")); !slices.Equal(got, want) {
		t.Errorf("links =\n%q\nwant\n%q", got, want)
	}
}

// movingHerdr is a herdr session with two workspaces and a tab in the
// first, whose panes a move gives new IDs, as herdr does. Its socket, at
// HERDR_SOCKET_PATH, answers pane.get, which the hook and targetTab ask; a
// move calls its methods.
type movingHerdr struct {
	mu    sync.Mutex
	tabs  []herdr.Tab
	panes map[string]string // pane ID -> its tab's ID
	next  int
}

func newMovingHerdr(t *testing.T) (*movingHerdr, *herdr.Client) {
	t.Helper()
	h := &movingHerdr{
		tabs:  []herdr.Tab{{ID: "w1:t1", WorkspaceID: "w1", Label: "Reply to Dana"}},
		panes: map[string]string{"w1:p1": "w1:t1", "w1:p2": "w1:t1"},
	}
	// t.TempDir paths can pass the unix socket path limit on macOS.
	dir, err := os.MkdirTemp("", "herdr")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	path := filepath.Join(dir, "herdr.sock")
	ln, err := net.Listen("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go h.answer(conn)
		}
	}()
	t.Setenv("HERDR_SOCKET_PATH", path)
	return h, &herdr.Client{SocketPath: path}
}

func (h *movingHerdr) answer(conn net.Conn) {
	defer conn.Close()
	var req struct {
		Method string
		Params struct {
			PaneID string `json:"pane_id"`
		}
	}
	if err := json.NewDecoder(conn).Decode(&req); err != nil {
		return
	}
	resp := `{"id":"herdr-tasks","result":{}}`
	if req.Method == "pane.get" {
		h.mu.Lock()
		tab, ok := h.panes[req.Params.PaneID]
		h.mu.Unlock()
		resp = `{"id":"herdr-tasks","error":{"code":"pane_not_found","message":"no pane ` + req.Params.PaneID + `"}}`
		if ok {
			resp = `{"id":"herdr-tasks","result":{"pane":{"pane_id":"` + req.Params.PaneID + `","tab_id":"` + tab + `"}}}`
		}
	}
	_, _ = conn.Write([]byte(resp + "\n"))
}

func (h *movingHerdr) Live() (*herdr.Live, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	return &herdr.Live{
		Workspaces: []herdr.Workspace{{ID: "w1", Label: "infra", Number: 1}, {ID: "w2", Label: "billing", Number: 2}},
		Tabs:       slices.Clone(h.tabs),
	}, nil
}
func (h *movingHerdr) RenameTab(string, string) error                         { return nil }
func (h *movingHerdr) MoveTab(string, int, int) ([]herdr.Tab, error)          { return nil, nil }
func (h *movingHerdr) ReportWorkspaceTokens(string, map[string]*string) error { return nil }

func (h *movingHerdr) Layout(tab string) (*herdr.Node, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	var n *herdr.Node
	for _, p := range slices.Sorted(maps.Keys(h.panes)) {
		if h.panes[p] != tab {
			continue
		}
		leaf := &herdr.Node{Type: "pane", PaneID: p}
		if n == nil {
			n = leaf
		} else {
			n = &herdr.Node{Type: "split", First: n, Second: leaf}
		}
	}
	if n == nil {
		return nil, fmt.Errorf("no tab %s", tab)
	}
	return n, nil
}

// moved gives pane a new ID in tab.
func (h *movingHerdr) moved(pane, tab string) herdr.Moved {
	delete(h.panes, pane)
	h.next++
	np := fmt.Sprintf("%s:np%d", strings.SplitN(tab, ":", 2)[0], h.next)
	h.panes[np] = tab
	return herdr.Moved{PaneID: np, TabID: tab}
}

func (h *movingHerdr) MovePaneToNewTab(pane, ws, label string) (herdr.Moved, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	tab := fmt.Sprintf("%s:n%d", ws, h.next+1)
	h.tabs = append(h.tabs, herdr.Tab{ID: tab, WorkspaceID: ws, Label: label})
	return h.moved(pane, tab), nil
}

func (h *movingHerdr) MovePaneNextTo(pane, tab, _, _ string, _ float64) (herdr.Moved, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.moved(pane, tab), nil
}

func (h *movingHerdr) CloseTab(id string) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.tabs = slices.DeleteFunc(h.tabs, func(t herdr.Tab) bool { return t.ID == id })
	maps.DeleteFunc(h.panes, func(_, tab string) bool { return tab == id })
	return nil
}

func (h *movingHerdr) Panes() ([]herdr.Pane, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	var out []herdr.Pane
	for p, tab := range h.panes {
		out = append(out, herdr.Pane{ID: p, TabID: tab})
	}
	return out, nil
}

// movedTask is a task in w1:t1, moved to billing, whose panes had the IDs
// in the env inHerdr gives. It returns the task's new tab and its store.
func movedTask(t *testing.T, tk *task.Task) (string, *store.Store, *herdr.Client) {
	t.Helper()
	t.Setenv("HOME", t.TempDir())
	t.Setenv("HERDR_PLUGIN_STATE_DIR", t.TempDir())
	h, c := newMovingHerdr(t)
	st := seedIn(t, store.Dir(), herdr.SessionKey(c.SocketPath), map[string]*task.Task{"w1:t1": tk})
	tab, err := move.Task(h, st, reconcile.Options{}, time.Now(), "w1:t1", "billing")
	if err != nil {
		t.Fatal(err)
	}
	if tab == "w1:t1" || tab == "" {
		t.Fatalf("moved to %q", tab)
	}
	return tab, st, c
}

// Claude knows its pane by the ID it had when Claude started. After a move,
// the hook still finds its task, and labels and adds links there.
func TestClaudeHookFindsItsTaskAfterAMove(t *testing.T) {
	withConfig(t, "claude_links = true\n")
	const dana = "https://acme.slack.com/archives/C0123ABCD/p1793454730123456"
	tab, st, _ := movedTask(t, &task.Task{TabID: "w1:t1", Workspace: "infra", Title: "Reply to Dana", Links: []task.Link{{URL: dana, By: "claude"}}})
	for _, in := range []io.Reader{
		searchCall(t, "mcp__plugin_slack_slack__slack_search_public_and_private", searchResults),
		hookInput(t, "get_issue.json"),
	} {
		if err := claudeHook(in, inHerdr, st.Dir, linkFound); err != nil {
			t.Fatal(err)
		}
	}
	want := []string{
		dana + " |  | #billing · Dana Reed: can the invoice fix ship Friday? | claude",
		"https://linear.app/acme/issue/ABC-12/fix-login |  |  | claude",
	}
	if got := summary(linksOf(t, st, tab)); !slices.Equal(got, want) {
		t.Errorf("links in %s =\n%q\nwant\n%q", tab, got, want)
	}
}

// The hook links what it found first, then saves the channels' names: the
// links are what the hook is for, and saving the names waits on a lock.
func TestClaudeHookLinksBeforeLearningChannels(t *testing.T) {
	withConfig(t, "claude_links = true\n")
	dir := t.TempDir()
	var during map[string]string
	linked := false
	link := func(string, found) error {
		linked = true
		during, _ = slack.Channels(dir)
		return errors.New("no task for tab w1:t1")
	}
	err := claudeHook(searchCall(t, "mcp__plugin_slack_slack__slack_search_public_and_private", searchResults), inHerdr, dir, link)
	if !linked || err == nil || err.Error() != "no task for tab w1:t1" {
		t.Errorf("linked %v, err %v; want the link's error", linked, err)
	}
	if len(during) != 0 {
		t.Errorf("channels saved before linking: %v", during)
	}
	after, err := slack.Channels(dir)
	if want := map[string]string{"C0123ABCD": "#billing"}; err != nil || !maps.Equal(after, want) {
		t.Errorf("channels = %v, %v; want %v saved after the link failed", after, err, want)
	}
}

func TestClaudeHookSkipsHugeInput(t *testing.T) {
	withConfig(t, "claude_links = true\n")
	dir := t.TempDir()
	// Claude Code sends the whole file a Write wrote.
	in := `{"hook_event_name":"PostToolUse","tool_name":"Write","tool_input":{"file_path":"/tmp/acme/big.html","content":"<!doctype html>` +
		strings.Repeat("x", 9<<20) + `"}}`
	var got []linked
	quietly(dir, func() error { return claudeHook(strings.NewReader(in), inHerdr, t.TempDir(), recorder(&got)) })
	if len(got) > 0 {
		t.Errorf("linked %d times, want none", len(got))
	}
	if _, err := os.Stat(filepath.Join(dir, "claude-hook.log")); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("wrote a log (%v), want none", err)
	}
}

func TestClaudeHookIgnoresOtherTools(t *testing.T) {
	withConfig(t, "claude_links = true\n")
	for _, name := range []string{"bash.json", "list_issues.json", "save_comment_edit.json"} {
		var got []linked
		if err := claudeHook(hookInput(t, name), inHerdr, t.TempDir(), recorder(&got)); err != nil || len(got) > 0 {
			t.Errorf("%s: claudeHook = %v, linked %v; want nothing", name, err, got)
		}
	}
}

func TestClaudeHookReportsInputItCantRead(t *testing.T) {
	withConfig(t, "claude_links = true\n")
	var got []linked
	if err := claudeHook(strings.NewReader(`{"tool_name":`), inHerdr, t.TempDir(), recorder(&got)); err == nil || len(got) > 0 {
		t.Errorf("claudeHook = %v, linked %v; want an error and nothing linked", err, got)
	}
}

func TestOrgIsTheMostCommonOne(t *testing.T) {
	state := func(urls ...string) *store.State {
		s := &store.State{Tasks: map[string]*task.Task{}}
		for i, u := range urls {
			// Spread the links over two tasks.
			id := []string{"w1:t1", "w1:t2"}[i%2]
			if s.Tasks[id] == nil {
				s.Tasks[id] = &task.Task{TabID: id}
			}
			s.Tasks[id].Links = append(s.Tasks[id].Links, task.Link{URL: u})
		}
		return s
	}
	cases := []struct {
		name string
		s    *store.State
		want string
	}{
		{"most common", state(
			"https://linear.app/zeta/issue/Z-1",
			"https://linear.app/acme/issue/ABC-1/first",
			"https://github.com/zeta/api/pull/3",
			"https://linear.app/acme/issue/ABC-2",
			"https://linear.app/zeta/project/roadmap-1",
		), "acme"},
		{"tie", state("https://linear.app/zeta/issue/Z-1", "https://linear.app/beta/issue/B-1"), "beta"},
		{"none", state("https://github.com/acme/api/pull/31"), ""},
		{"empty", &store.State{}, ""},
	}
	for _, c := range cases {
		if got := orgFor(c.s); got != c.want {
			t.Errorf("%s: orgFor = %q, want %q", c.name, got, c.want)
		}
	}
}

// seed saves tasks to a new store in a temp dir.
func seed(t *testing.T, tasks map[string]*task.Task) *store.Store {
	t.Helper()
	return seedIn(t, t.TempDir(), "test", tasks)
}

// seedIn saves tasks to the store of session in dir.
func seedIn(t *testing.T, dir, session string, tasks map[string]*task.Task) *store.Store {
	t.Helper()
	st := &store.Store{Dir: dir, Session: session}
	if err := st.Update(func(s *store.State) error { s.Tasks = tasks; return nil }); err != nil {
		t.Fatal(err)
	}
	return st
}

func linksOf(t *testing.T, st *store.Store, tab string) []task.Link {
	t.Helper()
	s, err := st.Load()
	if err != nil {
		t.Fatal(err)
	}
	return s.Tasks[tab].Links
}

func TestAddIssueBuildsTheURLFromTheOrg(t *testing.T) {
	st := seed(t, map[string]*task.Task{
		"w1:t1": {TabID: "w1:t1", Links: []task.Link{{URL: "https://linear.app/acme/issue/ABC-7/old-bug"}}},
		"w1:t2": {TabID: "w1:t2"},
	})
	if err := addFound(st, "w1:t2", issue("ABC-12", "")); err != nil {
		t.Fatal(err)
	}
	// Already linked, under another URL form.
	if err := addFound(st, "w1:t2", issue("ABC-12", "https://linear.app/acme/issue/ABC-12/fix-login")); err != nil {
		t.Fatal(err)
	}
	l := linksOf(t, st, "w1:t2")
	if len(l) != 1 || l[0].URL != "https://linear.app/acme/issue/ABC-12" || l[0].By != "claude" {
		t.Errorf("links = %+v, want one link to ABC-12 by claude", l)
	}
}

func TestAddIssueNeedsAnOrgForABareID(t *testing.T) {
	st := seed(t, map[string]*task.Task{"w1:t1": {TabID: "w1:t1"}})
	// A remembered workspace that no Linear URL could hold doesn't count.
	if err := os.WriteFile(filepath.Join(st.Dir, orgFile), []byte("acme/issue\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	err := addFound(st, "w1:t1", issue("ABC-12", ""))
	if !errors.Is(err, errNoOrg) || !strings.Contains(err.Error(), "ABC-12") {
		t.Errorf("addFound = %v, want errNoOrg naming ABC-12", err)
	}
	if l := linksOf(t, st, "w1:t1"); len(l) > 0 {
		t.Errorf("links = %+v, want none", l)
	}
}

func TestAddIssueRemembersTheOrg(t *testing.T) {
	st := seed(t, map[string]*task.Task{"w1:t1": {TabID: "w1:t1"}})
	if err := addFound(st, "w1:t1", issue("ABC-12", "https://linear.app/acme/issue/ABC-12/fix-login")); err != nil {
		t.Fatal(err)
	}
	if got := readOrg(st.Dir); got != "acme" {
		t.Errorf("remembered %q, want acme", got)
	}

	// Links saved before the hook remembered anything count too, even when
	// the issue is linked already.
	st = seed(t, map[string]*task.Task{
		"w1:t1": {TabID: "w1:t1", Links: []task.Link{{URL: "https://linear.app/zeta/issue/Z-7"}}},
	})
	if err := addFound(st, "w1:t1", issue("Z-7", "")); err != nil {
		t.Fatal(err)
	}
	if got := readOrg(st.Dir); got != "zeta" {
		t.Errorf("remembered %q, want zeta", got)
	}
}

func TestAddFoundRemembersTheOrgOfAPastedIssue(t *testing.T) {
	st := seed(t, map[string]*task.Task{"w1:t1": {TabID: "w1:t1"}})
	if err := addFound(st, "w1:t1", pages(claudehook.Link{URL: "https://linear.app/acme/issue/ABC-12/fix-login"})); err != nil {
		t.Fatal(err)
	}
	if got := readOrg(st.Dir); got != "acme" {
		t.Errorf("remembered %q, want acme", got)
	}
}

func TestAddFoundLeavesYourLinksAlone(t *testing.T) {
	const src = "/tmp/x/p.html"
	yours := []task.Link{{URL: anArtifact}, {URL: task.FileURL(src)}}
	st := seed(t, map[string]*task.Task{"w1:t1": {TabID: "w1:t1", Links: yours}})
	if err := addFound(st, "w1:t1", pages(claudehook.Link{URL: anArtifact, Label: "Acme plan", Source: src})); err != nil {
		t.Fatal(err)
	}
	if got, want := summary(linksOf(t, st, "w1:t1")), summary(yours); !slices.Equal(got, want) {
		t.Errorf("links = %q, want yours as they were: %q", got, want)
	}
}

func TestAddIssueUsesTheRememberedOrg(t *testing.T) {
	dir := t.TempDir()
	one := seedIn(t, dir, "one", map[string]*task.Task{
		"w1:t1": {TabID: "w1:t1", Links: []task.Link{{URL: "https://linear.app/acme/issue/ABC-7/old-bug"}}},
	})
	if err := addFound(one, "w1:t1", issue("ABC-7", "https://linear.app/acme/issue/ABC-7/old-bug")); err != nil {
		t.Fatal(err)
	}
	// Another herdr session, with no Linear links of its own.
	two := seedIn(t, dir, "two", map[string]*task.Task{"w2:t1": {TabID: "w2:t1"}})
	if err := addFound(two, "w2:t1", issue("ABC-12", "")); err != nil {
		t.Fatal(err)
	}
	if l := linksOf(t, two, "w2:t1"); len(l) != 1 || l[0].URL != "https://linear.app/acme/issue/ABC-12" {
		t.Errorf("links = %+v, want one link to https://linear.app/acme/issue/ABC-12", l)
	}
}

func TestAddIssueKeepsDismissedIssuesOff(t *testing.T) {
	st := seed(t, map[string]*task.Task{"w1:t1": {TabID: "w1:t1", DismissedIssues: []string{"ABC-12"}}})
	if err := addFound(st, "w1:t1", issue("ABC-12", "https://linear.app/acme/issue/ABC-12")); err != nil {
		t.Fatal(err)
	}
	if l := linksOf(t, st, "w1:t1"); len(l) > 0 {
		t.Errorf("links = %+v, want none for a dismissed issue", l)
	}
}

// The assistant names the links of other tasks, so its tab gets none.
func TestAddFoundLeavesTheAssistantAlone(t *testing.T) {
	st := seed(t, map[string]*task.Task{"w2:t1": {TabID: "w2:t1", Stage: task.Tool, Assistant: &task.Assistant{SessionID: "s"}}})
	f := pages(claudehook.Link{URL: "https://github.com/acme/web/pull/12"})
	f.issue = &claudehook.Issue{ID: "ABC-12", URL: "https://linear.app/acme/issue/ABC-12"}
	if err := addFound(st, "w2:t1", f); err != nil {
		t.Fatal(err)
	}
	if l := linksOf(t, st, "w2:t1"); len(l) > 0 {
		t.Errorf("links = %+v, want none on the assistant's tab", l)
	}
}

func TestAddIssueNeedsATask(t *testing.T) {
	st := seed(t, map[string]*task.Task{"w1:t1": {TabID: "w1:t1"}})
	err := addFound(st, "w1:t9", issue("ABC-12", "https://linear.app/acme/issue/ABC-12"))
	if err == nil || !strings.Contains(err.Error(), "no task for tab w1:t9") {
		t.Errorf("addFound = %v, want no task for tab w1:t9", err)
	}
}

func TestAddIssueLeavesTasksAloneAfterRemove(t *testing.T) {
	st := seed(t, map[string]*task.Task{"w1:t1": {TabID: "w1:t1"}})
	if err := st.Update(func(s *store.State) error { s.Removed = true; return nil }); err != nil {
		t.Fatal(err)
	}
	if err := addFound(st, "w1:t1", issue("ABC-12", "https://linear.app/acme/issue/ABC-12")); err != nil {
		t.Fatal(err)
	}
	if l := linksOf(t, st, "w1:t1"); len(l) > 0 {
		t.Errorf("links = %+v, want none after herdr-tasks remove", l)
	}
}

func TestAddIssueLeavesTasksAloneWhenRemovedMidway(t *testing.T) {
	st := seed(t, map[string]*task.Task{"w1:t1": {TabID: "w1:t1"}})
	// herdr-tasks remove runs after the hook has read the state.
	afterLoad = func() {
		if err := st.Update(func(s *store.State) error { s.Removed = true; return nil }); err != nil {
			t.Error(err)
		}
	}
	t.Cleanup(func() { afterLoad = func() {} })
	if err := addFound(st, "w1:t1", issue("ABC-12", "https://linear.app/acme/issue/ABC-12")); err != nil {
		t.Fatal(err)
	}
	if l := linksOf(t, st, "w1:t1"); len(l) > 0 {
		t.Errorf("links = %+v, want none after herdr-tasks remove", l)
	}
}

func readLog(t *testing.T, dir string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(dir, "claude-hook.log"))
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestClaudeHookGivesUpAtTheDeadline(t *testing.T) {
	withConfig(t, "claude_links = true\n")
	dir := t.TempDir()
	old := hookDeadline
	hookDeadline = 50 * time.Millisecond
	release := make(chan struct{})
	t.Cleanup(func() { close(release); hookDeadline = old })
	stuck := func(string, found) error { <-release; return nil }

	start := time.Now()
	quietly(dir, func() error { return claudeHook(hookInput(t, "get_issue.json"), inHerdr, dir, stuck) })
	if d := time.Since(start); d > time.Second {
		t.Errorf("returned after %s, want about 50ms", d)
	}
	if log := readLog(t, dir); !strings.Contains(log, "gave up") {
		t.Errorf("log = %q, want a line saying it gave up", log)
	}
}

// The log sits on the same filesystem as the state, so writing it can hang
// too. That doesn't hold the hook past its deadline either.
func TestQuietlyGivesUpOnAHungLog(t *testing.T) {
	dir := t.TempDir()
	log := filepath.Join(dir, "claude-hook.log")
	// Opening a FIFO that has no reader blocks, as on a hung filesystem.
	if err := syscall.Mkfifo(log, 0o644); err != nil {
		t.Fatal(err)
	}
	old := hookDeadline
	hookDeadline = 100 * time.Millisecond
	release := make(chan struct{})
	t.Cleanup(func() {
		close(release)
		hookDeadline = old
		// Let the log writes that were given up on go on.
		if f, err := os.OpenFile(log, os.O_RDONLY|syscall.O_NONBLOCK, 0); err == nil {
			f.Close()
		}
	})
	for name, fn := range map[string]func() error{
		"an error":  func() error { return errors.New("no task for tab w1:t9") },
		"giving up": func() error { <-release; return nil },
	} {
		start := time.Now()
		quietly(dir, fn)
		if d := time.Since(start); d > time.Second {
			t.Errorf("%s: returned after %s, want about %s", name, d, hookDeadline)
		}
	}
}

func TestQuietlyLogsErrorsAndPanics(t *testing.T) {
	dir := t.TempDir()
	quietly(dir, func() error { return nil })
	if _, err := os.Stat(filepath.Join(dir, "claude-hook.log")); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("a run without errors wrote a log (%v)", err)
	}
	quietly(dir, func() error { return errors.New("no task for tab w1:t9") })
	quietly(dir, func() error { panic("boom") })
	log := readLog(t, dir)
	if lines := strings.Split(strings.TrimSpace(log), "\n"); len(lines) != 2 ||
		!strings.Contains(lines[0], "no task for tab w1:t9") || !strings.Contains(lines[1], "boom") {
		t.Errorf("log = %q, want the error, then the panic", log)
	}
}

func TestHookLogStartsOverWhenLarge(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "claude-hook.log")
	if err := os.WriteFile(path, bytes.Repeat([]byte("old line\n"), 40_000), 0o644); err != nil {
		t.Fatal(err)
	}
	quietly(dir, func() error { return errors.New("new line") })
	if log := readLog(t, dir); strings.Contains(log, "old line") || !strings.Contains(log, "new line") {
		t.Errorf("log has %d bytes, want only the new line", len(log))
	}
}

// summary lists links as "URL source label by", for comparing.
func summary(links []task.Link) []string {
	var out []string
	for _, l := range links {
		out = append(out, strings.Join([]string{l.URL, l.Source, l.Label, l.By}, " | "))
	}
	return out
}

func TestArtifactReplacesItsFileLink(t *testing.T) {
	const src = "/tmp/x/p.html"
	st := seed(t, map[string]*task.Task{"w1:t1": {TabID: "w1:t1", Links: []task.Link{
		{URL: "https://github.com/acme/x/pull/1"},
		{URL: task.FileURL(src), By: "claude"},
	}}})
	if err := addFound(st, "w1:t1", pages(claudehook.Link{URL: anArtifact, Label: "Acme plan", Source: src})); err != nil {
		t.Fatal(err)
	}
	want := []string{
		"https://github.com/acme/x/pull/1 |  |  | ",
		anArtifact + " | " + src + " | Acme plan | claude",
	}
	if got := summary(linksOf(t, st, "w1:t1")); !slices.Equal(got, want) {
		t.Errorf("links = %q, want %q", got, want)
	}

	// Writing the file again adds nothing: the artifact stands for it.
	if err := addFound(st, "w1:t1", pages(claudehook.Link{URL: task.FileURL(src), Label: "Acme plan"})); err != nil {
		t.Fatal(err)
	}
	if got := summary(linksOf(t, st, "w1:t1")); !slices.Equal(got, want) {
		t.Errorf("after a Write, links = %q, want %q", got, want)
	}
}

func TestOutOfOrderArtifactMerges(t *testing.T) {
	const src = "/tmp/x/p.html"
	// A reply names the artifact, with its share key; the publish call
	// knows its file and title.
	reply := pages(claudehook.Link{URL: anArtifact + "?sk=abc"})
	publish := pages(claudehook.Link{URL: anArtifact, Label: "Acme plan", Source: src})
	fresh := func() *store.Store {
		return seed(t, map[string]*task.Task{"w1:t1": {TabID: "w1:t1", Links: []task.Link{{URL: task.FileURL(src), By: "claude"}}}})
	}
	check := func(name string, st *store.Store) {
		t.Helper()
		l := linksOf(t, st, "w1:t1")
		if len(l) != 1 || task.LinkKey(l[0].URL) != task.LinkKey(anArtifact) || l[0].Source != src || l[0].Label != "Acme plan" || l[0].By != "claude" {
			t.Errorf("%s: links = %q, want the artifact alone, with its file and title", name, summary(l))
		}
	}
	for _, order := range []struct {
		name  string
		finds []found
	}{{"reply first", []found{reply, publish}}, {"publish first", []found{publish, reply}}} {
		st := fresh()
		for _, f := range order.finds {
			if err := addFound(st, "w1:t1", f); err != nil {
				t.Fatal(err)
			}
		}
		check(order.name, st)
	}

	st := fresh()
	start := make(chan struct{})
	errs := make(chan error, 2)
	var wg sync.WaitGroup
	for _, f := range []found{reply, publish} {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			errs <- addFound(st, "w1:t1", f)
		}()
	}
	close(start)
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	check("together", st)
}

// removeLink removes which from tab's task, as the strip, the board and
// `link rm` do.
func removeLink(t *testing.T, st *store.Store, tab, which string) {
	t.Helper()
	if err := st.Update(func(s *store.State) error {
		_, err := s.Tasks[tab].RemoveLink(which)
		return err
	}); err != nil {
		t.Fatal(err)
	}
}

// addAll runs addFound on each of finds in turn, or all at once when
// together is set.
func addAll(t *testing.T, st *store.Store, tab string, together bool, finds ...found) {
	t.Helper()
	if !together {
		for _, f := range finds {
			if err := addFound(st, tab, f); err != nil {
				t.Fatal(err)
			}
		}
		return
	}
	start := make(chan struct{})
	errs := make(chan error, len(finds))
	var wg sync.WaitGroup
	for _, f := range finds {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			errs <- addFound(st, tab, f)
		}()
	}
	close(start)
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
}

// checkKeptOff checks that tab's task has no links, and that urls are
// dismissed.
func checkKeptOff(t *testing.T, name string, st *store.Store, tab string, urls ...string) {
	t.Helper()
	s, err := st.Load()
	if err != nil {
		t.Fatal(err)
	}
	tk := s.Tasks[tab]
	if len(tk.Links) > 0 {
		t.Errorf("%s: links = %q, want none", name, summary(tk.Links))
	}
	for _, u := range urls {
		if !tk.Dismissed(u, "") {
			t.Errorf("%s: %s isn't dismissed (dismissed: %q)", name, u, tk.DismissedLinks)
		}
	}
}

func TestRemovedFileKeepsItsArtifactOff(t *testing.T) {
	const src = "/tmp/x/p.html"
	reply := pages(claudehook.Link{URL: anArtifact + "?sk=abc"})
	publish := pages(claudehook.Link{URL: anArtifact, Label: "Acme plan", Source: src})
	write := pages(claudehook.Link{URL: task.FileURL(src), Label: "Acme plan"})
	for _, run := range []struct {
		name     string
		together bool
		finds    []found
	}{
		{"reply first", false, []found{reply, publish}},
		{"publish first", false, []found{publish, reply}},
		{"together", true, []found{reply, publish}},
	} {
		st := seed(t, map[string]*task.Task{"w1:t1": {TabID: "w1:t1", Links: []task.Link{{URL: task.FileURL(src), By: "claude"}}}})
		removeLink(t, st, "w1:t1", task.FileURL(src))
		addAll(t, st, "w1:t1", run.together, run.finds...)
		checkKeptOff(t, run.name, st, "w1:t1", anArtifact, task.FileURL(src))

		// Later hooks don't bring either back.
		addAll(t, st, "w1:t1", false, reply, write, publish)
		checkKeptOff(t, run.name+", then more hooks", st, "w1:t1", anArtifact, task.FileURL(src))
	}
}

func TestRemovedArtifactKeepsItsFileOff(t *testing.T) {
	const src = "/tmp/x/p.html"
	reply := pages(claudehook.Link{URL: anArtifact + "?sk=abc"})
	publish := pages(claudehook.Link{URL: anArtifact, Label: "Acme plan", Source: src})
	write := pages(claudehook.Link{URL: task.FileURL(src), Label: "Acme plan"})

	// The reply names the artifact before the publish call says where it
	// came from, and you remove it in between.
	st := seed(t, map[string]*task.Task{"w1:t1": {TabID: "w1:t1"}})
	addAll(t, st, "w1:t1", false, reply)
	removeLink(t, st, "w1:t1", anArtifact)
	addAll(t, st, "w1:t1", false, publish, write)
	checkKeptOff(t, "reply, remove, publish, write", st, "w1:t1", anArtifact, task.FileURL(src))

	// Claude's link to the file, from a Write before any of that, goes too.
	st = seed(t, map[string]*task.Task{"w1:t1": {TabID: "w1:t1", Links: []task.Link{{URL: task.FileURL(src), By: "claude"}}}})
	addAll(t, st, "w1:t1", false, reply)
	removeLink(t, st, "w1:t1", anArtifact)
	addAll(t, st, "w1:t1", false, publish, write)
	checkKeptOff(t, "write, reply, remove, publish, write", st, "w1:t1", anArtifact, task.FileURL(src))
}

// You linked the file yourself, so Claude's artifact of it sits next to it.
// Removing the file takes the artifact too, whatever hooks come after.
func TestRemovingYourFileKeepsItsArtifactOff(t *testing.T) {
	const src = "/tmp/x/p.html"
	reply := pages(claudehook.Link{URL: anArtifact + "?sk=abc"})
	publish := pages(claudehook.Link{URL: anArtifact, Label: "Acme plan", Source: src})
	write := pages(claudehook.Link{URL: task.FileURL(src), Label: "Acme plan"})
	for _, run := range []struct {
		name  string
		finds []found
	}{
		{"publish", []found{publish}},
		{"reply, then publish", []found{reply, publish}},
		{"publish, then reply", []found{publish, reply}},
	} {
		st := seed(t, map[string]*task.Task{"w1:t1": {TabID: "w1:t1", Links: []task.Link{{URL: task.FileURL(src)}}}})
		addAll(t, st, "w1:t1", false, run.finds...)
		if l := linksOf(t, st, "w1:t1"); len(l) != 2 {
			t.Fatalf("%s: links = %q, want your file and the artifact", run.name, summary(l))
		}
		removeLink(t, st, "w1:t1", task.FileURL(src))
		checkKeptOff(t, run.name, st, "w1:t1", anArtifact, task.FileURL(src))

		addAll(t, st, "w1:t1", false, reply, write)
		checkKeptOff(t, run.name+", then a reply and a write", st, "w1:t1", anArtifact, task.FileURL(src))
		addAll(t, st, "w1:t1", false, publish)
		checkKeptOff(t, run.name+", then a publish", st, "w1:t1", anArtifact, task.FileURL(src))
	}
}

func TestChangesIsADryRun(t *testing.T) {
	const src = "/tmp/x/p.html"
	// Room to grow, so an append in the dry run would land in t's array.
	dismissed := make([]string, 1, 4)
	dismissed[0] = task.LinkKey(task.FileURL(src))
	tk := &task.Task{TabID: "w1:t1", Links: []task.Link{{URL: anArtifact + "?sk=abc", By: "claude"}}, DismissedLinks: dismissed}
	before := summary(tk.Links)

	// Only the dismissals change: that's a change to save.
	if !changes(tk, pages(claudehook.Link{URL: anArtifact, Label: "Acme plan", Source: src})) {
		t.Fatal("changes = false, want true")
	}
	if got := summary(tk.Links); !slices.Equal(got, before) {
		t.Errorf("links = %q, want them as they were: %q", got, before)
	}
	if got := tk.DismissedLinks[:cap(tk.DismissedLinks)]; got[1] != "" {
		t.Errorf("the dry run wrote %q into the task's dismissed links", got)
	}
}

func TestKeepingAPairOffLeavesYourLinks(t *testing.T) {
	const src = "/tmp/x/p.html"
	publish := pages(claudehook.Link{URL: anArtifact, Label: "Acme plan", Source: src})
	for _, c := range []struct {
		name           string
		yours, removed string
	}{
		{"your artifact, its file removed", anArtifact, task.FileURL(src)},
		{"your file, its artifact removed", task.FileURL(src), anArtifact},
	} {
		st := seed(t, map[string]*task.Task{"w1:t1": {TabID: "w1:t1", Links: []task.Link{{URL: c.removed, By: "claude"}}}})
		removeLink(t, st, "w1:t1", c.removed)
		if err := st.Update(func(s *store.State) error {
			s.Tasks["w1:t1"].AddLink(c.yours, "Mine", "", time.Time{})
			return nil
		}); err != nil {
			t.Fatal(err)
		}
		addAll(t, st, "w1:t1", false, publish)
		s, err := st.Load()
		if err != nil {
			t.Fatal(err)
		}
		tk := s.Tasks["w1:t1"]
		if got, want := summary(tk.Links), []string{c.yours + " |  | Mine | "}; !slices.Equal(got, want) {
			t.Errorf("%s: links = %q, want yours as you made it: %q", c.name, got, want)
		}
		if tk.Dismissed(c.yours, "") {
			t.Errorf("%s: your link %s is dismissed", c.name, c.yours)
		}
		if !tk.Dismissed(c.removed, "") {
			t.Errorf("%s: %s isn't dismissed", c.name, c.removed)
		}
	}
}

func TestAddFoundStopsAt60Links(t *testing.T) {
	var links []task.Link
	for i := range 59 {
		links = append(links, task.Link{URL: task.FileURL(fmt.Sprintf("/tmp/x/p%d.html", i)), By: "claude"})
	}
	st := seed(t, map[string]*task.Task{"w1:t1": {TabID: "w1:t1", Links: links}})

	// The first new page makes 60; the second isn't added.
	if err := addFound(st, "w1:t1", pages(claudehook.Link{URL: "http://localhost:4100/a"}, claudehook.Link{URL: "http://localhost:4100/b"})); err != nil {
		t.Fatal(err)
	}
	l := linksOf(t, st, "w1:t1")
	if len(l) != 60 || l[59].URL != "http://localhost:4100/a" {
		t.Fatalf("links = %q, want 60 ending in /a", summary(l))
	}
	log := readLog(t, st.Dir)
	if lines := strings.Split(strings.TrimSpace(log), "\n"); len(lines) != 1 || !strings.Contains(log, "task w1:t1 has 60 links; Claude adds no more") {
		t.Fatalf("log = %q, want one line saying the task is full", log)
	}

	// Later pages aren't added, and the log says nothing more.
	if err := addFound(st, "w1:t1", pages(claudehook.Link{URL: "http://localhost:4100/c"})); err != nil {
		t.Fatal(err)
	}
	if l := linksOf(t, st, "w1:t1"); len(l) != 60 || slices.ContainsFunc(l, func(l task.Link) bool { return strings.HasSuffix(l.URL, "/c") }) {
		t.Errorf("links = %q, want the same 60", summary(l))
	}
	if again := readLog(t, st.Dir); again != log {
		t.Errorf("log = %q, want it unchanged", again)
	}

	// An artifact published from one of the pages still takes its place.
	if err := addFound(st, "w1:t1", pages(claudehook.Link{URL: anArtifact, Label: "Acme plan", Source: "/tmp/x/p7.html"})); err != nil {
		t.Fatal(err)
	}
	l = linksOf(t, st, "w1:t1")
	if len(l) != 60 || l[59].URL != anArtifact || l[59].Source != "/tmp/x/p7.html" ||
		slices.ContainsFunc(l, func(l task.Link) bool { return l.URL == task.FileURL("/tmp/x/p7.html") }) {
		t.Errorf("links = %q, want p7.html replaced by the artifact", summary(l))
	}
	if again := readLog(t, st.Dir); again != log {
		t.Errorf("log = %q, want it unchanged", again)
	}
}

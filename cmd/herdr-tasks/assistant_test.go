package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/alekkowalczyk/herdr-tasks/internal/board"
	"github.com/alekkowalczyk/herdr-tasks/internal/herdr"
	"github.com/alekkowalczyk/herdr-tasks/internal/reconcile"
	"github.com/alekkowalczyk/herdr-tasks/internal/store"
	"github.com/alekkowalczyk/herdr-tasks/internal/task"
)

// A command that changes a task leaves the assistant's tab alone, as the
// assistant runs there: a command it runs without --tab names it.
func TestChangeLeavesTheAssistantAlone(t *testing.T) {
	fn := notAssistant(func(t *task.Task) error { t.Note = "changed"; return nil })
	a := &task.Task{Stage: task.Tool, Assistant: &task.Assistant{SessionID: "s"}}
	if err := fn(a); !errors.Is(err, errAssistantTab) || a.Note != "" {
		t.Errorf("on the assistant: %v, note %q", err, a.Note)
	}
	tk := &task.Task{}
	if err := fn(tk); err != nil || tk.Note != "changed" {
		t.Errorf("on a task: %v, note %q", err, tk.Note)
	}
}

// contextCard is a card with something in each part context prints, some
// of it with control characters, as agents write it.
func contextCard(now time.Time) board.Card {
	log := []task.Entry{}
	for i, s := range []string{"one", "two", "three", "four", "five", "six\x1b[2J"} {
		log = append(log, task.Entry{Who: "claude", At: now.Add(time.Duration(i-10) * time.Minute), Text: s})
	}
	return board.Card{
		Tab: "w1:t2", Title: "Login loop", SpaceLabel: "Acme", Column: "review", Stage: "review",
		Since: now.Add(-3 * time.Hour), Reason: "approval", Priority: true,
		Note: "Blocked on the\nprovider's sandbox\x1b]0;x\x07",
		Log:  log,
		Links: []board.LinkView{
			{URL: "https://github.com/acme/web/pull/12", Label: "PR #12"},
			{URL: "https://evil.example/\x1b]52;c;aGVsbG8=\x07", Label: "Plan"},
		},
		Agents: []board.Agent{{
			Pane: "w1:p3", Agent: "claude", Status: "idle", Title: "Fix login",
			Recap: "You're fixing the redirect loop.", LastPrompt: "fix it",
			LastReply: strings.Repeat("word ", 100),
		}},
	}
}

func TestContextPrintsEveryPart(t *testing.T) {
	now := time.Date(2026, 11, 4, 14, 5, 0, 0, time.Local)
	var out bytes.Buffer
	writeContext(&out, "default", []board.Card{contextCard(now), {Tab: "w1:t1", Title: "Docs", Column: "running", Since: now}}, now)
	s := out.String()
	for _, bad := range []string{"\x1b", "\x07", "evil.example"} {
		if strings.Contains(s, bad) {
			t.Errorf("context printed %q:\n%q", bad, s)
		}
	}
	for _, want := range []string{
		"2 tasks in herdr session default, Wed Nov 4 14:05.",
		"\n\nLogin loop (w1:t2, Acme): in review · priority · 3h\n",
		"  Waiting for: approval\n",
		"  Note:\n    Blocked on the\n    provider's sandbox]0;x\n",
		"  Log:\n    (1 earlier lines)\n    claude ",
		": six[2J\n",
		"    - PR #12  https://github.com/acme/web/pull/12\n",
		"    - Plan  (unsafe link)\n",
		"  Agent claude \"Fix login\" in pane w1:p3: idle\n",
		"    Recap: You're fixing the redirect loop.\n",
		"    Last prompt: fix it\n",
		"\n\nDocs (w1:t1): working · now\n  No agent runs in its tab.\n",
	} {
		if !strings.Contains(s, want) {
			t.Errorf("context didn't print %q:\n%s", want, s)
		}
	}
	if strings.Contains(s, ": one\n") {
		t.Errorf("context printed the whole log:\n%s", s)
	}
	reply := s[strings.Index(s, "Last reply: ")+len("Last reply: "):]
	reply = reply[:strings.IndexByte(reply, '\n')]
	if n := len([]rune(reply)); n != contextText || !strings.HasSuffix(reply, "…") {
		t.Errorf("the last reply is %d runes, want it cut to %d: %q", n, contextText, reply)
	}
}

// One task's context has all of its log and what its agents said.
func TestContextOfOneTask(t *testing.T) {
	now := time.Now()
	var out bytes.Buffer
	writeCard(&out, contextCard(now), true, now)
	s := out.String()
	if !strings.Contains(s, ": one\n") || strings.Contains(s, "earlier lines") {
		t.Errorf("one task's log isn't whole:\n%s", s)
	}
	if !strings.Contains(s, "Last reply: "+strings.TrimSpace(strings.Repeat("word ", 100))+"\n") {
		t.Errorf("one task's last reply was cut:\n%s", s)
	}
}

// The hook reads what Claude sends, and prints the tasks.
func TestAssistantHookPrintsTheTasks(t *testing.T) {
	_, c := newMovingHerdr(t)
	a := &app{client: c, store: &store.Store{Dir: t.TempDir(), Session: herdr.SessionKey(c.SocketPath)}}
	in := strings.NewReader(`{"hook_event_name":"UserPromptSubmit","prompt":"what's waiting on me?"}`)
	var out bytes.Buffer
	if err := a.assistantHook(in, &out); err != nil {
		t.Fatal(err)
	}
	if in.Len() != 0 {
		t.Errorf("%d bytes of the input left unread", in.Len())
	}
	if s := out.String(); !strings.HasPrefix(s, "The tasks in herdr now (") || !strings.HasSuffix(s, "There are none.\n") {
		t.Errorf("hook printed %q", s)
	}
}

// move leaves the assistant's tab where it is, as the other changes do.
func TestMoveLeavesTheAssistantAlone(t *testing.T) {
	_, c := newMovingHerdr(t)
	st := &store.Store{Dir: t.TempDir(), Session: herdr.SessionKey(c.SocketPath)}
	err := st.Update(func(s *store.State) error {
		s.Tasks["w1:t1"] = &task.Task{TabID: "w1:t1", Stage: task.Tool, Assistant: &task.Assistant{SessionID: "s"}}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	a := &app{client: c, store: st}
	a.flags.tab, a.flags.args = "w1:t1", []string{"billing"}
	if err := a.move(); !errors.Is(err, errAssistantTab) {
		t.Errorf("moving the assistant's tab: %v", err)
	}
}

// listingHerdr lists workspace w1 "infra" and its tab w1:t1, and answers
// anything else with an empty result, so a strip can't be added: the tab's
// layout has no panes.
func listingHerdr(t *testing.T) *herdr.Client {
	t.Helper()
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
			go func() {
				defer conn.Close()
				var req struct{ Method string }
				if json.NewDecoder(conn).Decode(&req) != nil {
					return
				}
				result := `{}`
				switch req.Method {
				case "workspace.list":
					result = `{"workspaces":[{"workspace_id":"w1","label":"infra","number":1}]}`
				case "tab.list":
					result = `{"tabs":[{"tab_id":"w1:t1","workspace_id":"w1","label":"Reply to Dana","number":1}]}`
				}
				_, _ = conn.Write([]byte(`{"id":"herdr-tasks","result":` + result + "}\n"))
			}()
		}
	}()
	return &herdr.Client{SocketPath: path}
}

// A strip that can't be added to a tab doesn't keep context from printing
// the tasks.
func TestContextWhenAStripFails(t *testing.T) {
	c := listingHerdr(t)
	a := &app{client: c, store: &store.Store{Dir: t.TempDir(), Session: "test"}}
	a.opts.Strip = true
	if _, err := reconcile.Run(a.host(), a.store, a.opts, time.Now(), nil); err == nil {
		t.Fatal("the strip was added")
	}
	var out bytes.Buffer
	if err := a.contextCmd(&out); err != nil {
		t.Fatal(err)
	}
	if s := out.String(); !strings.HasPrefix(s, "1 task in herdr session ") || !strings.Contains(s, "\n\nReply to Dana (w1:t1, infra): ") {
		t.Errorf("context printed %q", s)
	}
}

// The hook keeps the session Claude says it runs as the one to resume, as
// /clear starts another.
func TestAssistantHookKeepsTheSession(t *testing.T) {
	c := listingHerdr(t)
	st := &store.Store{Dir: t.TempDir(), Session: "test"}
	err := st.Update(func(s *store.State) error {
		s.Tasks["w1:t1"] = &task.Task{TabID: "w1:t1", Stage: task.Tool, Assistant: &task.Assistant{SessionID: "11111111-1111-4111-8111-111111111111"}}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	a := &app{client: c, store: st}
	id := "22222222-2222-4222-8222-222222222222"
	in := strings.NewReader(`{"hook_event_name":"UserPromptSubmit","session_id":"` + id + `","prompt":"hi"}`)
	if err := a.assistantHook(in, io.Discard); err != nil {
		t.Fatal(err)
	}
	s, err := st.Load()
	if err != nil {
		t.Fatal(err)
	}
	if got := s.Tasks["w1:t1"].Assistant.SessionID; got != id {
		t.Errorf("session = %q, want %q", got, id)
	}
}

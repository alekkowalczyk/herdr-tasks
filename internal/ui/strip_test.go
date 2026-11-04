package ui

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/charmbracelet/bubbles/textarea"
	"github.com/charmbracelet/bubbles/textinput"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/x/ansi"

	"github.com/alekkowalczyk/herdr-tasks/internal/herdr"
	"github.com/alekkowalczyk/herdr-tasks/internal/reconcile"
	"github.com/alekkowalczyk/herdr-tasks/internal/store"
	"github.com/alekkowalczyk/herdr-tasks/internal/strip"
	"github.com/alekkowalczyk/herdr-tasks/internal/task"
)

// fakeHerdr answers like a herdr with one workspace, w1, holding one tab,
// w1:t1, that pane w1:p9 is in, and the empty workspaces more after it.
// Anything else gets an empty result.
func fakeHerdr(t *testing.T, more ...herdr.Workspace) *herdr.Client {
	t.Helper()
	wss := append([]herdr.Workspace{{ID: "w1", Label: "Acme", Number: 1}}, more...)
	return herdrOf(t, wss, []herdr.Tab{{ID: "w1:t1", WorkspaceID: "w1", Number: 1, Label: "Acme"}})
}

// herdrOf answers like a herdr with workspaces wss and tabs, the first of
// which pane w1:p9 is in. Anything else gets an empty result.
func herdrOf(t *testing.T, wss []herdr.Workspace, tabs []herdr.Tab) *herdr.Client {
	t.Helper()
	wsJSON, err := json.Marshal(wss)
	if err != nil {
		t.Fatal(err)
	}
	tabsJSON, err := json.Marshal(tabs)
	if err != nil {
		t.Fatal(err)
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
			go func() {
				defer conn.Close()
				line, err := bufio.NewReader(conn).ReadBytes('\n')
				if err != nil {
					return
				}
				var req struct {
					Method string
					Params struct {
						TabID string `json:"tab_id"`
					}
				}
				_ = json.Unmarshal(line, &req)
				result := `{}`
				switch req.Method {
				case "pane.get":
					result = `{"pane":{"pane_id":"w1:p9","tab_id":"` + tabs[0].ID + `"}}`
				case "tab.get":
					tab := tabs[0]
					if i := slices.IndexFunc(tabs, func(x herdr.Tab) bool { return x.ID == req.Params.TabID }); i >= 0 {
						tab = tabs[i]
					}
					b, _ := json.Marshal(tab)
					result = `{"tab":` + string(b) + `}`
				case "tab.list":
					result = `{"tabs":` + string(tabsJSON) + `}`
				case "workspace.list":
					result = `{"workspaces":` + string(wsJSON) + `}`
				}
				_, _ = conn.Write([]byte(`{"id":"herdr-tasks","result":` + result + "}\n"))
			}()
		}
	}()
	return &herdr.Client{SocketPath: path}
}

// setLinks replaces the links of w1:t1's task.
func setLinks(t *testing.T, st *store.Store, links ...task.Link) {
	t.Helper()
	if err := st.Update(func(s *store.State) error {
		if s.Tasks["w1:t1"] == nil {
			s.Tasks["w1:t1"] = &task.Task{TabID: "w1:t1", Workspace: "Acme", Stage: task.Active, Since: time.Now()}
		}
		s.Tasks["w1:t1"].Links = links
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

func TestUnlinkRemovesTheLinkYouSaw(t *testing.T) {
	st := &store.Store{Dir: t.TempDir(), Session: "test"}
	const src = "/tmp/acme/plan.html"
	a := task.Link{URL: task.FileURL(src), By: "claude"}
	b := task.Link{URL: "https://github.com/acme/x/issues/2", By: "claude"}
	c := task.Link{URL: "https://github.com/acme/x/issues/3", By: "claude"}
	art := task.Link{URL: "https://claude.ai/code/artifact/00000000-0000-4000-8000-000000000001", Source: src, By: "claude"}
	setLinks(t, st, a, b, c)

	m := &stripModel{c: fakeHerdr(t), st: st, opts: reconcile.Options{}, pane: "w1:p9", rows: strip.Rows}
	m.input = textinput.New()
	m.input.Prompt = ""
	m.note = textarea.New()
	m.width, m.height = 100, 14
	m.Update(m.load())
	if m.t == nil {
		t.Fatalf("no task loaded: %q", m.err)
	}

	// You ask to remove link 2, issue #2.
	m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("L")})
	m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("2")})
	// Meanwhile an artifact published from the file takes its place, and the
	// strip refreshes.
	setLinks(t, st, b, c, art)
	m.Update(m.load())
	if m.mode != modeUnlink {
		t.Fatal("the refresh ended unlinking")
	}
	// The numbers you're choosing from don't move.
	if view := m.View(); !slices.ContainsFunc(strings.Split(view, "\n"), func(l string) bool { return strings.Contains(plain(l), "2 Links issue #2") }) {
		t.Errorf("view doesn't list issue #2 as 2:\n%s", plain(view))
	}
	m.Update(tea.KeyMsg{Type: tea.KeyEnter})

	s, err := st.Load()
	if err != nil {
		t.Fatal(err)
	}
	tk := s.Tasks["w1:t1"]
	var got []string
	for _, l := range tk.Links {
		got = append(got, l.URL)
	}
	if want := []string{c.URL, art.URL}; !slices.Equal(got, want) {
		t.Errorf("links = %q, want %q", got, want)
	}
	if !tk.Dismissed(b.URL, "") || tk.Dismissed(c.URL, "") {
		t.Errorf("dismissed = %q, want issue #2 only", tk.DismissedLinks)
	}
	if m.msg != "Removed issue #2" {
		t.Errorf("message = %q, want Removed issue #2", m.msg)
	}
}

func TestUnlinkOfALinkGoneMeanwhileSaysSo(t *testing.T) {
	st := &store.Store{Dir: t.TempDir(), Session: "test"}
	b := task.Link{URL: "https://github.com/acme/x/issues/2", By: "claude"}
	c := task.Link{URL: "https://github.com/acme/x/issues/3", By: "claude"}
	setLinks(t, st, b, c)

	m := &stripModel{c: fakeHerdr(t), st: st, opts: reconcile.Options{}, pane: "w1:p9", rows: strip.Rows}
	m.input = textinput.New()
	m.note = textarea.New()
	m.width, m.height = 100, 14
	m.Update(m.load())
	m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("L")})
	m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("1")})
	// The board removes issue #2 before you press enter.
	setLinks(t, st, c)
	m.Update(tea.KeyMsg{Type: tea.KeyEnter})

	if l := linksOfTab(t, st); len(l) != 1 || l[0].URL != c.URL {
		t.Errorf("links = %+v, want issue #3 alone", l)
	}
	if m.msg != "issue #2 was removed already" {
		t.Errorf("message = %q, want issue #2 was removed already", m.msg)
	}
}

func linksOfTab(t *testing.T, st *store.Store) []task.Link {
	t.Helper()
	s, err := st.Load()
	if err != nil {
		t.Fatal(err)
	}
	return s.Tasks["w1:t1"].Links
}

// addLink types url into the strip's add-link input and saves it.
func (m *stripModel) addLink(url string) {
	m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("l")})
	m.input.SetValue(url)
	m.Update(tea.KeyMsg{Type: tea.KeyEnter})
}

func TestStripSaysWhenALinkIsntAdded(t *testing.T) {
	st := &store.Store{Dir: t.TempDir(), Session: "test"}
	setLinks(t, st)
	m := &stripModel{c: fakeHerdr(t), st: st, opts: reconcile.Options{}, pane: "w1:p9", rows: strip.Rows}
	m.input = textinput.New()
	m.note = textarea.New()
	m.width, m.height = 100, 14
	m.Update(m.load())

	for _, bad := range []string{"https://example.com/100%done", "https://example.com/%0A"} {
		m.addLink(bad)
		if l := linksOfTab(t, st); len(l) != 0 {
			t.Errorf("%s: links = %+v, want none", bad, l)
		}
		if m.msg != "Not a valid link" {
			t.Errorf("%s: message = %q, want Not a valid link", bad, m.msg)
		}
	}
	m.addLink("https://github.com/acme/x/pull/2")
	if m.msg != "Added PR #2" {
		t.Errorf("message = %q, want Added PR #2", m.msg)
	}
	m.addLink("https://github.com/acme/x/pull/2")
	if l := linksOfTab(t, st); len(l) != 1 {
		t.Errorf("links = %+v, want PR #2 once", l)
	}
	if m.msg != "Already linked" {
		t.Errorf("message = %q, want Already linked", m.msg)
	}
}

// The workspaces of the session besides Acme, where w1:t1 is.
var others = []herdr.Workspace{{ID: "w2", Label: "billing", Number: 2}, {ID: "w3", Label: "infra", Number: 3}, {ID: "w4", Label: "Slack", Number: 4}}

// setTask puts tk in the store as w1:t1's task.
func setTask(t *testing.T, st *store.Store, tk *task.Task) {
	t.Helper()
	tk.TabID, tk.Workspace = "w1:t1", "Acme"
	if tk.Since.IsZero() {
		tk.Since = time.Now()
	}
	if err := st.Update(func(s *store.State) error {
		s.Tasks["w1:t1"] = tk
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

func dana(r task.Research) *task.SlackAsk {
	a := &task.SlackAsk{Key: "C0123ABCD/1793454730.123456", Channel: "C0123ABCD", Place: "#billing", Author: "Dana Reed", Research: r}
	if r == task.ResearchLocked || r == task.ResearchOpen {
		a.SessionID = "00000000-0000-4000-8000-0000000000aa"
	}
	return a
}

// newStrip is w1:t1's strip, loaded.
func newStrip(t *testing.T, st *store.Store, c *herdr.Client) *stripModel {
	t.Helper()
	m := &stripModel{c: c, st: st, opts: reconcile.Options{}, pane: "w1:p9", rows: strip.Rows}
	m.input = textinput.New()
	m.input.Prompt = ""
	m.note = textarea.New()
	m.width, m.height = 160, 9
	m.Update(m.load())
	if m.t == nil {
		t.Fatalf("no task loaded: %q", m.err)
	}
	return m
}

// press types key into m and returns the command it starts, if any.
func (m *stripModel) press(key string) tea.Cmd {
	msg := tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(key)}
	switch key {
	case "enter":
		msg = tea.KeyMsg{Type: tea.KeyEnter}
	case "esc":
		msg = tea.KeyMsg{Type: tea.KeyEsc}
	case "up":
		msg = tea.KeyMsg{Type: tea.KeyUp}
	case "down":
		msg = tea.KeyMsg{Type: tea.KeyDown}
	}
	_, cmd := m.Update(msg)
	return cmd
}

// finish runs a key's command to its end and hands the result back.
func (m *stripModel) finish(t *testing.T, cmd tea.Cmd) {
	t.Helper()
	if cmd == nil {
		t.Fatal("no command to finish")
	}
	m.Update(cmd())
}

// buttonRow is the strip's button row, as text.
func (m *stripModel) buttonRow() string {
	lines := strings.Split(m.View(), "\n")
	return plain(lines[len(lines)-1])
}

func TestStripMutesASlackTask(t *testing.T) {
	st := &store.Store{Dir: t.TempDir(), Session: "test"}
	setTask(t, st, &task.Task{Stage: task.Active, Slack: dana(task.ResearchLocked)})
	m := newStrip(t, st, fakeHerdr(t))
	var calls [][2]string
	muted := map[string]bool{}
	m.act.mute = func(channel, place string) (bool, error) {
		calls = append(calls, [2]string{channel, place})
		added := !muted[channel]
		muted[channel] = true
		return added, nil
	}
	if row := m.buttonRow(); !strings.Contains(row, "m mute") {
		t.Errorf("buttons = %q, want m mute", row)
	}
	m.press("m")
	if want := "Muted #billing. Undo: herdr-tasks slack unmute C0123ABCD"; m.msg != want {
		t.Errorf("message = %q, want %q", m.msg, want)
	}
	if want := [][2]string{{"C0123ABCD", "#billing"}}; !slices.Equal(calls, want) {
		t.Errorf("mute calls = %v, want %v", calls, want)
	}
	m.press("m")
	if m.msg != "#billing is muted already" {
		t.Errorf("message = %q, want #billing is muted already", m.msg)
	}
	m.act.mute = func(string, string) (bool, error) { return false, errors.New("disk full") }
	m.press("m")
	if m.msg != "Couldn't mute: disk full" {
		t.Errorf("message = %q, want Couldn't mute: disk full", m.msg)
	}

	calls = nil
	setTask(t, st, &task.Task{Stage: task.Active})
	m.Update(m.load())
	m.act.mute = func(channel, place string) (bool, error) {
		calls = append(calls, [2]string{channel, place})
		return true, nil
	}
	m.press("m")
	if m.msg != "Only a Slack task's channel can be muted." || len(calls) != 0 {
		t.Errorf("plain task: message %q, mute calls %v; want the Only a Slack task's message and none", m.msg, calls)
	}
	if row := m.buttonRow(); strings.Contains(row, "m mute") {
		t.Errorf("plain task's buttons = %q, want no m mute", row)
	}
}

func TestStripContinueAndResearchKeys(t *testing.T) {
	st := &store.Store{Dir: t.TempDir(), Session: "test"}
	setTask(t, st, &task.Task{Stage: task.Active, Slack: dana(task.ResearchLocked)})
	m := newStrip(t, st, fakeHerdr(t))
	var conts, researches []string
	var contErr error
	m.act.cont = func(tab string) error { conts = append(conts, tab); return contErr }
	m.act.research = func(tab string) error { researches = append(researches, tab); return nil }

	// Locked: c continues; research has started.
	row := m.buttonRow()
	if !strings.Contains(row, "c continue") || strings.Contains(row, "s research") || !strings.Contains(row, "w move") {
		t.Errorf("locked buttons = %q, want c continue and w move, no s research", row)
	}
	m.press("s")
	if m.msg != "Research already started." || len(researches) != 0 {
		t.Errorf("locked s: message %q, research calls %v", m.msg, researches)
	}
	cmd := m.press("c")
	if m.msg != "Continuing with your tools…" {
		t.Errorf("message = %q, want Continuing with your tools…", m.msg)
	}
	m.finish(t, cmd)
	if m.msg != "Continued with your tools" || !slices.Equal(conts, []string{"w1:t1"}) {
		t.Errorf("message %q, continue calls %v; want Continued with your tools, w1:t1", m.msg, conts)
	}
	contErr = errors.New("Claude didn't stop")
	m.finish(t, m.press("c"))
	if m.msg != "Claude didn't stop" {
		t.Errorf("message = %q, want the error", m.msg)
	}

	// Held: s starts research; there's nothing to continue.
	conts = nil
	setTask(t, st, &task.Task{Stage: task.Active, Slack: dana(task.ResearchHeld)})
	m.Update(m.load())
	row = m.buttonRow()
	if strings.Contains(row, "c continue") || !strings.Contains(row, "s research") {
		t.Errorf("held buttons = %q, want s research, no c continue", row)
	}
	m.press("c")
	if m.msg != "No locked research session in this tab." || len(conts) != 0 {
		t.Errorf("held c: message %q, continue calls %v", m.msg, conts)
	}
	cmd = m.press("s")
	if m.msg != "Starting research…" {
		t.Errorf("message = %q, want Starting research…", m.msg)
	}
	m.finish(t, cmd)
	if !slices.Equal(researches, []string{"w1:t1"}) || m.msg != "Research started" {
		t.Errorf("research calls %v, message %q; want w1:t1, Research started", researches, m.msg)
	}

	// A task that isn't from Slack.
	researches = nil
	setTask(t, st, &task.Task{Stage: task.Active})
	m.Update(m.load())
	m.press("s")
	if m.msg != "Only a Slack task has research." {
		t.Errorf("plain s: message %q", m.msg)
	}
	m.press("c")
	if m.msg != "No locked research session in this tab." {
		t.Errorf("plain c: message %q", m.msg)
	}
	if len(conts)+len(researches) != 0 {
		t.Errorf("plain task called continue %v, research %v", conts, researches)
	}
	if row := m.buttonRow(); strings.Contains(row, "c continue") || strings.Contains(row, "s research") || !strings.Contains(row, "w move") {
		t.Errorf("plain buttons = %q, want w move only of these", row)
	}
}

// A asks the assistant about this tab's task, which shows it; while another
// action runs, it waits.
func TestStripAsksTheAssistant(t *testing.T) {
	st := &store.Store{Dir: t.TempDir(), Session: "test"}
	setTask(t, st, &task.Task{Stage: task.Active})
	m := newStrip(t, st, fakeHerdr(t))
	var asked [][2]string
	var askErr error
	m.act.ask = func(tab, from string) error {
		asked = append(asked, [2]string{tab, from})
		return askErr
	}
	if row := m.buttonRow(); !strings.Contains(row, "A assistant") {
		t.Errorf("buttons = %q, want A assistant", row)
	}
	cmd := m.press("A")
	if m.msg != "Opening the assistant…" || len(asked) != 0 {
		t.Errorf("message %q, asked %v; want Opening the assistant… and nothing yet", m.msg, asked)
	}
	if m.press("A") != nil {
		t.Error("A asked again while it ran")
	}
	m.finish(t, cmd)
	if want := [][2]string{{"w1:t1", "w1:t1"}}; !slices.Equal(asked, want) || m.msg != "" {
		t.Errorf("asked %v, message %q; want %v and no message", asked, m.msg, want)
	}
	askErr = errors.New("Claude didn't start in the assistant's tab: no shell")
	m.finish(t, m.press("A"))
	if m.msg != askErr.Error() {
		t.Errorf("message = %q, want the error", m.msg)
	}
}

func TestStripMovePicker(t *testing.T) {
	st := &store.Store{Dir: t.TempDir(), Session: "test"}
	setTask(t, st, &task.Task{Stage: task.Active})
	m := newStrip(t, st, fakeHerdr(t, others...))
	var moves [][2]string
	var moveErr error
	m.act.move = func(tab, ws string) (string, error) {
		moves = append(moves, [2]string{tab, ws})
		return "w3:t9", moveErr
	}
	bottom := func() string {
		lines := strings.Split(m.View(), "\n")
		return ansi.Strip(lines[len(lines)-1])
	}

	m.press("w")
	if m.mode != modeMove {
		t.Fatalf("mode = %v, want the picker (%q)", m.mode, m.msg)
	}
	if want := "Move to workspace: 1 billing  2 infra  3 Slack, then Enter"; !strings.Contains(bottom(), want) {
		t.Errorf("picker = %q, want %q", bottom(), want)
	}
	m.press("x")
	m.press("2")
	m.press(" ")
	if v := m.input.Value(); v != "2" {
		t.Errorf("typed %q, want only the digit 2", v)
	}
	cmd := m.press("enter")
	if m.mode != modeList || m.msg != "Moving to infra…" {
		t.Errorf("mode %v message %q, want the list and Moving to infra…", m.mode, m.msg)
	}
	m.finish(t, cmd)
	if want := [][2]string{{"w1:t1", "w3"}}; !slices.Equal(moves, want) {
		t.Errorf("moves = %v, want %v", moves, want)
	}

	// Esc leaves the picker without moving.
	m.press("w")
	m.press("1")
	m.press("esc")
	if m.mode != modeList || len(moves) != 1 {
		t.Errorf("esc: mode %v, moves %v; want the list, no new move", m.mode, moves)
	}

	// A number off the list, and a move that fails, say so.
	m.press("w")
	m.press("7")
	m.press("enter")
	if m.msg != "No workspace 7. Pick 1–3." || len(moves) != 1 {
		t.Errorf("7: message %q, moves %v", m.msg, moves)
	}
	moveErr = errors.New("Moved 1 of 2 panes; the rest are still in the old tab: refused")
	m.press("w")
	m.press("3")
	m.finish(t, m.press("enter"))
	if m.msg != moveErr.Error() || moves[len(moves)-1] != [2]string{"w1:t1", "w4"} {
		t.Errorf("failed move: message %q, moves %v", m.msg, moves)
	}
}

func TestStripMoveWithNoOtherWorkspace(t *testing.T) {
	st := &store.Store{Dir: t.TempDir(), Session: "test"}
	setTask(t, st, &task.Task{Stage: task.Active})
	m := newStrip(t, st, fakeHerdr(t))
	m.press("w")
	if m.mode != modeList || m.msg != "No other workspace to move to." {
		t.Errorf("mode %v message %q, want the list and No other workspace to move to.", m.mode, m.msg)
	}
}

func TestStripRunsOneSlowActionAtATime(t *testing.T) {
	st := &store.Store{Dir: t.TempDir(), Session: "test"}
	setTask(t, st, &task.Task{Stage: task.Active, Slack: dana(task.ResearchHeld)})
	m := newStrip(t, st, fakeHerdr(t, others...))
	m.act.research = func(string) error { return nil }
	// The installed binary changes while research starts.
	bin := filepath.Join(t.TempDir(), "herdr-tasks")
	if err := os.WriteFile(bin, nil, 0o755); err != nil {
		t.Fatal(err)
	}
	m.bin, m.binID = bin, fileID(bin)+1

	cmd := m.press("s")
	for _, k := range []string{"s", "c", "w"} {
		if again := m.press(k); again != nil || m.mode != modeList || m.msg != "Starting research…" {
			t.Errorf("%s while starting: command %v, mode %v, message %q; want nothing", k, again != nil, m.mode, m.msg)
		}
	}
	m.Update(stripTick{})
	if m.restart {
		t.Error("the strip restarted while research was starting")
	}
	m.finish(t, cmd)
	m.Update(stripTick{})
	if !m.restart {
		t.Error("the strip didn't restart for the new binary once research had started")
	}
}

// While a slow action runs, the keys that change the task or close its tab
// wait: the action may be moving the tab.
func TestStripRefusesStagesAndCloseWhileBusy(t *testing.T) {
	st := &store.Store{Dir: t.TempDir(), Session: "test"}
	setTask(t, st, &task.Task{Stage: task.Done, Slack: dana(task.ResearchHeld), Links: harnessPages(2)})
	m := newStrip(t, st, fakeHerdr(t, others...))
	m.act.move = func(string, string) (string, error) { return "w1:t1", nil }
	var mutes int
	m.act.mute = func(string, string) (bool, error) { mutes++; return true, nil }
	m.press("w")
	m.press("1")
	cmd := m.press("enter")
	if m.msg != "Moving to billing…" {
		t.Fatalf("message = %q, want Moving to billing…", m.msg)
	}
	for _, k := range []string{"a", "v", "p", "z", "d", "o", "x", "m", "n", "l", "L", "r", "f"} {
		if again := m.press(k); again != nil || m.mode != modeList || m.msg != "Wait for it to finish" {
			t.Errorf("%s while moving: command %v, mode %v, message %q; want Wait for it to finish", k, again != nil, m.mode, m.msg)
		}
	}
	if s, _ := st.Load(); s.Tasks["w1:t1"].Stage != task.Done || mutes != 0 {
		t.Errorf("stage = %v, %d mutes while moving; want it left done, no mutes", s.Tasks["w1:t1"].Stage, mutes)
	}
	m.finish(t, cmd)
	m.finish(t, m.press("a"))
	if s, _ := st.Load(); s.Tasks["w1:t1"].Stage != task.Active {
		t.Errorf("stage = %v once moved, want a to work again", s.Tasks["w1:t1"].Stage)
	}
}

// harnessPages are n local pages with labels, as an agent adds them.
func harnessPages(n int) []task.Link {
	var ls []task.Link
	for i := 1; i <= n; i++ {
		ls = append(ls, task.Link{URL: fmt.Sprintf("http://localhost:4100/page-%d", i), Label: fmt.Sprintf("Acme harness page %d", i), By: "claude"})
	}
	return ls
}

// linksWidth is the width of the strip's links column, from where its first
// body row puts the "│" before it.
func (m *stripModel) linksWidth(t *testing.T) int {
	t.Helper()
	line := ansi.Strip(strings.Split(m.View(), "\n")[1])
	before, _, ok := strings.Cut(line, "│")
	if !ok {
		t.Fatalf("no links column: %q", line)
	}
	return m.width - ansi.StringWidth(before) - 2 // "│ "
}

func TestStripLinksUseFreeWidth(t *testing.T) {
	st := &store.Store{Dir: t.TempDir(), Session: "test"}
	setTask(t, st, &task.Task{Stage: task.Active, Links: harnessPages(6)})
	m := newStrip(t, st, fakeHerdr(t))
	m.width, m.height = 200, strip.Rows

	// No note: the links take most of the strip, and all six fit on a row.
	if w := m.linksWidth(t); w < 150 {
		t.Errorf("links column = %d cells, want at least 150", w)
	}
	view := m.View()
	if n := strings.Count(view, "\x1b]8;;"); n != 2*6 {
		t.Errorf("%d OSC 8 sequences, want 12 for six pages:\n%s", n, plain(view))
	}
	if strings.Contains(m.buttonRow(), "+ links") {
		t.Errorf("buttons = %q, want no + links while all are shown", m.buttonRow())
	}

	// A note leaves them what it doesn't use, and at least 52 cells, as before.
	for _, c := range []struct{ note, want int }{{100, 96}, {190, 52}} {
		setTask(t, st, &task.Task{Stage: task.Active, Note: strings.Repeat("abcd ", c.note/5-1) + "abcde", Links: harnessPages(6)})
		m.Update(m.load())
		if w := m.linksWidth(t); w != c.want {
			t.Errorf("%d-cell note: links column = %d cells, want %d", c.note, w, c.want)
		}
		for i, l := range strings.Split(m.View(), "\n") {
			if w := ansi.StringWidth(l); w > m.width {
				t.Errorf("%d-cell note: line %d is %d cells wide", c.note, i, w)
			}
		}
	}
}

// plusOnScreen is where the strip's view shows "+N" for the links it leaves out.
func (m *stripModel) plusOnScreen(t *testing.T) (x, y int) {
	t.Helper()
	plus := regexp.MustCompile(` \+[0-9]+ *$`)
	for y, l := range strings.Split(m.View(), "\n") {
		s := ansi.Strip(l)
		if i := plus.FindStringIndex(s); i != nil {
			return ansi.StringWidth(s[:i[0]+1]), y
		}
	}
	t.Fatalf("no +N:\n%s", plain(m.View()))
	return 0, 0
}

func TestStripPlusShowsAllLinks(t *testing.T) {
	st := &store.Store{Dir: t.TempDir(), Session: "test"}
	issue := task.Link{URL: "https://linear.app/acme/issue/ABC-12"}
	setTask(t, st, &task.Task{Stage: task.Active, Note: "Check the harness.", Links: append(harnessPages(20), issue)})
	m := newStrip(t, st, fakeHerdr(t))
	m.width, m.height = 120, strip.Rows

	if row := m.buttonRow(); !strings.Contains(row, "+ links") {
		t.Errorf("buttons = %q, want + links while some are hidden", row)
	}
	m.press("+")
	if m.mode != modeLinks || m.rows != noteRows {
		t.Fatalf("+: mode %v, rows %d; want every link, the strip grown to %d", m.mode, m.rows, noteRows)
	}
	m.Update(tea.WindowSizeMsg{Width: 120, Height: noteRows}) // herdr fits the pane
	view := m.View()
	lines := strings.Split(view, "\n")
	if n := strings.Count(view, "\x1b]8;;"); n != 2*21 {
		t.Errorf("%d OSC 8 sequences, want 42, one link each:\n%s", n, plain(view))
	}
	body := plain(strings.Join(lines[1:len(lines)-1], " "))
	for _, want := range []string{"1 Linear ABC-12", "2 Pages Acme harness page 1 ", "21 Pages Acme harness page 20"} {
		if !strings.Contains(body, want) {
			t.Errorf("body doesn't list %q:\n%s", want, body)
		}
	}
	if got := m.buttonRow(); got != "All links · + or esc to close" {
		t.Errorf("bottom row = %q, want All links · + or esc to close", got)
	}
	m.press("x") // other keys do nothing here
	if m.mode != modeLinks {
		t.Errorf("x closed the list")
	}
	m.press("+")
	if m.mode != modeList || m.rows != strip.Rows {
		t.Errorf("+ again: mode %v, rows %d; want the strip back", m.mode, m.rows)
	}
	m.Update(tea.WindowSizeMsg{Width: 120, Height: strip.Rows})

	// A click on "+N" shows them too, and esc closes the list.
	x, y := m.plusOnScreen(t)
	m.Update(tea.MouseMsg{X: x - 2, Y: y, Action: tea.MouseActionPress, Button: tea.MouseButtonLeft})
	if m.mode != modeList {
		t.Fatal("a click beside +N showed every link")
	}
	m.Update(tea.MouseMsg{X: x, Y: y, Action: tea.MouseActionPress, Button: tea.MouseButtonLeft})
	if m.mode != modeLinks {
		t.Fatalf("a click on +N at %d,%d: mode %v, want every link", x, y, m.mode)
	}
	m.press("esc")
	if m.mode != modeList || m.rows != strip.Rows {
		t.Errorf("esc: mode %v, rows %d; want the strip back", m.mode, m.rows)
	}

	// Once every link fits, there's no + links button.
	setTask(t, st, &task.Task{Stage: task.Active, Links: append(harnessPages(2), issue)})
	m.Update(m.load())
	if row := m.buttonRow(); strings.Contains(row, "+ links") {
		t.Errorf("buttons = %q, want no + links once all are shown", row)
	}
}

// More links than the list has room for scroll, a row at a time, until
// you've seen every one.
func TestStripAllLinksScroll(t *testing.T) {
	st := &store.Store{Dir: t.TempDir(), Session: "test"}
	setTask(t, st, &task.Task{Stage: task.Active, Links: harnessPages(40)})
	m := newStrip(t, st, fakeHerdr(t))
	m.width = 60
	m.press("+")
	m.Update(tea.WindowSizeMsg{Width: 60, Height: noteRows})
	number := regexp.MustCompile(`(\d+) Pages Acme harness page (\d+)`)
	seen := map[string]bool{}
	look := func() {
		t.Helper()
		lines := strings.Split(m.View(), "\n")
		for _, l := range lines[1 : len(lines)-1] {
			l = plain(l)
			if strings.Contains(l, "more:") {
				t.Fatalf("a +N more cell while the list scrolls: %q", l)
			}
			for _, g := range number.FindAllStringSubmatch(l, -1) {
				if g[1] != g[2] {
					t.Errorf("link %s shows page %s", g[1], g[2])
				}
				seen[g[1]] = true
			}
		}
	}
	look()
	if got, want := m.buttonRow(), "All links 1–12 of 40 · ↑↓ more · + or esc to close"; got != want {
		t.Errorf("bottom row = %q, want %q", got, want)
	}
	m.press("up") // already at the top
	if got := m.buttonRow(); !strings.HasPrefix(got, "All links 1–12 of 40") {
		t.Errorf("up at the top: bottom row = %q", got)
	}
	for i := 0; i < 40; i++ {
		key := "down"
		if i%2 == 1 {
			key = "j"
		}
		m.press(key)
		look()
	}
	if got, want := m.buttonRow(), "All links 29–40 of 40 · ↑↓ more · + or esc to close"; got != want {
		t.Errorf("at the end: bottom row = %q, want %q", got, want)
	}
	for i := 1; i <= 40; i++ {
		if !seen[strconv.Itoa(i)] {
			t.Errorf("link %d never showed", i)
		}
	}
	m.press("k")
	m.press("up")
	if got := m.buttonRow(); !strings.HasPrefix(got, "All links 27–38 of 40") {
		t.Errorf("k and up: bottom row = %q, want 27–38", got)
	}

	// The list opens at its top again.
	m.press("+")
	m.press("+")
	m.Update(tea.WindowSizeMsg{Width: 60, Height: noteRows})
	if got := m.buttonRow(); !strings.HasPrefix(got, "All links 1–12 of 40") {
		t.Errorf("reopened: bottom row = %q, want 1–12", got)
	}
}

func TestStripUnlinkNumbersInGroupedOrder(t *testing.T) {
	st := &store.Store{Dir: t.TempDir(), Session: "test"}
	issue := task.Link{URL: "https://linear.app/acme/issue/ABC-12"}
	setTask(t, st, &task.Task{Stage: task.Active, Links: append(harnessPages(2), issue)})
	m := newStrip(t, st, fakeHerdr(t))
	m.press("L")
	if view := plain(m.View()); !strings.Contains(view, "1 Linear ABC-12") || !strings.Contains(view, "2 Pages Acme harness page 1") {
		t.Errorf("unlink list doesn't number in grouped order:\n%s", view)
	}
	m.press("1")
	m.press("enter")
	if l := linksOfTab(t, st); len(l) != 2 || slices.ContainsFunc(l, func(l task.Link) bool { return l.URL == issue.URL }) {
		t.Errorf("links = %+v, want the two pages", l)
	}
	if m.msg != "Removed ABC-12" {
		t.Errorf("message = %q, want Removed ABC-12", m.msg)
	}
}

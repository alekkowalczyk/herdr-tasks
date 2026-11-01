package ui

import (
	"bufio"
	"encoding/json"
	"net"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/charmbracelet/bubbles/textarea"
	"github.com/charmbracelet/bubbles/textinput"
	tea "github.com/charmbracelet/bubbletea"

	"github.com/alekkowalczyk/herdr-tasks/internal/herdr"
	"github.com/alekkowalczyk/herdr-tasks/internal/reconcile"
	"github.com/alekkowalczyk/herdr-tasks/internal/store"
	"github.com/alekkowalczyk/herdr-tasks/internal/strip"
	"github.com/alekkowalczyk/herdr-tasks/internal/task"
)

// fakeHerdr answers like a herdr with one workspace, w1, holding one tab,
// w1:t1, that pane w1:p9 is in. Anything else gets an empty result.
func fakeHerdr(t *testing.T) *herdr.Client {
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
	const tab = `{"tab_id":"w1:t1","workspace_id":"w1","number":1,"label":"Acme"}`
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
				var req struct{ Method string }
				_ = json.Unmarshal(line, &req)
				result := `{}`
				switch req.Method {
				case "pane.get":
					result = `{"pane":{"pane_id":"w1:p9","tab_id":"w1:t1"}}`
				case "tab.get":
					result = `{"tab":` + tab + `}`
				case "tab.list":
					result = `{"tabs":[` + tab + `]}`
				case "workspace.list":
					result = `{"workspaces":[{"workspace_id":"w1","label":"Acme","number":1}]}`
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
	b := task.Link{URL: "https://github.com/acme/x/pull/2", By: "claude"}
	c := task.Link{URL: "https://github.com/acme/x/pull/3", By: "claude"}
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

	// You ask to remove link 2, PR #2.
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
	if view := m.View(); !slices.ContainsFunc(strings.Split(view, "\n"), func(l string) bool { return strings.Contains(plain(l), "2 Links PR #2") }) {
		t.Errorf("view doesn't list PR #2 as 2:\n%s", plain(view))
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
		t.Errorf("dismissed = %q, want PR #2 only", tk.DismissedLinks)
	}
	if m.msg != "Removed PR #2" {
		t.Errorf("message = %q, want Removed PR #2", m.msg)
	}
}

func TestUnlinkOfALinkGoneMeanwhileSaysSo(t *testing.T) {
	st := &store.Store{Dir: t.TempDir(), Session: "test"}
	b := task.Link{URL: "https://github.com/acme/x/pull/2", By: "claude"}
	c := task.Link{URL: "https://github.com/acme/x/pull/3", By: "claude"}
	setLinks(t, st, b, c)

	m := &stripModel{c: fakeHerdr(t), st: st, opts: reconcile.Options{}, pane: "w1:p9", rows: strip.Rows}
	m.input = textinput.New()
	m.note = textarea.New()
	m.width, m.height = 100, 14
	m.Update(m.load())
	m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("L")})
	m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("1")})
	// The board removes PR #2 before you press enter.
	setLinks(t, st, c)
	m.Update(tea.KeyMsg{Type: tea.KeyEnter})

	if l := linksOfTab(t, st); len(l) != 1 || l[0].URL != c.URL {
		t.Errorf("links = %+v, want PR #3 alone", l)
	}
	if m.msg != "PR #2 was removed already" {
		t.Errorf("message = %q, want PR #2 was removed already", m.msg)
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

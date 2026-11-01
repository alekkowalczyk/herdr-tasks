package ui

import (
	"slices"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/alekkowalczyk/herdr-tasks/internal/store"
	"github.com/alekkowalczyk/herdr-tasks/internal/task"
)

// unlinkCase is the race the overview's unlink tests play out: you see the links
// [plan.html, PR #2, PR #3] and pick PR #2, while the Claude hook publishes
// plan.html as an artifact, which takes the file's place in the list.
type unlinkCase struct {
	st        *store.Store
	m         *model
	b, c, art task.Link
	file      task.Link
}

func newUnlinkCase(t *testing.T) unlinkCase {
	t.Helper()
	st := &store.Store{Dir: t.TempDir(), Session: "test"}
	const src = "/tmp/acme/plan.html"
	u := unlinkCase{
		st:   st,
		file: task.Link{URL: task.FileURL(src), By: "claude"},
		b:    task.Link{URL: "https://github.com/acme/x/pull/2", By: "claude"},
		c:    task.Link{URL: "https://github.com/acme/x/pull/3", By: "claude"},
		art:  task.Link{URL: "https://claude.ai/code/artifact/00000000-0000-4000-8000-000000000001", Source: src, By: "claude"},
	}
	setLinks(t, st, u.file, u.b, u.c)
	m, err := newModel(Env{Client: fakeHerdr(t), Store: st, CurrentTab: "w1:t1"})
	if err != nil {
		t.Fatal(err)
	}
	m.width, m.height = 140, 30
	m.Update(tea.WindowSizeMsg{Width: 140, Height: 30})
	u.m = m
	return u
}

// publish is the hook replacing the file with its artifact.
func (u unlinkCase) publish(t *testing.T) { setLinks(t, u.st, u.b, u.c, u.art) }

// check fails the test unless PR #2 alone was removed and dismissed.
func (u unlinkCase) check(t *testing.T) {
	t.Helper()
	tk := func() *task.Task {
		s, err := u.st.Load()
		if err != nil {
			t.Fatal(err)
		}
		return s.Tasks["w1:t1"]
	}()
	var got []string
	for _, l := range tk.Links {
		got = append(got, l.URL)
	}
	if want := []string{u.c.URL, u.art.URL}; !slices.Equal(got, want) {
		t.Errorf("links = %q, want %q", got, want)
	}
	if !tk.Dismissed(u.b.URL, "") || tk.Dismissed(u.c.URL, "") {
		t.Errorf("dismissed = %q, want PR #2 only", tk.DismissedLinks)
	}
	if u.m.msg != "Removed PR #2" {
		t.Errorf("message = %q, want Removed PR #2", u.m.msg)
	}
}

func TestOverviewUnlinkRemovesTheLinkYouSaw(t *testing.T) {
	u := newUnlinkCase(t)
	u.m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("L")})
	u.publish(t)
	u.m.Update(refreshMsg{})
	if u.m.mode != modeUnlink {
		t.Fatal("the refresh ended unlinking")
	}
	// The numbers you're choosing from don't move.
	if view := u.m.View(); !slices.ContainsFunc(strings.Split(view, "\n"), func(l string) bool { return strings.Contains(plain(l), "2 PR #2") }) {
		t.Errorf("view doesn't list PR #2 as 2:\n%s", plain(view))
	}
	u.m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("2")})
	u.check(t)
}

func TestOverviewUnlinkClickRemovesTheLinkYouSaw(t *testing.T) {
	u := newUnlinkCase(t)
	u.m.View()
	y := -1
	for at, l := range u.m.linkAt {
		if l.URL == u.b.URL {
			y = at
		}
	}
	if y < 0 {
		t.Fatalf("PR #2 has no ×: %+v", u.m.linkAt)
	}
	// The hook writes after the overview drew the list, before you click.
	u.publish(t)
	u.m.Update(tea.MouseMsg{X: u.m.width - 1, Y: y, Action: tea.MouseActionPress, Button: tea.MouseButtonLeft})
	u.check(t)
}

func TestOverviewUnlinkOfALinkGoneMeanwhileSaysSo(t *testing.T) {
	u := newUnlinkCase(t)
	u.m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("L")})
	// The board removes PR #2 before you press 2.
	setLinks(t, u.st, u.file, u.c)
	u.m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("2")})
	if l := linksOfTab(t, u.st); len(l) != 2 || l[1].URL != u.c.URL {
		t.Errorf("links = %+v, want plan.html and PR #3", l)
	}
	if u.m.msg != "PR #2 was removed already" {
		t.Errorf("message = %q, want PR #2 was removed already", u.m.msg)
	}
	u.m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("L")})
	u.m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("7")})
	if u.m.msg != "No link 7" {
		t.Errorf("message = %q, want No link 7", u.m.msg)
	}
}

func TestOverviewSaysWhenALinkIsntAdded(t *testing.T) {
	st := &store.Store{Dir: t.TempDir(), Session: "test"}
	setLinks(t, st)
	m, err := newModel(Env{Client: fakeHerdr(t), Store: st, CurrentTab: "w1:t1"})
	if err != nil {
		t.Fatal(err)
	}
	add := func(url string) {
		m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("l")})
		m.input.SetValue(url)
		m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	}
	for _, bad := range []string{"https://example.com/100%done", "https://example.com/%0A"} {
		add(bad)
		if l := linksOfTab(t, st); len(l) != 0 {
			t.Errorf("%s: links = %+v, want none", bad, l)
		}
		if m.msg != "Not a valid link" {
			t.Errorf("%s: message = %q, want Not a valid link", bad, m.msg)
		}
	}
	add("https://github.com/acme/x/pull/2")
	if m.msg != "Added PR #2 to Acme" {
		t.Errorf("message = %q, want Added PR #2 to Acme", m.msg)
	}
	add("https://github.com/acme/x/pull/2")
	if l := linksOfTab(t, st); len(l) != 1 {
		t.Errorf("links = %+v, want PR #2 once", l)
	}
	if m.msg != "Already linked to Acme" {
		t.Errorf("message = %q, want Already linked to Acme", m.msg)
	}
}

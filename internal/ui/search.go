package ui

import (
	"slices"
	"strings"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"

	"github.com/alekkowalczyk/herdr-tasks/internal/claudelog"
	"github.com/alekkowalczyk/herdr-tasks/internal/herdr"
	"github.com/alekkowalczyk/herdr-tasks/internal/strip"
	"github.com/alekkowalczyk/herdr-tasks/internal/task"
	"github.com/alekkowalczyk/herdr-tasks/internal/view"
)

// The overview's search: / lists the tasks with what you type in their
// name first, then in a pane's name, then in a recap, then anywhere else,
// as the board's search does. ↵ opens the one chosen; esc goes back to the
// list, on it.

// searchData is what a search reads besides the state file: herdr's
// panes, and what each Claude pane's session log says, by pane ID.
type searchData struct {
	panes []herdr.Pane
	sums  map[string]claudelog.Summary
}

// searchLooked hands the overview what a look read.
type searchLooked struct{ data searchData }

// realLook reads herdr's panes through c, and the session logs of the
// panes running Claude, as the board does.
func realLook(c *herdr.Client) func() searchData {
	logs := claudelog.NewCache(claudelog.ConfigDir())
	return func() searchData {
		d := searchData{sums: map[string]claudelog.Summary{}}
		if c == nil {
			return d
		}
		panes, err := c.Panes()
		if err != nil {
			return d
		}
		d.panes = panes
		for _, p := range panes {
			if p.Agent != "claude" || p.Session == nil || p.Session.Kind != "id" {
				continue
			}
			if sum, ok, err := logs.Get(p.Session.Value); ok && err == nil {
				d.sums[p.ID] = sum
			}
		}
		return d
	}
}

// searching reports whether the list shows what a search found: you're
// searching, and have typed something.
func (m *model) searching() bool {
	return m.mode == modeSearch && len(view.Words(m.find.Value())) > 0
}

func (m *model) startSearch() tea.Cmd {
	m.mode = modeSearch
	m.find.SetValue("")
	m.find.Focus()
	m.search()
	return m.lookAgain()
}

// lookAgain reads herdr's panes and the session logs, unless a read is
// under way. Reading the logs takes a moment the first time, so it runs as
// a command, and the search reads names and notes meanwhile.
func (m *model) lookAgain() tea.Cmd {
	if m.looking || m.look == nil {
		return nil
	}
	m.looking = true
	look := m.look
	return func() tea.Msg { return searchLooked{look()} }
}

// endSearch goes back to the list, keeping the task you chose.
func (m *model) endSearch() {
	m.mode = modeList
	m.find.Blur()
	m.find.SetValue("")
	m.search()
}

func (m *model) searchKey(msg tea.KeyMsg) tea.Cmd {
	m.msg = ""
	switch msg.String() {
	case "ctrl+c":
		return m.quit("ctrl+c")
	case "esc":
		m.endSearch()
		return nil
	case "enter":
		if m.selected() == nil || m.searching() && len(m.found) == 0 {
			return nil
		}
		m.endSearch()
		return m.jump()
	case "up", "ctrl+p":
		m.move(-1)
		return nil
	case "down", "ctrl+n":
		m.move(1)
		return nil
	case "pgup":
		m.move(-10)
		return nil
	case "pgdown":
		m.move(10)
		return nil
	}
	was := m.find.Value()
	var cmd tea.Cmd
	m.find, cmd = m.find.Update(msg)
	if m.find.Value() != was {
		m.search()
		// What you typed picks the best it found.
		if len(m.found) > 0 {
			m.sel = m.found[0].tab.ID
		}
	}
	return cmd
}

// search finds what you typed among the rows, best first. The selection
// stays on a task it found, or else goes to the best.
func (m *model) search() {
	m.hits, m.found = nil, nil
	if !m.searching() {
		return
	}
	words := view.Words(m.find.Value())
	m.hits = map[string]view.Hit{}
	for _, r := range m.rows {
		if h := view.Search(words, fieldsOf(r, m.data)); h.Match != view.NoMatch {
			m.hits[r.tab.ID] = h
			m.found = append(m.found, r)
		}
	}
	slices.SortStableFunc(m.found, func(a, b row) int { return int(m.hits[a.tab.ID].Match - m.hits[b.tab.ID].Match) })
	if _, ok := m.hits[m.sel]; !ok && len(m.found) > 0 {
		m.sel = m.found[0].tab.ID
	}
}

// fieldsOf is what a search reads of r's task, as the board's page reads
// it of its card: its name, its panes' names and its agents' titles, their
// recaps, and everything else.
func fieldsOf(r row, d searchData) view.Fields {
	t := r.task
	f := view.Fields{Name: t.Name(r.pos)}
	add := func(list *[]string, s string) {
		if s = task.CleanText(s); strings.TrimSpace(s) != "" {
			*list = append(*list, s)
		}
	}
	var titles, said []string
	for _, p := range d.panes {
		if p.TabID != r.tab.ID || p.Label == strip.Label {
			continue
		}
		add(&f.Panes, p.Label)
		// Agent is empty once the agent exits.
		if p.Agent == "" {
			continue
		}
		sum := d.sums[p.ID]
		title := p.Title
		if title == "" {
			title = sum.Title
		}
		titles = append(titles, title)
		add(&f.Recaps, sum.Recap)
		said = append(said, sum.LastPrompt, sum.LastReply)
	}
	for _, s := range titles {
		add(&f.Panes, s)
	}
	add(&f.Content, t.Reason)
	add(&f.Content, t.Note)
	for _, e := range t.Log {
		add(&f.Content, e.Text)
	}
	for _, l := range t.Links {
		add(&f.Content, l.Display())
		add(&f.Content, l.URL)
	}
	for _, s := range said {
		add(&f.Content, s)
	}
	if t.Slack != nil {
		add(&f.Content, t.Slack.Place)
	}
	return f
}

// foundSections are the rows found, by where they were found, leaving out
// the places with none.
func (m *model) foundSections() []section {
	all := []section{
		{title: "NAME", color: cFg},
		{title: "PANE", hint: "pane names and agent titles", color: cRun},
		{title: "RECAP", hint: "Claude's recaps", color: cReview},
		{title: "CONTENT", hint: "note, log, links, prompts and replies", color: cDim},
	}
	for _, r := range m.found {
		i := int(m.hits[r.tab.ID].Match - view.InName)
		all[i].rows = append(all[i].rows, r)
	}
	return slices.DeleteFunc(all, func(s section) bool { return len(s.rows) == 0 })
}

// foundCell is where h was found, w columns wide on bg, with the word that
// found it bright. Text too long for it starts a little before the word, so
// the word shows.
func foundCell(h view.Hit, w int, bg lipgloss.Style) string {
	before, word, after := h.Text, "", ""
	if h.Len > 0 {
		before, word, after = h.Text[:h.At], h.Text[h.At:h.At+h.Len], h.Text[h.At+h.Len:]
	}
	if keep := w / 3; h.Len > 0 && ansi.StringWidth(h.Text) > w && ansi.StringWidth(before) > keep {
		all := []rune(before)
		rs := all
		for len(rs) > 0 && ansi.StringWidth(string(rs))+1 > keep {
			rs = rs[1:]
		}
		// It starts at a word, rather than in one, when a word starts there.
		if cut := len(all) - len(rs); all[cut-1] != ' ' {
			if i := slices.Index(rs, ' '); i >= 0 {
				rs = rs[i+1:]
			}
		}
		before = "…" + string(rs)
	}
	dim := bg.Foreground(cDim)
	out := dim.Render(fit(before, min(w, ansi.StringWidth(before))))
	left := w - ansi.StringWidth(before)
	if word != "" && left > 0 {
		out += bg.Foreground(cFg).Bold(true).Render(fit(word, min(left, ansi.StringWidth(word))))
		left -= min(left, ansi.StringWidth(word))
	}
	if left > 0 {
		out += dim.Render(fit(after, left))
	}
	return out
}

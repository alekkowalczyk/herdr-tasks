package ui

import (
	"slices"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"

	"github.com/alekkowalczyk/herdr-tasks/internal/herdr"
	"github.com/alekkowalczyk/herdr-tasks/internal/store"
	"github.com/alekkowalczyk/herdr-tasks/internal/task"
)

// The overview's closed tasks: c lists the tasks whose tabs closed, newest
// first, but those in hidden spaces, and ↵ reopens one in a new tab, as it
// was (see package reopen). A search finds them too, below the tasks.

// closedKey is a closed task's key in the list, where an open task's is its
// tab's ID.
func closedKey(id string) string { return "closed:" + id }

// closedRows are the closed tasks of s, newest first, but those in hidden
// spaces.
func closedRows(s *store.State) []row {
	var rows []row
	for i := range s.Closed {
		c := &s.Closed[i]
		if c.Task == nil || s.IsHidden(c.Task.Workspace) {
			continue
		}
		rows = append(rows, row{tab: herdr.Tab{ID: closedKey(c.ID)}, task: c.Task, ws: herdr.Workspace{Label: c.Task.Workspace}, closed: c})
	}
	return rows
}

// closedName is what the overview calls a closed task. With no title, it
// has no tab number either.
func closedName(t *task.Task) string {
	if title := task.CleanLabel(t.Title); title != "" {
		return title
	}
	return "untitled task"
}

// toggleClosed switches the list between the tasks and the closed ones, on
// the first of them unless the one chosen is there.
func (m *model) toggleClosed() {
	m.showClosed = !m.showClosed
	rows := m.listed()
	if len(rows) > 0 && !slices.ContainsFunc(rows, func(r row) bool { return r.tab.ID == m.sel }) {
		m.sel = rows[0].tab.ID
	}
}

// changesTask are the keys that change a task or act on its tab, which a
// closed task has none of.
var changesTask = map[string]bool{
	"a": true, "p": true, "v": true, "z": true, "d": true, "o": true, "f": true, "n": true,
	"l": true, "L": true, "r": true, "w": true, "m": true, "x": true, "b": true, "A": true,
}

// reopenedMsg says how reopening a closed task went: the tab it's in, when
// it got one, and what to say about it.
type reopenedMsg struct {
	tab, say string
	err      error
}

// reopenSelected reopens the selected closed task. Claude takes a few
// seconds to resume, so it runs as a command.
func (m *model) reopenSelected() tea.Cmd {
	r := m.selected()
	if r == nil || r.closed == nil || m.reopening {
		return nil
	}
	name := closedName(r.task)
	m.msg, m.reopening = "Reopening "+name+"…", true
	reopen, id := m.act.reopen, r.closed.ID
	return func() tea.Msg {
		got, err := reopen(id)
		return reopenedMsg{tab: got.Tab, say: got.Say(name), err: err}
	}
}

// reopened shows the reopened task's tab, which ends the overview. One that
// went wrong says why; when the task has its tab all the same, the list
// shows it.
func (m *model) reopened(msg reopenedMsg) tea.Cmd {
	m.reopening = false
	m.refresh()
	switch {
	case msg.err == nil:
		if err := m.env.Client.FocusTab(msg.tab); err != nil {
			m.msg = msg.say + ", but its tab didn't show: " + err.Error()
			return nil
		}
		return m.quit("enter")
	case msg.tab != "":
		m.showClosed, m.sel = false, msg.tab
	}
	m.msg = msg.err.Error()
	return nil
}

// closedDetailLines are the details of r, a closed task: what reopening it
// brings back.
func (m *model) closedDetailLines(r *row, w, h int) []string {
	t := r.task
	var lines []string
	add := func(s ...string) { lines = append(lines, s...) }

	add(sBold.Render(fit(closedName(t), w)))
	add(sDim.Render(fit(task.CleanLabel(t.Workspace)+" · closed "+task.Age(time.Since(r.closed.At))+" ago", w)))
	line := lipgloss.NewStyle().Foreground(stageColor(t.Stage)).Render(t.Stage.Glyph() + " " + t.Stage.Label())
	if t.Priority {
		line = sAsk.Render("! priority") + sDim.Render(" · ") + line
	}
	add(line)
	if t.Stage.TakesReason() && t.Reason != "" {
		for _, l := range wrap(reasonLabel(t.Stage)+" "+t.Reason, w) {
			add(lipgloss.NewStyle().Foreground(stageColor(t.Stage)).Render(l))
		}
	}
	add("")

	add(sDim.Render("NOTE"))
	add(noteLines(t, w)...)
	add("")

	add(sDim.Render("LINKS"))
	if len(t.Links) == 0 {
		add(sDim.Render("No links."))
	}
	section := ""
	for _, l := range task.Grouped(t.Links) {
		if s := task.KindOf(l.URL).Section(); s != section {
			section = s
			add(sectionName(s))
		}
		label := task.CleanLabel(l.Display())
		if section == "Slack" {
			label = linkItem(l)
		}
		if !task.SafeURL(l.URL) && l.Label == "" {
			label = "unsafe link"
		}
		add(fit(sLink.Render(hyperlink(l.URL, label)), w))
	}
	add("")

	add(sDim.Render("REOPEN") + strings.Repeat(" ", max(1, w-14)) + sDim.Render("↵ reopen"))
	ws := task.CleanLabel(t.Workspace)
	there := slices.ContainsFunc(m.live.Workspaces, func(x herdr.Workspace) bool { return strings.EqualFold(x.Label, t.Workspace) })
	if there {
		add(wrap("A new tab in "+ws+".", w)...)
	} else {
		add(wrap("A new tab in a new workspace, "+ws+".", w)...)
	}
	if t.Cwd != "" {
		add(wrap("In "+task.CleanLabel(t.Cwd)+".", w)...)
	}
	if t.Claude != "" {
		add(sRun.Render("Claude resumes its session there."))
	}
	if len(lines) > h {
		lines = lines[:h]
	}
	return lines
}

// noteLines are t's note and log, as the details show them.
func noteLines(t *task.Task, w int) []string {
	var lines []string
	if t.Note == "" && len(t.Log) == 0 {
		lines = append(lines, sDim.Render("No note yet."))
	}
	if t.Note != "" {
		lines = append(lines, wrap(t.Note, w)...)
	}
	for _, e := range t.Log {
		who := task.CleanLabel(e.Who) + " " + e.At.Local().Format("15:04")
		if time.Since(e.At) > 20*time.Hour {
			who = task.CleanLabel(e.Who) + " " + e.At.Local().Format("Mon 15:04")
		}
		body := wrap(e.Text, max(10, w-ansi.StringWidth(who)-1))
		for i, l := range body {
			prefix := strings.Repeat(" ", ansi.StringWidth(who))
			if i == 0 {
				prefix = sRun.Render(who)
			}
			lines = append(lines, prefix+" "+l)
		}
	}
	return lines
}

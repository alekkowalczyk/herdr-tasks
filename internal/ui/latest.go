package ui

import (
	"slices"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/alekkowalczyk/herdr-tasks/internal/store"
	"github.com/alekkowalczyk/herdr-tasks/internal/task"
)

// The overview's other order: s lists the tasks by Claude's latest reply,
// as the board's Latest reply does. Those an agent works on come first,
// then the rest, newest reply first, then those Claude hasn't replied in.
// The tools keep their place at the end. The state file keeps the order
// you chose.

// activity is what a look saw of a tab's agents: whether one works, and
// Claude's latest reply there.
type activity struct {
	working bool
	at      time.Time
	reply   string
}

// activities is the activity of each tab in d, by tab ID.
func activities(d searchData) map[string]activity {
	out := map[string]activity{}
	for _, p := range d.panes {
		a := out[p.TabID]
		if p.AgentStatus == "working" {
			a.working = true
		}
		if sum, ok := d.sums[p.ID]; ok && sum.ReplyAt.After(a.at) {
			a.at, a.reply = sum.ReplyAt, sum.LastReply
		}
		out[p.TabID] = a
	}
	return out
}

// latest reports whether the list is in the order of Claude's replies: you
// chose it, and aren't searching.
func (m *model) latest() bool { return m.state.OverviewLatest && !m.searching() }

// toggleLatest switches the list between the stages and Claude's replies,
// and reads the logs for the replies.
func (m *model) toggleLatest() tea.Cmd {
	on := !m.state.OverviewLatest
	err := m.env.Store.Update(func(s *store.State) error {
		s.OverviewLatest = on
		return nil
	})
	if err != nil {
		m.msg = "Couldn't save the order: " + err.Error()
		return nil
	}
	m.state.OverviewLatest = on
	if !on {
		return nil
	}
	return m.lookAgain()
}

// latestSections are the rows in the order of Claude's replies. Until the
// first look, no task has a reply.
func (m *model) latestSections() []section {
	none := "Claude hasn't replied in them"
	if m.acts == nil {
		none = "reading Claude's logs…"
	}
	work := section{title: "WORKING", hint: "an agent works on them", color: cRun}
	replied := section{title: "LATEST REPLY", hint: "newest first", color: cYou}
	quiet := section{title: "NO REPLY", hint: none, color: cDim}
	tools := section{}
	tools.title, tools.hint, tools.color = groupInfo(task.GTool)
	rows := slices.Clone(m.rows)
	slices.SortStableFunc(rows, func(a, b row) int { return m.acts[b.tab.ID].at.Compare(m.acts[a.tab.ID].at) })
	for _, r := range rows {
		a := m.acts[r.tab.ID]
		switch {
		case r.group == task.GTool:
			tools.rows = append(tools.rows, r)
		case a.working || r.tab.AgentStatus == "working":
			work.rows = append(work.rows, r)
		case !a.at.IsZero():
			replied.rows = append(replied.rows, r)
		default:
			quiet.rows = append(quiet.rows, r)
		}
	}
	return []section{work, replied, quiet, tools}
}

// Package board is the web board: one local server that shows the tasks of
// every running herdr session, what their agents are doing, and the status
// of their Linear issues.
package board

import (
	"slices"
	"sort"
	"time"

	"github.com/alekkowalczyk/herdr-tasks/internal/claudelog"
	"github.com/alekkowalczyk/herdr-tasks/internal/herdr"
	"github.com/alekkowalczyk/herdr-tasks/internal/linear"
	"github.com/alekkowalczyk/herdr-tasks/internal/slack"
	"github.com/alekkowalczyk/herdr-tasks/internal/store"
	"github.com/alekkowalczyk/herdr-tasks/internal/strip"
	"github.com/alekkowalczyk/herdr-tasks/internal/task"
	"github.com/alekkowalczyk/herdr-tasks/internal/view"
)

// Board is everything the page shows. The page reads these JSON names.
type Board struct {
	Version   string     `json:"version"`
	LinearKey bool       `json:"linearKey"`       // a Linear API key is set
	Slack     *SlackLine `json:"slack,omitempty"` // the Slack watcher's status, while it's on or offers a fetch
	Sessions  []Session  `json:"sessions"`        // by name
	// SlackSession is the herdr session Slack asks go to.
	SlackSession string `json:"slackSession"`
	// SlackWaiting says why the Slack line has no session to show it on:
	// the session Slack asks go to isn't running. It's only set by For.
	SlackWaiting string `json:"slackWaiting,omitempty"`
	// Tailnet is the board on the tailnet, while board_tailnet is on.
	Tailnet *TailnetLine `json:"tailnet,omitempty"`
}

// TailnetLine is the board on the tailnet, as the header shows it: its
// address while it's shared, or why it isn't, and the ports herdr-tasks
// shares.
type TailnetLine struct {
	Address string       `json:"address"`
	Problem string       `json:"problem,omitempty"`
	Shares  []SharedPort `json:"shares,omitempty"`
}

// SharedPort is a localhost port herdr-tasks shares with the tailnet.
type SharedPort struct {
	Port  int       `json:"port"`
	Since time.Time `json:"since"`
}

// For is the board of one running session: only that session, and the Slack
// line only on the session Slack asks go to. It's false when no running
// session has that name.
func (b Board) For(session string) (Board, bool) {
	i := slices.IndexFunc(b.Sessions, func(s Session) bool { return s.Name == session })
	if i < 0 {
		return Board{}, false
	}
	one := b
	one.Sessions = []Session{b.Sessions[i]}
	if session != b.SlackSession {
		one.Slack = nil
	}
	if b.Slack != nil && !slices.ContainsFunc(b.Sessions, func(s Session) bool { return s.Name == b.SlackSession }) {
		one.SlackWaiting = "Slack asks go to herdr session " + b.SlackSession + ", which isn't running. " + slack.OtherSession + "."
	}
	return one, true
}

// SessionList is what the board answers with when no session is asked for:
// the running sessions, for the page that lists them.
type SessionList struct {
	Version  string         `json:"version"`
	Slack    string         `json:"slack,omitempty"` // the session Slack asks go to, while the Slack line shows
	Sessions []SessionEntry `json:"sessions"`
	Tailnet  *TailnetLine   `json:"tailnet,omitempty"`
}

// SessionEntry is one running session in the list.
type SessionEntry struct {
	Name  string `json:"name"`
	Cards int    `json:"cards"`
	Stale bool   `json:"stale"`
}

// List is the running sessions, by name, and while the Slack line shows,
// the one Slack asks go to.
func (b Board) List() SessionList {
	l := SessionList{Version: b.Version, Sessions: []SessionEntry{}, Tailnet: b.Tailnet}
	if b.Slack != nil {
		l.Slack = b.SlackSession
	}
	for _, s := range b.Sessions {
		l.Sessions = append(l.Sessions, SessionEntry{Name: s.Name, Cards: len(s.Cards), Stale: s.Stale})
	}
	return l
}

// SlackLine is the Slack watcher's status, as the header shows it, with
// Fetch when it offers one, and the skipped asks.
type SlackLine struct {
	Text    string       `json:"text"`
	Problem bool         `json:"problem"`
	Fetch   bool         `json:"fetch"`
	Skipped []SkippedAsk `json:"skipped,omitempty"` // newest first
}

// SkippedAsk is an ask that made no task, as off-topic or maybe answered
// already, as the Slack line lists it.
type SkippedAsk struct {
	ID    string `json:"id"`    // its message ID, which POST /api/slack/take takes
	Label string `json:"label"` // as `slack skipped` shows it, without the number
	Why   string `json:"why"`   // "may be answered already", or "off-topic: lunch plans"
	Link  string `json:"link"`  // its permalink
}

// Session is one herdr session's part of the board.
type Session struct {
	Name   string  `json:"name"`
	Stale  bool    `json:"stale,omitempty"` // the last read failed; this is the last good one
	Spaces []Space `json:"spaces"`          // visible spaces, in herdr's order
	Cards  []Card  `json:"cards"`           // in the overview's order, without tools
}

type Space struct {
	ID    string `json:"id"`
	Label string `json:"label"`
}

// Card is one task.
type Card struct {
	Session    string `json:"session"`
	Tab        string `json:"tab"`
	Title      string `json:"title"`
	Space      string `json:"space"`
	SpaceLabel string `json:"spaceLabel"`
	// The overview's group: asking, yourturn, running, review, paused, parked or done.
	Column      string       `json:"column"`
	Stage       string       `json:"stage"`
	Since       time.Time    `json:"since"`
	Reason      string       `json:"reason,omitempty"`
	Priority    bool         `json:"priority"`
	AgentStatus string       `json:"agentStatus"`
	Note        string       `json:"note,omitempty"`
	Log         []task.Entry `json:"log,omitempty"`
	Links       []LinkView   `json:"links"`
	Agents      []Agent      `json:"agents"`
	// Tasks made from a Slack message only.
	Slack *CardSlack `json:"slack,omitempty"`
}

// CardSlack is what a card made from a Slack message shows of it.
type CardSlack struct {
	Place    string `json:"place"`    // "#billing", "a DM" or "a group DM"
	Channel  string `json:"channel"`  // its ID, which slack unmute takes
	Research string `json:"research"` // "", held, failed, locked or open
	// You muted the channel. It comes from the watcher's state, which
	// Build doesn't read: the server marks it.
	Muted bool `json:"muted"`
}

// LinkView is a link as a card shows it.
type LinkView struct {
	URL   string `json:"url"`
	Label string `json:"label"`
	By    string `json:"by,omitempty"`
	// What it points at, from task.KindOf. The page groups and shows links
	// by it.
	Kind string `json:"kind"`
	// File links only: the file's path.
	Path string `json:"path,omitempty"`
	// Slack messages only: when it was posted.
	At *time.Time `json:"at,omitempty"`
	// The local file a page was published from.
	Source string `json:"source,omitempty"`
	// Linear issues only: the fetched status, or the ID alone until it's fetched.
	Linear *linear.Issue `json:"linear,omitempty"`
}

// Agent is an agent running in one of the task's panes. The recap and the
// last prompt and reply come from Claude's session log.
type Agent struct {
	Pane string `json:"pane"`
	// The name the pane has in herdr, when it has one.
	Name       string     `json:"name,omitempty"`
	Agent      string     `json:"agent"`
	Status     string     `json:"status"`
	Title      string     `json:"title"`
	Recap      string     `json:"recap,omitempty"`
	RecapAt    *time.Time `json:"recapAt,omitempty"`
	RecapStale bool       `json:"recapStale,omitempty"` // a prompt came after the recap
	LastPrompt string     `json:"lastPrompt,omitempty"`
	LastReply  string     `json:"lastReply,omitempty"`
}

// Snapshot is the last good read of one herdr session. State and Live are
// nil until a read works.
type Snapshot struct {
	Name  string
	State *store.State
	Live  *herdr.Live
	Panes []herdr.Pane
	Stale bool
}

// Build makes the board from each session's snapshot. summary says what a
// Claude pane's session log holds, or false when there's no log; it's only
// asked about panes running Claude with a session ID. issues are the fetched
// Linear issues by ID.
func Build(snaps []Snapshot, summary func(herdr.Pane) (claudelog.Summary, bool), issues map[string]linear.Issue, linearKey bool, version string) Board {
	b := Board{Version: version, LinearKey: linearKey, Sessions: []Session{}}
	for _, snap := range snaps {
		b.Sessions = append(b.Sessions, buildSession(snap, summary, issues))
	}
	sort.SliceStable(b.Sessions, func(i, j int) bool { return b.Sessions[i].Name < b.Sessions[j].Name })
	return b
}

func buildSession(snap Snapshot, summary func(herdr.Pane) (claudelog.Summary, bool), issues map[string]linear.Issue) Session {
	s := Session{Name: snap.Name, Stale: snap.Stale, Spaces: []Space{}, Cards: []Card{}}
	if snap.State == nil || snap.Live == nil {
		return s
	}
	for _, ws := range snap.Live.Workspaces {
		if !snap.State.IsHidden(ws.Label) {
			s.Spaces = append(s.Spaces, Space{ID: ws.ID, Label: ws.Label})
		}
	}
	panes := map[string][]herdr.Pane{}
	for _, p := range snap.Panes {
		panes[p.TabID] = append(panes[p.TabID], p)
	}
	for _, r := range view.Rows(snap.State, snap.Live) {
		if r.Group == task.GTool {
			continue
		}
		s.Cards = append(s.Cards, buildCard(snap.Name, r, panes[r.Tab.ID], summary, issues))
	}
	return s
}

func buildCard(session string, r view.Row, panes []herdr.Pane, summary func(herdr.Pane) (claudelog.Summary, bool), issues map[string]linear.Issue) Card {
	t := r.Task
	c := Card{
		Session:     session,
		Tab:         r.Tab.ID,
		Title:       t.Name(r.Pos),
		Space:       r.Workspace.ID,
		SpaceLabel:  r.Workspace.Label,
		Column:      string(r.Group),
		Stage:       string(t.Stage),
		Since:       t.Since,
		Reason:      t.Reason,
		Priority:    t.Priority,
		AgentStatus: r.Tab.AgentStatus,
		Note:        t.Note,
		Log:         t.Log,
		Links:       []LinkView{},
		Agents:      []Agent{},
	}
	for _, l := range t.Links {
		c.Links = append(c.Links, linkView(l, issues))
	}
	if a := t.Slack; a != nil {
		c.Slack = &CardSlack{Place: task.CleanLabel(a.Place), Channel: task.CleanLabel(a.Channel), Research: string(a.Research)}
	}
	for _, p := range panes {
		// Agent is empty once the agent exits, though herdr keeps its session.
		if p.Agent != "" && p.Label != strip.Label {
			c.Agents = append(c.Agents, buildAgent(p, summary))
		}
	}
	return c
}

func linkView(l task.Link, issues map[string]linear.Issue) LinkView {
	kind := task.KindOf(l.URL)
	lv := LinkView{URL: l.URL, Label: l.Display(), By: l.By, Kind: string(kind), Source: l.Source}
	switch kind {
	case task.KindLinear:
		id, _ := task.LinearID(l.URL)
		issue, ok := issues[id]
		if !ok {
			issue = linear.Issue{ID: id}
		}
		lv.Linear = &issue
	case task.KindFile:
		lv.Path, _ = task.FilePath(l.URL)
	case task.KindSlack:
		if at, ok := task.SlackTime(l.URL); ok {
			lv.At = &at
		}
	}
	return lv
}

func buildAgent(p herdr.Pane, summary func(herdr.Pane) (claudelog.Summary, bool)) Agent {
	a := Agent{Pane: p.ID, Name: p.Label, Agent: p.Agent, Status: p.AgentStatus, Title: p.Title}
	if summary == nil || p.Agent != "claude" || p.Session == nil || p.Session.Kind != "id" {
		return a
	}
	sum, ok := summary(p)
	if !ok {
		return a
	}
	if a.Title == "" {
		a.Title = sum.Title
	}
	a.Recap, a.RecapStale = sum.Recap, sum.RecapStale
	if !sum.RecapAt.IsZero() {
		at := sum.RecapAt
		a.RecapAt = &at
	}
	a.LastPrompt, a.LastReply = sum.LastPrompt, sum.LastReply
	return a
}

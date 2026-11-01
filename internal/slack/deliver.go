package slack

import (
	"fmt"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/alekkowalczyk/herdr-tasks/internal/herdr"
	"github.com/alekkowalczyk/herdr-tasks/internal/reconcile"
	"github.com/alekkowalczyk/herdr-tasks/internal/store"
	"github.com/alekkowalczyk/herdr-tasks/internal/strip"
	"github.com/alekkowalczyk/herdr-tasks/internal/task"
)

// Herdr is what delivering asks and running research needs. strip.Host has it all.
type Herdr interface {
	reconcile.Herdr
	CreateWorkspace(label, cwd string) (herdr.Created, error)
	CreateTab(workspaceID, label, cwd string) (herdr.Created, error)
	CloseTab(tabID string) error
	Layout(tabID string) (*herdr.Node, error)
	StartAgent(name, kind, paneID string, args []string, timeout time.Duration) error
	Processes(paneID string) ([]herdr.Process, error)
}

var _ Herdr = strip.Host{}

// tasksOnly is a Herdr without its strips. A run through it changes tasks and
// the tab bar, and leaves strips to the next full run, so a strip herdr can't
// add to some tab doesn't fail a change it saved.
type tasksOnly struct{ reconcile.Herdr }

const (
	maxLabel   = 60  // runes in a tab label
	maxQuote   = 500 // runes of the message the note quotes
	maxLogText = 200 // runes of a follow-up's text in the log
	labelWords = 40  // runes of a link label's first words
	noteTime   = "Mon Jan 2 15:04"
	slackWho   = "slack" // who adds the links and log lines
)

// Delivered is where an ask went. Tab is the new tab, or the tab of the task
// it followed up. Research is true when a research session's start was tried,
// whether or not it started, and ResearchErr is why it didn't. Skipped is
// why the ask was skipped, which then went nowhere.
type Delivered struct {
	Tab                string
	FollowUp, Research bool
	ResearchErr        error
	Skipped            *Skip
}

// Skip is why an ask makes no task: you may have answered it already
// (Answered), or the topic check found it off-topic, and Why.
type Skip struct {
	Answered bool
	Why      string
}

// noWhy is why an off-topic ask was skipped when the check didn't say.
const noWhy = "not about the topic"

// Deliver puts an ask in front of you without taking the focus. A message in
// a conversation that has a task joins that task, wherever it was moved:
// its link and a log line are added, and it's made active again. Otherwise
// it gets a tab of its own in the set.Workspace workspace, made if it's
// missing, and a task with the ask's note and link. Then its research
// session starts, or, when research is false, the task says why it didn't.
// Before an ask makes a task, skip, unless it's nil, says whether to skip
// it, and why: a skipped ask makes nothing, and Delivered says why. A
// follow-up is never skipped.
// An error means the ask didn't reach a task, and a tab made for it is
// closed again. A research session that didn't start isn't one: the task
// shows that, and so does Delivered.
func Deliver(h Herdr, st *store.Store, opt reconcile.Options, set Settings, a Ask, exe, stateDir string, research bool, skip func(Ask) (Skip, bool), now time.Time) (Delivered, error) {
	// The task is looked for in the run, after closed tabs are dropped and
	// moved ones found.
	var joined string
	live, err := reconcile.Run(tasksOnly{h}, st, opt, now, func(s *store.State, _ *herdr.Live) error {
		if t := taskFor(s, a); t != nil {
			join(t, a, now)
			joined = t.TabID
		}
		return nil
	})
	if err != nil {
		return Delivered{}, err
	}
	if joined != "" {
		return Delivered{Tab: joined, FollowUp: true}, nil
	}
	if skip != nil {
		if k, ok := skip(a); ok {
			if !k.Answered && k.Why == "" {
				k.Why = noWhy
			}
			return Delivered{Skipped: &k}, nil
		}
	}

	c, err := newTab(h, live, set, a)
	if err != nil {
		return Delivered{}, err
	}
	_, err = reconcile.Run(tasksOnly{h}, st, opt, now, func(s *store.State, _ *herdr.Live) error {
		t, ok := s.Tasks[c.TabID]
		if !ok {
			return fmt.Errorf("herdr doesn't list the new tab %s", c.TabID)
		}
		fill(t, a, research, set.MaxNew, now)
		return nil
	})
	if err != nil {
		// The ask is fetched again next time, and gets a tab then; this one
		// would stay empty.
		_ = h.CloseTab(c.TabID)
		return Delivered{}, err
	}
	// The strip, as every tab gets one. Each run adds a strip that's missing,
	// so one herdr can't add now isn't the ask's failure.
	_, _ = reconcile.Run(h, st, opt, now, nil)

	d := Delivered{Tab: c.TabID, Research: research}
	if research {
		d.ResearchErr = StartResearch(h, st, opt, set, c.TabID, exe, stateDir, now)
	}
	return d, nil
}

// taskFor is the task for a's conversation, in any workspace. For a DM or a
// group DM it's one that isn't done: a done one has had its say.
func taskFor(s *store.State, a Ask) *task.Task {
	ids := make([]string, 0, len(s.Tasks))
	for id := range s.Tasks {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		t := s.Tasks[id]
		if t.Slack == nil || t.Slack.Key != a.Key || (a.Kind != KindMention && t.Stage == task.Done) {
			continue
		}
		return t
	}
	return nil
}

// join adds a follow-up to its conversation's task. Nothing is typed into
// the tab.
func join(t *task.Task, a Ask, now time.Time) {
	t.AddLink(a.Permalink, askLabel(a), slackWho, now)
	t.Log = append(t.Log, task.Entry{Who: slackWho, At: a.At, Text: author(a) + ": " + cut(flat(a.Text), maxLogText)})
	if t.Stage != task.Active {
		t.SetStage(task.Active, "", now)
	}
}

// newTab makes the ask's tab in the first workspace labelled set.Workspace,
// in any case, or in a new one, without focusing either.
func newTab(h Herdr, live *herdr.Live, set Settings, a Ask) (herdr.Created, error) {
	label := TabLabel(a)
	for _, w := range live.Workspaces {
		if strings.EqualFold(w.Label, set.Workspace) {
			return h.CreateTab(w.ID, label, set.Cwd)
		}
	}
	c, err := h.CreateWorkspace(set.Workspace, set.Cwd)
	if err != nil {
		return c, err
	}
	// A workspace comes with a tab herdr names. The task's title names it
	// too, but with stage markers off only the label keeps the title.
	_ = h.RenameTab(c.TabID, label)
	return c, nil
}

// fill makes the new tab's task the ask's.
func fill(t *task.Task, a Ask, research bool, maxNew int, now time.Time) {
	t.Title = TabLabel(a)
	t.Note = NoteFor(a)
	t.AddLink(a.Permalink, askLabel(a), slackWho, now)
	t.Slack = &task.SlackAsk{
		Key: a.Key, Channel: a.Channel, Place: place(a), Author: author(a),
		TS: a.TS, ThreadTS: a.ThreadTS, At: a.At, Permalink: a.Permalink,
	}
	if !research {
		t.Slack.Research = task.ResearchHeld
		t.Log = append(t.Log, task.Entry{Who: slackWho, At: now, Text: heldLine(maxNew)})
	}
}

// heldLine says why research wasn't started, past the hourly limit, and how
// to start it.
func heldLine(maxNew int) string {
	why := "slack_max_new is 0"
	switch {
	case maxNew == 1:
		why = "1 research session already started this hour (slack_max_new)"
	case maxNew > 1:
		why = fmt.Sprintf("%d research sessions already started this hour (slack_max_new)", maxNew)
	}
	return held(why)
}

// held says research wasn't started, why, and how to start it.
func held(why string) string {
	return "Research not started: " + why + ". Press s in the strip or Start research on the board to start it."
}

// TabLabel is the ask's tab label, "Dana Reed: ship the invoice fix Friday?",
// cleaned and at most 60 runes.
func TabLabel(a Ask) string { return cut(task.CleanLabel(author(a)+": "+a.Gist), maxLabel) }

// NoteFor is the new task's note: who asked, where and when, and the first
// 500 runes of the message, quoted.
//
//	Dana Reed in #billing, Fri Oct 31 14:32:
//	> <@U0ME00001> can the invoice fix ship Friday?
func NoteFor(a Ask) string {
	var b strings.Builder
	fmt.Fprintf(&b, "%s in %s, %s:", author(a), place(a), a.At.Format(noteTime))
	if text := strings.TrimSpace(task.CleanText(a.Text)); text != "" {
		for _, line := range strings.Split(cut(text, maxQuote), "\n") {
			b.WriteString("\n" + strings.TrimRight("> "+line, " "))
		}
	}
	return b.String()
}

// LinkLabel is a Slack link's label: where, who and what, as far as each is
// known, "#billing · Dana Reed: can you check the invoice fix before…".
// place is "#billing", "a DM" or "a group DM"; the label says "DM" or
// "group DM". The text's first words fill at most 40 runes, without Slack's
// mention tokens. An empty part goes, with its separator. The label is
// cleaned.
func LinkLabel(place, author, text string) string {
	switch place = task.CleanLabel(place); place {
	case "a DM":
		place = "DM"
	case "a group DM":
		place = "group DM"
	}
	author = task.CleanLabel(author)
	words := firstWords(task.CleanLabel(unmention(text)), labelWords)
	label := place
	if author != "" {
		label = joinLabel(label, author)
	}
	switch {
	case words == "":
	case author != "":
		label += ": " + words
	default:
		label = joinLabel(label, words)
	}
	return task.CleanLabel(label)
}

// mention is a person's mention token in a Slack message: <@U0123ABCD>, or
// <@U0123ABCD|name>.
var mention = regexp.MustCompile(`<@[A-Za-z0-9]+(?:\|([^<>|]*))?>`)

// unmention takes the mention tokens out of a message's text. One that
// carries a name reads "@name".
func unmention(text string) string {
	return mention.ReplaceAllStringFunc(text, func(token string) string {
		if name := mention.FindStringSubmatch(token)[1]; name != "" {
			return "@" + name
		}
		return ""
	})
}

// joinLabel joins two parts of a label with " · ".
func joinLabel(a, b string) string {
	if a == "" {
		return b
	}
	return a + " · " + b
}

// askLabel is the label of the ask's link.
func askLabel(a Ask) string { return LinkLabel(place(a), author(a), a.Text) }

// MessageLabel is the label of a search result's link: where it is, who
// wrote it, and its first words.
func MessageLabel(m Message) string { return LinkLabel(messagePlace(m), authorOf(m), m.Text) }

// PlaceLabel is the label of a search result's link with only where it is:
// "#billing", "DM" or "group DM".
func PlaceLabel(m Message) string { return LinkLabel(messagePlace(m), "", "") }

// messagePlace is where a search result is: "#billing", "a DM" or "a group
// DM".
func messagePlace(m Message) string {
	switch {
	case strings.HasPrefix(m.Channel, "D"):
		return "a DM"
	case dmLike(m):
		return "a group DM"
	}
	return channelPlace(m)
}

// author is who asked, cleaned, or their user ID when Slack gave no name.
func author(a Ask) string { return authorOf(a.Message) }

// authorOf is who wrote m, cleaned, or their user ID when Slack gave no name.
func authorOf(m Message) string {
	if name := task.CleanLabel(m.Author); name != "" {
		return name
	}
	return m.AuthorID
}

// place is where the ask was: "#billing", "a DM" or "a group DM".
func place(a Ask) string {
	switch a.Kind {
	case KindDM:
		return "a DM"
	case KindGroup:
		return "a group DM"
	}
	return channelPlace(a.Message)
}

// channelPlace is the channel m is in, "#billing", or "#" and its ID when
// Slack gave no name.
func channelPlace(m Message) string {
	name := bare(task.CleanLabel(m.Place))
	if name == "" {
		name = m.Channel
	}
	return "#" + name
}

// flat is s cleaned and on one line.
func flat(s string) string { return strings.Join(strings.Fields(task.CleanText(s)), " ") }

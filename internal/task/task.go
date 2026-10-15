// Package task holds the per-tab task model: stage, note, links, and how a
// task is grouped and labeled.
package task

import (
	"fmt"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"
)

type Stage string

const (
	Active Stage = "active"
	// Your part is done; waiting on approval, merge, or testing.
	Review Stage = "review"
	Paused Stage = "paused"
	Parked Stage = "parked"
	Done   Stage = "done"
	// Not a task: a tab that stays open for a tool or a running app.
	Tool Stage = "tool"
)

// Stages in tab-bar order.
var Stages = []Stage{Active, Review, Paused, Parked, Done, Tool}

func ParseStage(s string) (Stage, bool) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "active", "a":
		return Active, true
	case "review", "in-review", "inreview", "in review", "v":
		return Review, true
	case "paused", "pause", "p":
		return Paused, true
	case "parked", "park", "z":
		return Parked, true
	case "done", "d":
		return Done, true
	case "tool", "o":
		return Tool, true
	}
	return "", false
}

func (s Stage) Glyph() string {
	switch s {
	case Tool:
		return "◇"
	case Review:
		return "◐"
	case Paused:
		return "‖"
	case Parked:
		return "○"
	case Done:
		return "✓"
	}
	return "●"
}

func (s Stage) Label() string {
	switch s {
	case Tool:
		return "Tool"
	case Review:
		return "In review"
	case Paused:
		return "Paused"
	case Parked:
		return "Parked"
	case Done:
		return "Done"
	}
	return "Active"
}

func (s Stage) Order() int {
	for i, st := range Stages {
		if st == s {
			return i
		}
	}
	return 0
}

type Link struct {
	URL   string    `json:"url"`
	Label string    `json:"label,omitempty"`
	By    string    `json:"by,omitempty"`
	Added time.Time `json:"added"`
}

func (l Link) Display() string {
	if l.Label != "" {
		return l.Label
	}
	return LinkLabel(l.URL)
}

var ticketID = regexp.MustCompile(`(?i)^[a-z][a-z0-9]*-\d+$`)

// LinkLabel names a link by what it points at: "PR #431", "MWP-418", "Figma".
func LinkLabel(raw string) string {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return raw
	}
	host := strings.TrimPrefix(strings.ToLower(u.Hostname()), "www.")
	parts := strings.FieldsFunc(u.Path, func(r rune) bool { return r == '/' })
	switch {
	case host == "github.com":
		if len(parts) >= 4 && parts[2] == "pull" {
			return "PR #" + parts[3]
		}
		if len(parts) >= 4 && parts[2] == "issues" {
			return "issue #" + parts[3]
		}
		if len(parts) >= 2 {
			return parts[0] + "/" + parts[1]
		}
		return "GitHub"
	case host == "gitlab.com" || strings.HasPrefix(host, "gitlab."):
		for i, p := range parts {
			if p == "merge_requests" && i+1 < len(parts) {
				return "MR !" + parts[i+1]
			}
		}
	case strings.HasSuffix(host, "linear.app"):
		for _, p := range parts {
			if ticketID.MatchString(p) {
				return strings.ToUpper(p)
			}
		}
		return "Linear"
	case strings.HasSuffix(host, "atlassian.net"):
		for _, p := range parts {
			if ticketID.MatchString(p) {
				return strings.ToUpper(p)
			}
		}
		return "Jira"
	case strings.HasSuffix(host, "figma.com"):
		return "Figma"
	case strings.HasSuffix(host, "notion.so") || strings.HasSuffix(host, "notion.site"):
		return "Notion page"
	case host == "docs.google.com":
		return "Google Doc"
	}
	return host
}

// Entry is a line someone other than you added to the task, usually an agent.
type Entry struct {
	Who  string    `json:"who"`
	At   time.Time `json:"at"`
	Text string    `json:"text"`
}

type Task struct {
	TabID string `json:"tab_id"`
	// Workspace label, used to find the tab again if herdr hands out new IDs.
	Workspace string    `json:"workspace"`
	Title     string    `json:"title,omitempty"`
	Stage     Stage     `json:"stage"`
	Since     time.Time `json:"since"`
	Reason    string    `json:"reason,omitempty"`
	// A priority task shows in the sidebar with a red ! whatever its stage.
	Priority bool    `json:"priority,omitempty"`
	Note     string  `json:"note,omitempty"`
	Log      []Entry `json:"log,omitempty"`
	Links    []Link  `json:"links,omitempty"`
	// The tab label this plugin last set, so its own renames aren't mistaken for yours.
	Applied string `json:"applied,omitempty"`
	// The pane showing this task's strip. You can turn a task's strip off,
	// and turn one on for a tool tab, which has none by default.
	StripPane string `json:"strip_pane,omitempty"`
	StripOff  bool   `json:"strip_off,omitempty"`
	StripOn   bool   `json:"strip_on,omitempty"`
}

// WantsStrip reports whether the tab should show a strip.
func (t *Task) WantsStrip() bool {
	if t.Stage == Tool {
		return t.StripOn
	}
	return !t.StripOff
}

// SetStrip records that you showed or hid the tab's strip. It only affects
// the tab as it is now: hiding a task's strip doesn't hide a strip you gave
// it as a tool, and showing one on a tool doesn't carry over to the task.
func (t *Task) SetStrip(show bool) {
	if t.Stage == Tool {
		t.StripOn = show
	} else {
		t.StripOff = !show
	}
}

// IsTask reports whether the tab counts as work, unlike a tool tab.
func (s Stage) IsTask() bool { return s != Tool }

func (t *Task) WorkspaceID() string {
	if i := strings.IndexByte(t.TabID, ':'); i > 0 {
		return t.TabID[:i]
	}
	return ""
}

// TakesReason reports whether a stage records what it's waiting on.
func (s Stage) TakesReason() bool { return s == Paused || s == Review }

// SetStage moves the task to a stage. A reason only applies to Paused and
// In review; moving between those two keeps it unless a new one is given.
func (t *Task) SetStage(s Stage, reason string, now time.Time) {
	if t.Stage != s {
		t.Stage = s
		t.Since = now
	}
	switch {
	case !s.TakesReason():
		t.Reason = ""
	case reason != "":
		t.Reason = reason
	}
	if !s.TakesPriority() {
		t.Priority = false
	}
}

// TakesPriority reports whether a task in this stage can be a priority:
// done tasks are finished, and tools aren't work.
func (s Stage) TakesPriority() bool { return s != Done && s != Tool }

// SetPriority marks the task as a priority, or not.
func (t *Task) SetPriority(on bool) error {
	if on && !t.Stage.TakesPriority() {
		return fmt.Errorf("a %s task can't be a priority", strings.ToLower(t.Stage.Label()))
	}
	t.Priority = on
	return nil
}

func (t *Task) AddLink(raw, label, by string, now time.Time) Link {
	for _, l := range t.Links {
		if l.URL == raw {
			return l
		}
	}
	l := Link{URL: raw, Label: label, By: by, Added: now}
	t.Links = append(t.Links, l)
	return l
}

// RemoveLink removes a link by 1-based number or by URL.
func (t *Task) RemoveLink(which string) (Link, error) {
	if n, err := strconv.Atoi(which); err == nil {
		if n < 1 || n > len(t.Links) {
			return Link{}, fmt.Errorf("no link %d (this task has %d)", n, len(t.Links))
		}
		l := t.Links[n-1]
		t.Links = append(t.Links[:n-1], t.Links[n:]...)
		return l, nil
	}
	for i, l := range t.Links {
		if l.URL == which {
			t.Links = append(t.Links[:i], t.Links[i+1:]...)
			return l, nil
		}
	}
	return Link{}, fmt.Errorf("no link %q", which)
}

// Name is what the overview calls the task.
func (t *Task) Name(position int) string {
	if t.Title != "" {
		return t.Title
	}
	if position > 0 {
		return fmt.Sprintf("tab %d", position)
	}
	return "tab"
}

// Group is where the overview lists a task.
type Group string

const (
	// The agent stopped mid-work to ask a question or for permission.
	Asking Group = "asking"
	// The agent finished, or nothing is running.
	YourTurn Group = "yourturn"
	Running  Group = "running"
	GReview  Group = "review"
	GPaused  Group = "paused"
	GParked  Group = "parked"
	GDone    Group = "done"
	GTool    Group = "tool"
)

var Groups = []Group{Asking, YourTurn, Running, GReview, GPaused, GParked, GDone, GTool}

// GroupOf splits active tasks by what their agents are doing.
func GroupOf(s Stage, agentStatus string) Group {
	switch s {
	case Tool:
		return GTool
	case Review:
		return GReview
	case Paused:
		return GPaused
	case Parked:
		return GParked
	case Done:
		return GDone
	}
	switch agentStatus {
	case "working":
		return Running
	case "blocked":
		return Asking
	}
	return YourTurn
}

// StripMarker removes a stage marker this plugin put in front of a tab label.
func StripMarker(label string) string {
	for _, s := range Stages {
		if rest, ok := strings.CutPrefix(label, s.Glyph()+" "); ok {
			return rest
		}
	}
	return label
}

// TitleFromLabel reads a tab label as a task title. herdr names unnamed tabs
// by their position, and those stay unnamed ("") so the number can follow
// the tab when it moves.
func TitleFromLabel(label string, position int) string {
	title := strings.TrimSpace(StripMarker(label))
	if title == "" || title == strconv.Itoa(position) {
		return ""
	}
	return title
}

// MarkedLabel is the tab label with its stage marker.
func MarkedLabel(s Stage, title string, position int) string {
	if title == "" {
		title = strconv.Itoa(position)
	}
	return s.Glyph() + " " + title
}

// Age is a short duration: 4m, 3h, 2d.
func Age(d time.Duration) string {
	switch {
	case d < time.Minute:
		return "now"
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	case d < 48*time.Hour:
		return fmt.Sprintf("%dh", int(d.Hours()))
	}
	return fmt.Sprintf("%dd", int(d.Hours()/24))
}

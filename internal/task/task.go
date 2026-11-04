// Package task holds the per-tab task model: stage, note, links, and how a
// task is grouped and labeled.
package task

import (
	"fmt"
	"net/url"
	"path/filepath"
	"regexp"
	"slices"
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
	// The local file a page was published from.
	Source string `json:"source,omitempty"`
}

func (l Link) Display() string {
	if l.Label != "" {
		return l.Label
	}
	return LinkLabel(l.URL)
}

var ticketID = regexp.MustCompile(`(?i)^[a-z][a-z0-9]*-\d+$`)

// LinkLabel names a link by what it points at: "PR #431", "ABC-418",
// "Slack message", "plan.html", "localhost:4100/showcase".
func LinkLabel(raw string) string {
	switch KindOf(raw) {
	case KindGitHubPR:
		_, n, _ := GitHubPR(raw)
		return "PR #" + strconv.Itoa(n)
	case KindSlack:
		if _, ok := SlackTime(raw); ok {
			return "Slack message"
		}
		return "Slack channel"
	case KindArtifact:
		return "Artifact"
	case KindFile:
		p, _ := FilePath(raw)
		return CleanLabel(filepath.Base(p))
	case KindLocal, KindHosted:
		// Host and path; the fragment too, as it can pick the page.
		u, _ := url.Parse(raw)
		label := strings.ToLower(u.Host) + strings.TrimRight(u.Path, "/")
		if u.Fragment != "" {
			label += "#" + u.Fragment
		}
		return CleanLabel(label)
	}
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return raw
	}
	host := strings.TrimPrefix(strings.ToLower(u.Hostname()), "www.")
	parts := strings.FieldsFunc(u.Path, func(r rune) bool { return r == '/' })
	switch {
	case host == "github.com":
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

// LinearID returns the issue ID a Linear issue URL points at, as "ABC-12".
func LinearID(raw string) (string, bool) {
	_, id, ok := linearIssue(raw)
	return id, ok
}

// LinearOrg returns the workspace part of a Linear issue URL, as "acme".
func LinearOrg(raw string) (string, bool) {
	org, _, ok := linearIssue(raw)
	return org, ok
}

// LinearURL is the plain URL of a Linear issue, without the title slug.
func LinearURL(org, id string) string {
	return "https://linear.app/" + org + "/issue/" + strings.ToUpper(id)
}

// linearIssue reads https://linear.app/<org>/issue/<ID>[/<slug>].
func linearIssue(raw string) (org, id string, ok bool) {
	u, err := url.Parse(raw)
	if err != nil || (u.Scheme != "https" && u.Scheme != "http") {
		return "", "", false
	}
	if host := strings.ToLower(u.Hostname()); host != "linear.app" && host != "www.linear.app" {
		return "", "", false
	}
	parts := strings.Split(strings.Trim(u.EscapedPath(), "/"), "/")
	if len(parts) < 3 || len(parts) > 4 || parts[0] == "" || parts[1] != "issue" || !ticketID.MatchString(parts[2]) {
		return "", "", false
	}
	return parts[0], strings.ToUpper(parts[2]), true
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
	// Linear issues you removed, so agents don't add them back.
	DismissedIssues []string `json:"dismissed_issues,omitempty"`
	// Keys of other links you removed, so agents don't add them back.
	DismissedLinks []string `json:"dismissed_links,omitempty"`
	// The tab label this plugin last set, so its own renames aren't mistaken for yours.
	Applied string `json:"applied,omitempty"`
	// The pane showing this task's strip. You can turn a task's strip off,
	// and turn one on for a tool tab, which has none by default.
	StripPane string `json:"strip_pane,omitempty"`
	StripOff  bool   `json:"strip_off,omitempty"`
	StripOn   bool   `json:"strip_on,omitempty"`
	// Set on a task made from a Slack message.
	Slack *SlackAsk `json:"slack,omitempty"`
}

// SlackAsk is what a task made from a Slack message keeps about it. It
// moves with the task.
type SlackAsk struct {
	// The conversation: "C0123ABCD/1793450000.000100" for a channel thread,
	// "D0RUI0001" for a DM or group DM.
	Key       string    `json:"key"`
	Channel   string    `json:"channel"`
	Place     string    `json:"place"`               // "#billing", "a DM" or "a group DM"
	Author    string    `json:"author"`              // cleaned name
	TS        string    `json:"ts"`                  // the first message's ts
	ThreadTS  string    `json:"thread_ts,omitempty"` // its thread's ts; "" for a top-level message or a DM
	At        time.Time `json:"at"`                  // when the first message was posted
	Permalink string    `json:"permalink"`
	// The research session's Claude session ID.
	SessionID string   `json:"session_id,omitempty"`
	Research  Research `json:"research,omitempty"`
}

// Research is where a Slack ask's research session stands.
type Research string

const (
	ResearchHeld   Research = "held"   // not started: over the hourly limit, or slack_max_new = 0
	ResearchFailed Research = "failed" // starting it failed; the task's log says why
	ResearchLocked Research = "locked" // running with the research tools only
	ResearchOpen   Research = "open"   // continued with your tools
)

// CanStart reports whether research can be started: it hasn't been, or it
// was held or failed.
func (a *SlackAsk) CanStart() bool {
	return a.Research == "" || a.Research == ResearchHeld || a.Research == ResearchFailed
}

// CanContinue reports whether the locked session can be continued with your
// tools.
func (a *SlackAsk) CanContinue() bool {
	return a.Research == ResearchLocked && a.SessionID != ""
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

// AddLink adds a link as Add does. by is "" for you, or the agent's name.
func (t *Task) AddLink(raw, label, by string, now time.Time) (Link, bool) {
	return t.Add(Link{URL: raw, Label: label, By: by, Added: now})
}

// Add adds l unless the task has a link with the same LinkKey, and reports
// whether it did. It refuses a URL that isn't SafeURL, and cleans the label.
// An agent (l.By != "") can't add back a link you removed; adding it
// yourself takes it off the dismissed lists.
func (t *Task) Add(l Link) (Link, bool) {
	if !SafeURL(l.URL) {
		return Link{}, false
	}
	key := LinkKey(l.URL)
	for _, o := range t.Links {
		if LinkKey(o.URL) == key {
			return o, false
		}
	}
	if l.By != "" && t.Dismissed(l.URL, l.Source) {
		return Link{}, false
	}
	if l.By == "" {
		t.undismiss(l.URL)
		if l.Source != "" {
			t.undismiss(FileURL(l.Source))
		}
	}
	l.Label = CleanLabel(l.Label)
	t.Links = append(t.Links, l)
	return l, true
}

// Dismissed reports whether agents may not add raw: its key, or for an
// artifact with a source, its source file's key, was removed.
func (t *Task) Dismissed(raw, source string) bool {
	if t.dismissed(raw) {
		return true
	}
	return source != "" && t.dismissed(FileURL(source))
}

func (t *Task) dismissed(raw string) bool {
	if id, ok := LinearID(raw); ok {
		return slices.Contains(t.DismissedIssues, id)
	}
	return slices.Contains(t.DismissedLinks, LinkKey(raw))
}

// Dismiss keeps raw off the task, so agents don't add it: a Linear issue by
// its ID, anything else by its key. It reports whether raw wasn't dismissed
// already. It leaves the task's links as they are.
func (t *Task) Dismiss(raw string) bool {
	if id, ok := LinearID(raw); ok {
		if slices.Contains(t.DismissedIssues, id) {
			return false
		}
		t.DismissedIssues = append(t.DismissedIssues, id)
		return true
	}
	key := LinkKey(raw)
	if slices.Contains(t.DismissedLinks, key) {
		return false
	}
	t.DismissedLinks = append(t.DismissedLinks, key)
	return true
}

func (t *Task) undismiss(raw string) {
	if id, ok := LinearID(raw); ok {
		t.DismissedIssues = slices.DeleteFunc(t.DismissedIssues, func(d string) bool { return d == id })
		return
	}
	key := LinkKey(raw)
	t.DismissedLinks = slices.DeleteFunc(t.DismissedLinks, func(d string) bool { return d == key })
}

// Grouped is links in Sections order, as the strip, the overview and the
// board show them, in a new slice. Links keep their order within a section.
// A link's number, wherever one is shown or taken, is its place here.
func Grouped(links []Link) []Link {
	out := make([]Link, 0, len(links))
	for _, i := range groupedOrder(links) {
		out = append(out, links[i])
	}
	return out
}

// groupedOrder is the indexes of links in Grouped order.
func groupedOrder(links []Link) []int {
	order := make([]int, len(links))
	section := make([]int, len(links))
	for i, l := range links {
		order[i] = i
		section[i] = slices.Index(Sections, KindOf(l.URL).Section())
	}
	slices.SortStableFunc(order, func(a, b int) int { return section[a] - section[b] })
	return order
}

// RemoveLink removes a link by its 1-based number in Grouped order, by URL,
// or by Linear issue ID.
// The link goes under all its URLs, and is dismissed, so agents don't add it
// back. A page published from a file dismisses that file too, and removing
// a file removes and dismisses the artifacts an agent published from it.
func (t *Task) RemoveLink(which string) (Link, error) {
	i, err := t.findLink(which)
	if err != nil {
		return Link{}, err
	}
	l := t.Links[i]
	// State can hold the same link under several URLs: from before 0.6.0 for
	// Linear issues, and from before 0.9.0 for everything else.
	key := LinkKey(l.URL)
	t.Links = slices.DeleteFunc(t.Links, func(o Link) bool { return LinkKey(o.URL) == key })
	t.Dismiss(l.URL)
	if l.Source != "" {
		t.Dismiss(FileURL(l.Source))
	}
	// The other way round: artifacts an agent published from a removed file
	// go too, and are dismissed, so a reply naming one without its file
	// doesn't add it back. One you linked yourself stays, and isn't dismissed.
	if p, ok := FilePath(l.URL); ok {
		yours := map[string]bool{}
		for _, o := range t.Links {
			if o.By == "" {
				yours[LinkKey(o.URL)] = true
			}
		}
		for _, o := range slices.Clone(t.Links) {
			k := LinkKey(o.URL)
			if o.By != "" && o.Source != "" && KindOf(o.URL) == KindArtifact && filepath.Clean(o.Source) == p && !yours[k] {
				t.Links = slices.DeleteFunc(t.Links, func(x Link) bool { return LinkKey(x.URL) == k })
				t.Dismiss(o.URL)
			}
		}
	}
	return l, nil
}

// findLink finds a link's index in t.Links by its number in Grouped order,
// by its exact URL, by another URL for the same link, then by Linear issue ID.
func (t *Task) findLink(which string) (int, error) {
	if n, err := strconv.Atoi(which); err == nil {
		if n < 1 || n > len(t.Links) {
			return 0, fmt.Errorf("no link %d (this task has %d)", n, len(t.Links))
		}
		return groupedOrder(t.Links)[n-1], nil
	}
	for i, l := range t.Links {
		if l.URL == which {
			return i, nil
		}
	}
	key := LinkKey(which)
	for i, l := range t.Links {
		if LinkKey(l.URL) == key {
			return i, nil
		}
	}
	if ticketID.MatchString(which) {
		id := strings.ToUpper(which)
		for i, l := range t.Links {
			if lid, _ := LinearID(l.URL); lid == id {
				return i, nil
			}
		}
	}
	return 0, fmt.Errorf("no link %q", which)
}

// Name is what the overview calls the task. The title can come from an agent
// or a hand-edited state file, so it's cleaned as a label is.
func (t *Task) Name(position int) string {
	if title := CleanLabel(t.Title); title != "" {
		return title
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

// Order is the group's place in Groups.
func (g Group) Order() int {
	for i, gr := range Groups {
		if gr == g {
			return i
		}
	}
	return 0
}

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

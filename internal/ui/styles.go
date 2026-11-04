package ui

import (
	"net/url"
	"strings"

	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"

	"github.com/alekkowalczyk/herdr-tasks/internal/task"
)

var (
	cDim    = lipgloss.Color("#7b8490")
	cFg     = lipgloss.Color("#d6dbe1")
	cActive = lipgloss.Color("#62d39d")
	cPaused = lipgloss.Color("#e7b75a")
	cParked = lipgloss.Color("#93a5c6")
	cDone   = lipgloss.Color("#6f7884")
	cRun    = lipgloss.Color("#5ec9e8")
	cYou    = lipgloss.Color("#ff8d6e")
	cAsk    = lipgloss.Color("#ff5f6d")
	cReview = lipgloss.Color("#b9a6ff")
	cTool   = lipgloss.Color("#8a96a8")
	cLink   = lipgloss.Color("#9cc4ff")
	cLinear = lipgloss.Color("#8f8cf2")
	cGitHub = lipgloss.Color("#f0f3f6")
	cSlack  = lipgloss.Color("#c965a9")
	cPages  = lipgloss.Color("#4fb86e")
	cSelBG  = lipgloss.Color("#232b35")
	cChipBG = lipgloss.Color("#2b3442")
	cKeyBG  = lipgloss.Color("#20262e")

	sDim  = lipgloss.NewStyle().Foreground(cDim)
	sBold = lipgloss.NewStyle().Bold(true)
	sLink = lipgloss.NewStyle().Foreground(cLink)
	sRun  = lipgloss.NewStyle().Foreground(cRun)
	sYou  = lipgloss.NewStyle().Foreground(cYou)
	sAsk  = lipgloss.NewStyle().Foreground(cAsk).Bold(true)
)

func stageColor(s task.Stage) lipgloss.Color {
	switch s {
	case task.Tool:
		return cTool
	case task.Review:
		return cReview
	case task.Paused:
		return cPaused
	case task.Parked:
		return cParked
	case task.Done:
		return cDone
	}
	return cActive
}

func groupInfo(g task.Group) (title, hint string, color lipgloss.Color) {
	switch g {
	case task.Asking:
		return "AGENT ASKING", "stopped mid-work, waiting for your answer", cAsk
	case task.YourTurn:
		return "YOUR TURN", "agent finished, or nothing running", cYou
	case task.Running:
		return "AGENT RUNNING", "nothing to do yet", cRun
	case task.GReview:
		return "IN REVIEW", "your part is done; waiting on approval, merge, or testing", cReview
	case task.GPaused:
		return "PAUSED", "on hold until something happens", cPaused
	case task.GParked:
		return "PARKED", "", cParked
	case task.GTool:
		return "TOOLS", "not tasks: tools and running apps", cTool
	}
	return "DONE", "close when ready", cDone
}

// agentLabel describes a tab's rolled-up agent status.
func agentLabel(status string) (string, lipgloss.Color) {
	switch status {
	case "working":
		return "working", cRun
	case "blocked":
		return "asking", cAsk
	case "done":
		return "answered", cYou
	case "idle":
		return "idle", cDim
	}
	return "—", cDim
}

// fit truncates or pads s to exactly w cells.
func fit(s string, w int) string {
	if w <= 0 {
		return ""
	}
	if ansi.StringWidth(s) > w {
		s = ansi.Truncate(s, w, "…")
	}
	if pad := w - ansi.StringWidth(s); pad > 0 {
		s += strings.Repeat(" ", pad)
	}
	return s
}

// hyperlink wraps text in an OSC 8 link to raw, clickable in terminals that
// support it. When raw isn't task.SafeURL, isn't linkable, or holds ";" in
// its scheme or host part, it returns the cleaned text alone. The text is
// always cleaned.
func hyperlink(raw, text string) string {
	text = task.CleanLabel(text)
	if !task.SafeURL(raw) || !linkable(raw) || strings.Contains(urlHead(raw), ";") {
		return text
	}
	return "\x1b]8;;" + raw + "\x1b\\" + text + "\x1b]8;;\x1b\\"
}

// linkable reports whether a terminal may be handed raw as an OSC 8 target:
// http(s) with a host, or a local file:// URL. A terminal hands any other
// scheme, such as shortcuts: or vscode:, to the app registered for it, and a
// label can hide which scheme a link has. The board links http(s) only.
func linkable(raw string) bool {
	if _, ok := task.FilePath(raw); ok {
		return true
	}
	u, err := url.Parse(raw) // Parse lowercases the scheme
	return err == nil && (u.Scheme == "http" || u.Scheme == "https") && u.Host != ""
}

// urlHead is a URL up to its path: the scheme, user, host and port.
func urlHead(raw string) string {
	rest := raw
	if _, after, ok := strings.Cut(raw, "://"); ok {
		rest = after
	}
	if i := strings.IndexAny(rest, "/?#"); i >= 0 {
		return raw[:len(raw)-len(rest)+i]
	}
	return raw
}

// wrap breaks text into lines of at most w cells. The text is cleaned with
// task.CleanText first, as notes, log entries and reasons can come from an
// agent or a hand-edited state file.
func wrap(text string, w int) []string {
	if w <= 0 {
		return nil
	}
	text = task.CleanText(text)
	var out []string
	for _, para := range strings.Split(text, "\n") {
		if para == "" {
			out = append(out, "")
			continue
		}
		out = append(out, strings.Split(ansi.Wrap(para, w, ""), "\n")...)
	}
	return out
}

// groupWord is the short name of a group, for the detail panel and the strip.
func groupWord(g task.Group) string {
	switch g {
	case task.Asking:
		return "agent asking"
	case task.YourTurn:
		return "your turn"
	case task.Running:
		return "agent running"
	case task.GReview:
		return "in review"
	case task.GPaused:
		return "paused"
	case task.GParked:
		return "parked"
	case task.GTool:
		return "tool"
	}
	return "done"
}

// reasonLabel is what a stage's reason means, or "" if it takes none.
func reasonLabel(s task.Stage) string {
	switch s {
	case task.Paused:
		return "until"
	case task.Review:
		return "waiting for"
	}
	return ""
}

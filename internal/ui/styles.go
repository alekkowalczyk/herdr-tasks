package ui

import (
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
	cLink   = lipgloss.Color("#9cc4ff")
	cSelBG  = lipgloss.Color("#232b35")
	cChipBG = lipgloss.Color("#2b3442")
	cKeyBG  = lipgloss.Color("#20262e")

	sDim  = lipgloss.NewStyle().Foreground(cDim)
	sBold = lipgloss.NewStyle().Bold(true)
	sLink = lipgloss.NewStyle().Foreground(cLink)
	sRun  = lipgloss.NewStyle().Foreground(cRun)
	sYou  = lipgloss.NewStyle().Foreground(cYou)
)

func stageColor(s task.Stage) lipgloss.Color {
	switch s {
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
	case task.YourTurn:
		return "YOUR TURN", "agent answered, needs input, or nothing running", cYou
	case task.Running:
		return "AGENT RUNNING", "nothing to do yet", cRun
	case task.GPaused:
		return "PAUSED", "on hold until something happens", cPaused
	case task.GParked:
		return "PARKED", "", cParked
	}
	return "DONE", "close when ready", cDone
}

// agentLabel describes a tab's rolled-up agent status.
func agentLabel(status string) (string, lipgloss.Color) {
	switch status {
	case "working":
		return "working", cRun
	case "blocked":
		return "needs input", cYou
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

// hyperlink makes text a clickable OSC 8 link in terminals that support it.
func hyperlink(url, text string) string {
	return "\x1b]8;;" + url + "\x1b\\" + text + "\x1b]8;;\x1b\\"
}

// wrap breaks text into lines of at most w cells.
func wrap(text string, w int) []string {
	if w <= 0 {
		return nil
	}
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

package ui

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"

	"github.com/alekkowalczyk/herdr-tasks/internal/task"
)

// sectionW is the longest section name, so items line up after it.
const sectionW = 6

// sectionName is a section's name in its color, padded to sectionW.
func sectionName(section string) string {
	c := cLink
	switch section {
	case "Linear":
		c = cLinear
	case "Slack":
		c = cSlack
	case "Pages":
		c = cPages
	}
	return lipgloss.NewStyle().Foreground(c).Bold(true).Render(section) + strings.Repeat(" ", max(0, sectionW-len(section)))
}

// linkRows renders links for the strip's right column: one row per section that
// has links, in task.Sections order. Each row is the section's name in its color,
// then its items joined by " · ", each a hyperlink. A row ends in "+N" when the
// rest don't fit. No row is wider than w cells (ansi.StringWidth). With numbered,
// each item is preceded by its 1-based index in links.
func linkRows(links []task.Link, w int, numbered bool) []string {
	var rows []string
	for _, section := range task.Sections {
		var items []int
		for i, l := range links {
			if task.KindOf(l.URL).Section() == section {
				items = append(items, i)
			}
		}
		if len(items) > 0 {
			rows = append(rows, linkRow(section, links, items, w, numbered))
		}
	}
	return rows
}

// linkRow is one row of linkRows. items are the section's indexes in links.
func linkRow(section string, links []task.Link, items []int, w int, numbered bool) string {
	var b strings.Builder
	b.WriteString(sectionName(section) + " ")
	used, shown := sectionW+1, 0
	for k, i := range items {
		lead := ""
		if k > 0 {
			lead = " · "
		}
		if numbered {
			lead += strconv.Itoa(i+1) + " "
		}
		// Leave room for "+N" while items are left after this one.
		more := 0
		if rest := len(items) - k - 1; rest > 0 {
			more = len(" +" + strconv.Itoa(rest))
		}
		text := linkItem(links[i])
		room := w - used - ansi.StringWidth(lead) - more
		if ansi.StringWidth(text) > room {
			// Only the first item is cut to fit; the rest count in "+N".
			if k > 0 || room < 1 {
				break
			}
			text = ansi.Truncate(text, room, "…")
		}
		b.WriteString(sDim.Render(lead) + hyperlink(links[i].URL, text))
		used += ansi.StringWidth(lead + text)
		shown++
	}
	if n := len(items) - shown; n > 0 {
		b.WriteString(sDim.Render(" +" + strconv.Itoa(n)))
	}
	row := b.String()
	if ansi.StringWidth(row) > w {
		// Only when w is narrower than a section name and one item.
		row = ansi.Truncate(row, w, "")
	}
	return row
}

// linkItem is an item's text, always cleaned:
//
//	Linear: its ID;
//	Slack: "Jan 2 15:04" from SlackTime (local time), or "channel";
//	Pages and Links: l.Display().
func linkItem(l task.Link) string {
	text := l.Display()
	switch task.KindOf(l.URL).Section() {
	case "Linear":
		text, _ = task.LinearID(l.URL)
	case "Slack":
		text = "channel"
		if at, ok := task.SlackTime(l.URL); ok {
			text = at.Format("Jan 2 15:04")
		}
	default:
		if l.Label == "" && !task.SafeURL(l.URL) {
			// LinkLabel shows a URL it can't parse as it is, and a terminal
			// could still find a link in that text.
			text = "unsafe link"
		}
	}
	return task.CleanLabel(text)
}

// unlinkMinW is the narrowest column unlinkRows uses, gap included. It fits
// the "+N more" cell.
const unlinkMinW = 32

// unlinkRows lists every link as "N Section label", for picking one to remove,
// in at most rows rows of w cells. The links go in columns left to right when
// one column isn't enough. When even that isn't enough, the last cell says how
// many more there are and how to remove them.
func unlinkRows(links []task.Link, w, rows int) []string {
	inner := w - 1 // a space on the left
	if rows <= 0 || inner <= 0 || len(links) == 0 {
		return nil
	}
	digits := len(strconv.Itoa(len(links)))
	cells := make([]string, len(links))
	for i, l := range links {
		section := task.KindOf(l.URL).Section()
		cells[i] = sDim.Render(fmt.Sprintf("%*d ", digits, i+1)) + sectionName(section) + " " + linkItem(l)
	}
	cols := (len(cells) + rows - 1) / rows
	if most := max(1, inner/unlinkMinW); cols > most {
		cols = most
		keep := cols*rows - 1
		cells = append(cells[:keep], sDim.Render(fmt.Sprintf("+%d more: herdr-tasks link rm", len(links)-keep)))
	}
	perCol := (len(cells) + cols - 1) / cols
	widest := 0
	for _, c := range cells {
		widest = max(widest, ansi.StringWidth(c))
	}
	colW := min(inner/cols, widest+2)
	out := make([]string, perCol)
	for r := range out {
		line := " "
		for c := 0; c < cols; c++ {
			if i := c*perCol + r; i < len(cells) {
				line += fit(cells[i], colW-2) + "  "
			}
		}
		out[r] = fit(line, w)
	}
	return out
}

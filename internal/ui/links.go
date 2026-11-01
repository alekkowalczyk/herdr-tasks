package ui

import (
	"fmt"
	"strconv"
	"strings"
	"time"

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

// linkColumn is the strip's links column, as linkRows lays it out.
type linkColumn struct {
	rows []string
	// The cells each row's closing "+N" takes, or 0 for a row without one.
	plus []int
	// How many links no row shows.
	hidden int
}

// linkRows lays out links for the strip's links column, in at most rows rows
// of w cells. Each section that has links, in task.Sections order, starts a
// row with its name in its color, then its items joined by " · ", each a
// hyperlink. A section whose items don't fit goes on to the next row,
// indented under its first item, while it leaves one row for each section
// below it. Its last row ends in "+N" when the rest still don't fit. No row
// is wider than w cells (ansi.StringWidth). With numbered, each item is
// preceded by its number in task.Grouped order.
func linkRows(links []task.Link, w, rows int, numbered bool) linkColumn {
	grouped := task.Grouped(links)
	type section struct {
		name  string
		items []int // indexes in grouped
	}
	var sections []section
	for _, name := range task.Sections {
		s := section{name: name}
		for i, l := range grouped {
			if task.KindOf(l.URL).Section() == name {
				s.items = append(s.items, i)
			}
		}
		if len(s.items) > 0 {
			sections = append(sections, s)
		}
	}
	var col linkColumn
	for k, s := range sections {
		left := rows - len(col.rows)
		if left <= 0 {
			col.hidden += len(s.items)
			continue
		}
		out, plus, shown := linkSection(s.name, grouped, s.items, w, max(1, left-(len(sections)-k-1)), numbered)
		col.rows = append(col.rows, out...)
		col.plus = append(col.plus, plus...)
		col.hidden += len(s.items) - shown
	}
	return col
}

// linkSection is one section of linkRows, in at most rows rows: the rows,
// the cells each one's "+N" takes, and how many of items they show. items
// are the section's indexes in links.
func linkSection(section string, links []task.Link, items []int, w, rows int, numbered bool) (out []string, plus []int, shown int) {
	indent := sectionW + 1
	var b strings.Builder
	b.WriteString(sectionName(section) + " ")
	used, first := indent, true // first: nothing on this row yet
	for shown < len(items) {
		i := items[shown]
		last := len(out) == rows-1
		lead := ""
		if !first {
			lead = " · "
		}
		if numbered {
			lead += strconv.Itoa(i+1) + " "
		}
		// The last row leaves room for "+N" while items are left after this one.
		more := 0
		if rest := len(items) - shown - 1; last && rest > 0 {
			more = len(" +" + strconv.Itoa(rest))
		}
		text := linkItem(links[i])
		room := w - used - ansi.StringWidth(lead) - more
		if ansi.StringWidth(text) > room {
			if !first && !last {
				// On to the next row, under the first item.
				out, plus = append(out, b.String()), append(plus, 0)
				b.Reset()
				b.WriteString(strings.Repeat(" ", indent))
				used, first = indent, true
				continue
			}
			// Only a row's first item is cut to fit; on the last row, the
			// rest count in "+N".
			if !first || room < 1 {
				break
			}
			text = ansi.Truncate(text, room, "…")
		}
		b.WriteString(sDim.Render(lead) + hyperlink(links[i].URL, text))
		used += ansi.StringWidth(lead + text)
		first = false
		shown++
	}
	p := 0
	if n := len(items) - shown; n > 0 {
		b.WriteString(sDim.Render(" +" + strconv.Itoa(n)))
		p = len("+" + strconv.Itoa(n))
	}
	row := b.String()
	if ansi.StringWidth(row) > w {
		// Only when w is narrower than a section name and one item.
		row, p = ansi.Truncate(row, w, ""), 0
	}
	return append(out, row), append(plus, p), shown
}

// linkItem is an item's text, always cleaned:
//
//	Linear: its ID;
//	Slack: a message's "Jan 2 15:04" from SlackStamp (local time), then its
//	label when it has one; a channel's label, or "channel";
//	Pages and Links: l.Display().
func linkItem(l task.Link) string {
	text := l.Display()
	switch task.KindOf(l.URL).Section() {
	case "Linear":
		text, _ = task.LinearID(l.URL)
	case "Slack":
		text = l.Label
		if at := task.SlackStamp(l.URL, time.Local); at != "" {
			text = strings.TrimSpace(at + " " + l.Label)
		} else if text == "" {
			text = "channel"
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

// numberedMinW is the narrowest column numberedLinks uses, gap included. It
// fits the "+N more" cell.
const numberedMinW = 32

// numberedLinks lists links as "N Section label", numbered in the order
// given, which is task.Grouped's, each label a hyperlink. It's how the strip
// shows every link, and lets you pick one to remove. It uses at most rows
// rows of w cells, with the links in columns left to right when one column
// isn't enough. When even that isn't enough, the last cell says how many
// more there are, and where to find them: "+N more: " and rest.
func numberedLinks(links []task.Link, w, rows int, rest string) []string {
	inner := w - 1 // a space on the left
	if rows <= 0 || inner <= 0 || len(links) == 0 {
		return nil
	}
	cells := numberedCells(links)
	cols := (len(cells) + rows - 1) / rows
	if most := max(1, inner/numberedMinW); cols > most {
		cols = most
		keep := cols*rows - 1
		cells = append(cells[:keep], sDim.Render(fmt.Sprintf("+%d more: %s", len(links)-keep, rest)))
	}
	perCol := (len(cells) + cols - 1) / cols
	colW := cellWidth(cells, inner, cols)
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

// numberedCells are links as numberedLinks shows them, one cell each.
func numberedCells(links []task.Link) []string {
	digits := len(strconv.Itoa(len(links)))
	cells := make([]string, len(links))
	for i, l := range links {
		section := task.KindOf(l.URL).Section()
		cells[i] = sDim.Render(fmt.Sprintf("%*d ", digits, i+1)) + sectionName(section) + " " + hyperlink(l.URL, linkItem(l))
	}
	return cells
}

// cellWidth is the width of each of cols columns of cells in inner cells,
// gap included: the widest cell's, but no more than their share.
func cellWidth(cells []string, inner, cols int) int {
	widest := 0
	for _, c := range cells {
		widest = max(widest, ansi.StringWidth(c))
	}
	return min(inner/cols, widest+2)
}

// linkPage is what pagedLinks shows: its rows, the row of cells it starts
// at and the last it can, and the links it shows, from first to last of n,
// counted from 1. scrolls is whether there are more than fit.
type linkPage struct {
	lines          []string
	top, maxTop    int
	first, last, n int
	scrolls        bool
}

// pagedLinks lists links as numberedLinks does when they fit in rows rows
// of w cells. When they don't, it lays them out a row of cells at a time,
// left to right, and shows rows of them from the row of cells top, kept in
// range, so a scroll by a row reaches every link.
func pagedLinks(links []task.Link, w, rows, top int) linkPage {
	inner := w - 1 // a space on the left
	if rows <= 0 || inner <= 0 || len(links) == 0 {
		return linkPage{n: len(links)}
	}
	cols := max(1, inner/numberedMinW)
	if len(links) <= cols*rows {
		return linkPage{lines: numberedLinks(links, w, rows, ""), first: 1, last: len(links), n: len(links)}
	}
	cells := numberedCells(links)
	total := (len(cells) + cols - 1) / cols
	p := linkPage{maxTop: total - rows, n: len(links), scrolls: true}
	p.top = min(max(0, top), p.maxTop)
	colW := cellWidth(cells, inner, cols)
	for r := p.top; r < p.top+rows; r++ {
		line := " "
		for c := 0; c < cols; c++ {
			if i := r*cols + c; i < len(cells) {
				line += fit(cells[i], colW-2) + "  "
			}
		}
		p.lines = append(p.lines, fit(line, w))
	}
	p.first, p.last = p.top*cols+1, min(len(cells), (p.top+rows)*cols)
	return p
}

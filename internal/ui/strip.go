package ui

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"

	"github.com/alekkowalczyk/herdr-tasks/internal/herdr"
	"github.com/alekkowalczyk/herdr-tasks/internal/store"
	"github.com/alekkowalczyk/herdr-tasks/internal/strip"
	"github.com/alekkowalczyk/herdr-tasks/internal/task"
)

// RunStrip shows the task of the tab this pane is in. herdr-tasks starts it in
// a short pane along the bottom of each tab.
//
// When herdr-tasks is updated, the strip restarts itself with the new
// version in the same pane.
func RunStrip(c *herdr.Client, st *store.Store) error {
	bin := installedBinary()
	m := &stripModel{c: c, st: st, pane: os.Getenv("HERDR_PANE_ID"), bin: bin, binID: fileID(bin)}
	if _, err := tea.NewProgram(m, tea.WithAltScreen()).Run(); err != nil {
		return err
	}
	if m.restart {
		return syscall.Exec(bin, []string{bin, "strip"}, os.Environ())
	}
	return nil
}

// installedBinary is the herdr-tasks that setup installs, or this one.
func installedBinary() string {
	home, _ := os.UserHomeDir()
	bin := filepath.Join(home, ".local", "bin", "herdr-tasks")
	if _, err := os.Stat(bin); err == nil {
		return bin
	}
	exe, _ := os.Executable()
	return exe
}

// fileID identifies the file at path, changing when it's replaced.
func fileID(path string) uint64 {
	var st syscall.Stat_t
	if syscall.Stat(path, &st) != nil {
		return 0
	}
	return st.Ino
}

type stripModel struct {
	c    *herdr.Client
	st   *store.Store
	pane string

	bin     string
	binID   uint64
	restart bool

	tab herdr.Tab
	t   *task.Task
	err string
	msg string

	width, height int
}

type stripData struct {
	tab herdr.Tab
	t   *task.Task
	err string
}

type stripTick struct{}

func (m *stripModel) load() tea.Msg {
	tabID, err := m.c.PaneTab(m.pane)
	if err != nil {
		return stripData{err: "Can't reach herdr: " + err.Error()}
	}
	tab, err := m.c.TabGet(tabID)
	if err != nil {
		return stripData{err: err.Error()}
	}
	s, err := m.st.Load()
	if err != nil {
		return stripData{err: err.Error()}
	}
	return stripData{tab: tab, t: s.Tasks[tabID]}
}

func (m *stripModel) Init() tea.Cmd { return m.load }

func (m *stripModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width, m.height = msg.Width, msg.Height
		// Keep the strip its intended height when the window changes size.
		// Fitting again changes nothing once it's right, so this settles.
		if m.height != strip.Rows {
			return m, func() tea.Msg {
				_ = strip.Host{Client: m.c}.Fit(m.pane)
				return nil
			}
		}
	case stripData:
		m.tab, m.t, m.err = msg.tab, msg.t, msg.err
		return m, tea.Tick(refreshEvery, func(time.Time) tea.Msg { return stripTick{} })
	case stripTick:
		if id := fileID(m.bin); id != 0 && id != m.binID {
			m.restart = true
			return m, tea.Quit
		}
		return m, m.load
	case tea.KeyMsg:
		switch msg.String() {
		case "enter", "t":
			if err := m.c.OpenPluginPane(store.PluginID, "overview"); err != nil {
				m.msg = "Couldn't open the overview: " + err.Error()
			}
		default:
			m.msg = "↵ opens the task overview · ctrl+b i hides this strip"
		}
	}
	return m, nil
}

func (m *stripModel) View() string {
	w, h := m.width, m.height
	if w <= 0 || h <= 0 {
		return ""
	}
	var lines []string
	if m.err != "" {
		lines = append(lines, sDim.Render(fit(" "+m.err, w)))
		return strings.Join(lines, "\n")
	}
	if m.t == nil {
		return sDim.Render(fit(" Not a task yet. It becomes one in a moment.", w))
	}
	t := m.t
	g := task.GroupOf(t.Stage, m.tab.AgentStatus)
	_, _, gc := groupInfo(g)

	// First line: stage, what's going on, and what it's waiting for.
	head := " " + lipgloss.NewStyle().Foreground(stageColor(t.Stage)).Render(t.Stage.Glyph()+" "+t.Stage.Label())
	if t.Stage == task.Active {
		head += sDim.Render(" · ") + lipgloss.NewStyle().Foreground(gc).Bold(g == task.Asking).Render(groupWord(g))
	}
	head += sDim.Render(" · " + task.Age(time.Since(t.Since)))
	if t.Stage.TakesReason() && t.Reason != "" {
		head += sDim.Render(" · ") + lipgloss.NewStyle().Foreground(stageColor(t.Stage)).Render(reasonLabel(t.Stage)+" "+t.Reason)
	}
	hint := "↵ tasks · ctrl+b i hide "
	if m.msg != "" {
		hint = m.msg + " "
	}
	if gap := w - ansi.StringWidth(head) - ansi.StringWidth(hint); gap >= 2 {
		head += strings.Repeat(" ", gap) + sDim.Render(hint)
	}
	lines = append(lines, fit(head, w))

	// Below: note and agent lines on the left, links on the right.
	linkW := 0
	if len(t.Links) > 0 {
		linkW = min(52, w/2)
	}
	noteW := w - linkW - 3
	var left []string
	if t.Note != "" {
		left = append(left, wrap(t.Note, noteW)...)
	}
	for _, e := range t.Log {
		who := e.Who + " " + e.At.Local().Format("15:04")
		for i, l := range wrap(e.Text, max(10, noteW-ansi.StringWidth(who)-1)) {
			prefix := strings.Repeat(" ", ansi.StringWidth(who))
			if i == 0 {
				prefix = sRun.Render(who)
			}
			left = append(left, prefix+" "+l)
		}
	}
	if len(left) == 0 {
		left = append(left, sDim.Render("No note yet. ↵ opens the overview, where n edits it."))
	}
	// Show the newest lines when the note doesn't fit.
	if room := h - 1; len(left) > room && room > 0 {
		left = left[len(left)-room:]
	}
	var right []string
	for i, l := range t.Links {
		via := ""
		if l.By != "" {
			via = sRun.Render(" via " + l.By)
		}
		label := l.Display()
		url := strings.TrimPrefix(strings.TrimPrefix(l.URL, "https://"), "http://")
		rest := max(0, linkW-ansi.StringWidth(label)-ansi.StringWidth(via)-5)
		right = append(right, sDim.Render(strconv.Itoa(i+1)+" ")+hyperlink(l.URL, sLink.Render(label))+" "+sDim.Render(fit(url, rest))+via)
	}
	for i := 0; i < h-1; i++ {
		l, r := "", ""
		if i < len(left) {
			l = left[i]
		}
		if i < len(right) {
			r = right[i]
		}
		line := " " + fit(l, noteW)
		if linkW > 0 {
			line += sDim.Render(" │") + fit(r, linkW)
		}
		lines = append(lines, line)
	}
	return strings.Join(lines, "\n")
}

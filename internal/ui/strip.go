package ui

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/charmbracelet/bubbles/textarea"
	"github.com/charmbracelet/bubbles/textinput"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"

	"github.com/alekkowalczyk/herdr-tasks/internal/herdr"
	"github.com/alekkowalczyk/herdr-tasks/internal/reconcile"
	"github.com/alekkowalczyk/herdr-tasks/internal/store"
	"github.com/alekkowalczyk/herdr-tasks/internal/strip"
	"github.com/alekkowalczyk/herdr-tasks/internal/task"
)

// noteRows is how tall the strip grows while you edit the note.
const noteRows = 14

// RunStrip shows the task of the tab this pane is in, with buttons to change
// it. herdr-tasks starts it in a short pane along the bottom of each tab.
//
// When herdr-tasks is updated, the strip restarts itself with the new
// version in the same pane.
func RunStrip(c *herdr.Client, st *store.Store, opts reconcile.Options) error {
	bin := installedBinary()
	pane := os.Getenv("HERDR_PANE_ID")
	opts.Self = pane // saving a change never closes this strip; it exits itself
	m := &stripModel{c: c, st: st, opts: opts, pane: pane, bin: bin, binID: fileID(bin), rows: strip.Rows}
	m.input = textinput.New()
	m.input.Prompt = ""
	m.note = textarea.New()
	m.note.ShowLineNumbers = false
	m.note.Prompt = ""
	m.note.Placeholder = "What matters about this task?"
	m.note.CharLimit = 4000
	if _, err := tea.NewProgram(m, tea.WithAltScreen(), tea.WithMouseCellMotion()).Run(); err != nil {
		return err
	}
	if m.restart && !m.closing {
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
	opts reconcile.Options
	pane string

	bin     string
	binID   uint64
	restart bool
	closing bool // the tab no longer wants a strip; exiting closes the pane

	tab herdr.Tab
	pos int
	t   *task.Task
	err string
	msg string

	width, height int
	rows          int // rows the strip asks herdr for

	mode        mode
	input       textinput.Model
	inputKind   inputKind
	reasonStage task.Stage
	note        textarea.Model
	buttons     []hit
}

type stripData struct {
	tab herdr.Tab
	pos int
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
	pos := 0
	if live, err := m.c.Live(); err == nil {
		pos = live.Position(tabID)
	}
	return stripData{tab: tab, pos: pos, t: s.Tasks[tabID]}
}

func (m *stripModel) Init() tea.Cmd { return m.load }

func (m *stripModel) fit() tea.Cmd {
	rows := m.rows
	return func() tea.Msg {
		_ = strip.Host{Client: m.c}.FitRows(m.pane, rows)
		return nil
	}
}

// backToWork moves focus to the pane above the strip.
func (m *stripModel) backToWork() {
	_ = m.c.Call("pane.focus_direction", map[string]any{"pane_id": m.pane, "direction": "up"}, nil)
}

// change edits this tab's task and syncs herdr, then goes back to work.
func (m *stripModel) change(fn func(t *task.Task), done string) tea.Cmd {
	id := m.tab.ID
	_, err := reconcile.Run(strip.Host{Client: m.c}, m.st, m.opts, time.Now(), func(s *store.State, _ *herdr.Live) error {
		t, ok := s.Tasks[id]
		if !ok {
			return fmt.Errorf("this tab isn't a task yet")
		}
		fn(t)
		return nil
	})
	if err != nil {
		m.msg = "Couldn't save: " + err.Error()
		return nil
	}
	m.msg = done
	m.backToWork()
	// A change can mean this tab no longer wants a strip, as when it becomes
	// a tool. Saving left this strip open, so it closes itself now.
	if s, err := m.st.Load(); err == nil {
		if t, ok := s.Tasks[id]; ok && (!m.opts.Strip || !t.WantsStrip()) {
			// Forget this strip first, so its going away isn't read as you hiding it.
			_ = m.st.Update(func(s *store.State) error {
				if t, ok := s.Tasks[id]; ok && t.StripPane == m.pane {
					t.StripPane = ""
				}
				return nil
			})
			m.closing = true
			return tea.Quit
		}
	}
	return m.load
}

func (m *stripModel) setStage(s task.Stage) tea.Cmd {
	return m.change(func(t *task.Task) { t.SetStage(s, "", time.Now()) }, s.Label())
}

func (m *stripModel) startInput(kind inputKind, value, placeholder string) {
	m.mode, m.inputKind = modeInput, kind
	m.input.SetValue(value)
	m.input.Placeholder = placeholder
	m.input.Width = max(10, m.width-30)
	m.input.CursorEnd()
	m.input.Focus()
}

func (m *stripModel) finishInput(save bool) tea.Cmd {
	m.mode = modeList
	m.input.Blur()
	v := strings.TrimSpace(m.input.Value())
	switch m.inputKind {
	case inputReason:
		stage := m.reasonStage
		if !save {
			v = ""
		}
		return m.change(func(t *task.Task) {
			t.SetStage(stage, "", time.Now())
			if v != "" {
				t.Reason = v
			}
		}, stage.Label())
	case inputLink:
		if !save || v == "" {
			return nil
		}
		if !strings.Contains(v, "://") {
			v = "https://" + v
		}
		return m.change(func(t *task.Task) { t.AddLink(v, "", "", time.Now()) }, "Added "+task.LinkLabel(v))
	case inputRename:
		if !save {
			return nil
		}
		return m.change(func(t *task.Task) { t.Title = v }, "Renamed")
	}
	return nil
}

func (m *stripModel) startNote() tea.Cmd {
	if m.t == nil {
		return nil
	}
	m.mode = modeNote
	m.note.SetValue(m.t.Note)
	m.note.Focus()
	m.rows = noteRows
	m.sizeNote()
	return m.fit()
}

func (m *stripModel) finishNote(save bool) tea.Cmd {
	m.mode = modeList
	m.note.Blur()
	m.rows = strip.Rows
	fit := m.fit()
	if !save {
		m.backToWork()
		return fit
	}
	v := strings.TrimSpace(m.note.Value())
	return tea.Batch(fit, m.change(func(t *task.Task) { t.Note = v }, "Note saved"))
}

func (m *stripModel) sizeNote() {
	m.note.SetWidth(max(10, m.width-4))
	m.note.SetHeight(max(1, m.height-2))
}

// action runs a button or key.
func (m *stripModel) action(k string) tea.Cmd {
	m.msg = ""
	if m.t == nil {
		return nil
	}
	switch k {
	case "a":
		return m.setStage(task.Active)
	case "v":
		m.reasonStage = task.Review
		m.startInput(inputReason, m.t.Reason, "approval, merge, testing…")
	case "p":
		m.reasonStage = task.Paused
		m.startInput(inputReason, m.t.Reason, "PR review, QA, your own decision…")
	case "z":
		return m.setStage(task.Parked)
	case "d":
		return m.setStage(task.Done)
	case "o":
		return m.setStage(task.Tool)
	case "n":
		return m.startNote()
	case "l":
		m.startInput(inputLink, "", "https://github.com/… or linear.app/…")
	case "L":
		if len(m.t.Links) > 0 {
			m.mode = modeUnlink
		}
	case "r":
		m.startInput(inputRename, m.t.Title, "task name (empty uses the tab number)")
	case "x":
		if m.t.Stage != task.Done {
			m.msg = "Mark it done first (d), then close the tab."
			return nil
		}
		if err := m.c.CloseTab(m.tab.ID); err != nil {
			m.msg = "Couldn't close the tab: " + err.Error()
		}
	case "enter", "t":
		if err := m.c.OpenPluginPane(store.PluginID, "overview"); err != nil {
			m.msg = "Couldn't open the overview: " + err.Error()
		}
	case "esc":
		m.backToWork()
	}
	return nil
}

func (m *stripModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width, m.height = msg.Width, msg.Height
		m.sizeNote()
		// Keep the strip its intended height when the window changes size.
		// Fitting again changes nothing once it's right, so this settles.
		if m.height != m.rows {
			return m, m.fit()
		}
	case stripData:
		m.tab, m.pos, m.t, m.err = msg.tab, msg.pos, msg.t, msg.err
		return m, tea.Tick(refreshEvery, func(time.Time) tea.Msg { return stripTick{} })
	case stripTick:
		if id := fileID(m.bin); id != 0 && id != m.binID && m.mode == modeList {
			m.restart = true
			return m, tea.Quit
		}
		return m, m.load
	case tea.MouseMsg:
		if m.mode != modeList || msg.Action != tea.MouseActionPress || msg.Button != tea.MouseButtonLeft || msg.Y != m.height-1 {
			return m, nil
		}
		for _, b := range m.buttons {
			if msg.X >= b.x0 && msg.X < b.x1 {
				return m, m.action(b.target)
			}
		}
	case tea.KeyMsg:
		switch m.mode {
		case modeNote:
			switch msg.String() {
			case "ctrl+s":
				return m, m.finishNote(true)
			case "esc":
				return m, m.finishNote(false)
			}
			var cmd tea.Cmd
			m.note, cmd = m.note.Update(msg)
			return m, cmd
		case modeInput:
			switch msg.String() {
			case "enter":
				return m, m.finishInput(true)
			case "esc":
				return m, m.finishInput(false)
			}
			var cmd tea.Cmd
			m.input, cmd = m.input.Update(msg)
			return m, cmd
		case modeUnlink:
			m.mode = modeList
			if n, err := strconv.Atoi(msg.String()); err == nil && m.t != nil && n >= 1 && n <= len(m.t.Links) {
				label := m.t.Links[n-1].Display()
				return m, m.change(func(t *task.Task) { _, _ = t.RemoveLink(strconv.Itoa(n)) }, "Removed "+label)
			}
			return m, nil
		}
		return m, m.action(msg.String())
	}
	return m, nil
}

func (m *stripModel) View() string {
	w, h := m.width, m.height
	if w <= 0 || h <= 0 {
		return ""
	}
	if m.err != "" {
		return sDim.Render(fit(" "+m.err, w))
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
	right := sBold.Render(t.Name(m.pos)) + " "
	if m.msg != "" {
		right = m.msg + " "
	}
	if gap := w - ansi.StringWidth(head) - ansi.StringWidth(right); gap >= 2 {
		head += strings.Repeat(" ", gap) + right
	}
	lines := []string{fit(head, w)}
	body := h - 2 // between the first line and the buttons

	if m.mode == modeNote {
		for _, l := range strings.Split(m.note.View(), "\n") {
			if len(lines) > body {
				break
			}
			lines = append(lines, " "+l)
		}
		for len(lines) <= body {
			lines = append(lines, "")
		}
		lines = append(lines, fit(sDim.Render(" Editing the note · ctrl+s save · esc cancel"), w))
		return strings.Join(lines, "\n")
	}

	// Note and agent lines on the left, links on the right.
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
		left = append(left, sDim.Render("No note yet."))
	}
	// Show the newest lines when the note doesn't fit.
	if len(left) > body && body > 0 {
		left = left[len(left)-body:]
	}
	var links []string
	for i, l := range t.Links {
		via := ""
		if l.By != "" {
			via = sRun.Render(" via " + l.By)
		}
		label := l.Display()
		url := strings.TrimPrefix(strings.TrimPrefix(l.URL, "https://"), "http://")
		rest := max(0, linkW-ansi.StringWidth(label)-ansi.StringWidth(via)-5)
		links = append(links, sDim.Render(strconv.Itoa(i+1)+" ")+hyperlink(l.URL, sLink.Render(label))+" "+sDim.Render(fit(url, rest))+via)
	}
	for i := 0; i < body; i++ {
		l, r := "", ""
		if i < len(left) {
			l = left[i]
		}
		if i < len(links) {
			r = links[i]
		}
		line := " " + fit(l, noteW)
		if linkW > 0 {
			line += sDim.Render(" │") + fit(r, linkW)
		}
		lines = append(lines, line)
	}
	lines = append(lines, m.bottom(w))
	return strings.Join(lines, "\n")
}

// bottom is the button row, or the line you type into.
func (m *stripModel) bottom(w int) string {
	m.buttons = m.buttons[:0]
	switch m.mode {
	case modeInput:
		label := map[inputKind]string{inputReason: m.reasonStage.Label() + ", " + reasonLabel(m.reasonStage), inputLink: "Add link", inputRename: "Name"}[m.inputKind]
		color := map[inputKind]lipgloss.Color{inputReason: stageColor(m.reasonStage), inputLink: cLink, inputRename: cFg}[m.inputKind]
		return fit(" "+lipgloss.NewStyle().Foreground(color).Render(label)+" "+m.input.View()+sDim.Render("  ↵ save · esc cancel"), w)
	case modeUnlink:
		return fit(sDim.Render(fmt.Sprintf(" Press a link number to remove it (1–%d), any other key to cancel", len(m.t.Links))), w)
	}
	type button struct{ key, label string }
	buttons := []button{{"a", "active"}, {"v", "review"}, {"p", "pause"}, {"z", "park"}, {"d", "done"}, {"o", "tool"},
		{"n", "note"}, {"l", "link"}, {"r", "rename"}}
	if len(m.t.Links) > 0 {
		buttons = append(buttons, button{"L", "unlink"})
	}
	if m.t.Stage == task.Done {
		buttons = append(buttons, button{"x", "close tab"})
	}
	buttons = append(buttons, button{"enter", "tasks"}, button{"esc", "back"})
	current := map[task.Stage]string{task.Active: "a", task.Review: "v", task.Paused: "p", task.Parked: "z", task.Done: "d", task.Tool: "o"}[m.t.Stage]

	var b strings.Builder
	x := 1
	b.WriteString(" ")
	for _, bt := range buttons {
		key := bt.key
		if key == "enter" {
			key = "↵"
		}
		st := lipgloss.NewStyle().Background(cKeyBG)
		if bt.key == current {
			// The current stage's button shows in its color.
			st = st.Foreground(stageColor(m.t.Stage))
		}
		chip := st.Render(sBold.Render(key) + st.Render(" "+bt.label))
		cw := ansi.StringWidth(chip)
		if x+cw+1 > w {
			break
		}
		m.buttons = append(m.buttons, hit{x, x + cw, bt.key})
		b.WriteString(chip + " ")
		x += cw + 1
	}
	return b.String()
}

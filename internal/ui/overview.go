// Package ui is the task overview that opens in a herdr popup.
package ui

import (
	"fmt"
	"sort"
	"strconv"
	"strings"
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

type Env struct {
	Client *herdr.Client
	Store  *store.Store
	Opts   reconcile.Options
	// The tab to select when the overview opens.
	CurrentTab string
}

func (e Env) host() strip.Host { return strip.Host{Client: e.Client} }

func Run(env Env) error {
	m, err := newModel(env)
	if err != nil {
		return err
	}
	_, err = tea.NewProgram(m, tea.WithAltScreen(), tea.WithMouseCellMotion()).Run()
	return err
}

type mode int

const (
	modeList mode = iota
	modeNote
	modeInput
	modeUnlink
)

type inputKind int

const (
	inputReason inputKind = iota // for Paused or In review, see reasonStage
	inputLink
	inputRename
)

const (
	detailWidth  = 44
	doubleClick  = 450 * time.Millisecond
	refreshEvery = 2 * time.Second
)

type row struct {
	tab   herdr.Tab
	task  *task.Task
	ws    herdr.Workspace
	pos   int
	group task.Group
}

type hit struct {
	x0, x1 int
	target string
}

type model struct {
	env   Env
	live  *herdr.Live
	state *store.State
	rows  []row
	sel   string

	width, height int
	mode          mode
	input         textinput.Model
	inputKind     inputKind
	reasonStage   task.Stage
	note          textarea.Model
	msg           string
	help          bool

	// Hit areas from the last render, for the mouse.
	rowAt      map[int]string
	linkAt     map[int]int
	chips      []hit
	keys       []hit
	filterY    int
	footerY    int
	alertY     int
	listTop    int
	listScroll int

	lastTarget string
	lastAt     time.Time
}

func newModel(env Env) (*model, error) {
	m := &model{env: env, rowAt: map[int]string{}, linkAt: map[int]int{}, alertY: -1}
	// Track any tabs created since the last sync before showing them.
	live, err := reconcile.Run(env.host(), env.Store, env.Opts, time.Now(), nil)
	if err != nil {
		return nil, err
	}
	m.live = live
	if m.state, err = env.Store.Load(); err != nil {
		return nil, err
	}
	m.sel = env.CurrentTab
	m.rebuild()

	m.input = textinput.New()
	m.input.Prompt = ""
	m.note = textarea.New()
	m.note.ShowLineNumbers = false
	m.note.Prompt = ""
	m.note.Placeholder = "What matters about this task?"
	m.note.CharLimit = 4000
	return m, nil
}

type refreshMsg struct{}

func tick() tea.Cmd {
	return tea.Tick(refreshEvery, func(time.Time) tea.Msg { return refreshMsg{} })
}

func (m *model) Init() tea.Cmd { return tick() }

// refresh rereads herdr and the state file, picking up agent status changes
// and notes agents added.
func (m *model) refresh() {
	if live, err := m.env.Client.Live(); err == nil {
		m.live = live
	}
	if st, err := m.env.Store.Load(); err == nil {
		m.state = st
	}
	m.rebuild()
}

// rebuild recomputes the visible rows and keeps the selection on a visible task.
func (m *model) rebuild() {
	m.rows = m.rows[:0]
	for _, tab := range m.live.Tabs {
		t, ok := m.state.Tasks[tab.ID]
		if !ok {
			continue
		}
		ws, _ := m.live.Workspace(tab.WorkspaceID)
		if m.state.IsHidden(ws.Label) {
			continue
		}
		m.rows = append(m.rows, row{tab: tab, task: t, ws: ws, pos: m.live.Position(tab.ID), group: task.GroupOf(t.Stage, tab.AgentStatus)})
	}
	order := map[task.Group]int{}
	for i, g := range task.Groups {
		order[g] = i
	}
	sort.SliceStable(m.rows, func(i, j int) bool {
		a, b := m.rows[i], m.rows[j]
		if order[a.group] != order[b.group] {
			return order[a.group] < order[b.group]
		}
		if a.task.Priority != b.task.Priority {
			return a.task.Priority
		}
		return a.task.Since.After(b.task.Since)
	})
	if m.selected() == nil && len(m.rows) > 0 {
		m.sel = m.rows[0].tab.ID
	}
}

func (m *model) selected() *row {
	for i := range m.rows {
		if m.rows[i].tab.ID == m.sel {
			return &m.rows[i]
		}
	}
	return nil
}

func (m *model) move(delta int) {
	if len(m.rows) == 0 {
		return
	}
	i := 0
	for k, r := range m.rows {
		if r.tab.ID == m.sel {
			i = k
		}
	}
	i = max(0, min(len(m.rows)-1, i+delta))
	m.sel = m.rows[i].tab.ID
}

// change edits the selected task and syncs herdr (markers, order, sidebar).
func (m *model) change(fn func(t *task.Task), done string) {
	r := m.selected()
	if r == nil {
		return
	}
	id := r.tab.ID
	live, err := reconcile.Run(m.env.host(), m.env.Store, m.env.Opts, time.Now(), func(s *store.State, _ *herdr.Live) error {
		t, ok := s.Tasks[id]
		if !ok {
			return fmt.Errorf("that tab is gone")
		}
		fn(t)
		return nil
	})
	if err != nil {
		m.msg = "Couldn't save: " + err.Error()
		return
	}
	m.live = live
	if st, err := m.env.Store.Load(); err == nil {
		m.state = st
	}
	m.rebuild()
	m.msg = done
}

func (m *model) togglePriority() {
	r := m.selected()
	if r == nil {
		return
	}
	name := r.task.Name(r.pos)
	if !r.task.Stage.TakesPriority() {
		m.msg = fmt.Sprintf("%s is %s, so it can't be a priority", name, strings.ToLower(r.task.Stage.Label()))
		return
	}
	on := !r.task.Priority
	done := name + " is a priority"
	if !on {
		done = name + " is no longer a priority"
	}
	m.change(func(t *task.Task) { _ = t.SetPriority(on) }, done)
}

func (m *model) setStage(s task.Stage) {
	r := m.selected()
	if r == nil {
		return
	}
	name := r.task.Name(r.pos)
	m.change(func(t *task.Task) { t.SetStage(s, "", time.Now()) }, fmt.Sprintf("%s → %s", name, s.Label()))
}

// Space filter. Hidden spaces are stored by label so the filter survives restarts.

func (m *model) saveHidden(hidden []string) {
	err := m.env.Store.Update(func(s *store.State) error {
		s.Hidden = hidden
		return nil
	})
	if err != nil {
		m.msg = "Couldn't save the filter: " + err.Error()
		return
	}
	m.state.Hidden = hidden
	m.rebuild()
}

func (m *model) toggleSpace(n int) {
	if n < 1 || n > len(m.live.Workspaces) {
		return
	}
	label := m.live.Workspaces[n-1].Label
	var hidden []string
	found := false
	for _, h := range m.state.Hidden {
		if h == label {
			found = true
			continue
		}
		hidden = append(hidden, h)
	}
	if !found {
		if len(m.visibleSpaces()) <= 1 {
			m.msg = "At least one space stays visible. Press 0 to show all."
			return
		}
		hidden = append(hidden, label)
	}
	m.saveHidden(hidden)
}

func (m *model) onlySpace(n int) {
	if n < 1 || n > len(m.live.Workspaces) {
		return
	}
	var hidden []string
	for i, w := range m.live.Workspaces {
		if i != n-1 {
			hidden = append(hidden, w.Label)
		}
	}
	m.saveHidden(hidden)
	m.msg = "Showing only " + m.live.Workspaces[n-1].Label + ". Press 0 to show all."
}

func (m *model) showAll() { m.saveHidden(nil) }

func (m *model) visibleSpaces() []herdr.Workspace {
	var out []herdr.Workspace
	for _, w := range m.live.Workspaces {
		if !m.state.IsHidden(w.Label) {
			out = append(out, w)
		}
	}
	return out
}

func spaceIndex(target string) int {
	n, _ := strconv.Atoi(strings.TrimPrefix(target, "space:"))
	return n
}

// Input modes.

func (m *model) startInput(kind inputKind, value, placeholder string) {
	m.mode, m.inputKind = modeInput, kind
	m.input.SetValue(value)
	m.input.Placeholder = placeholder
	m.input.CursorEnd()
	m.input.Focus()
}

func (m *model) finishInput() {
	v := strings.TrimSpace(m.input.Value())
	m.mode = modeList
	m.input.Blur()
	r := m.selected()
	if r == nil {
		return
	}
	name := r.task.Name(r.pos)
	switch m.inputKind {
	case inputReason:
		stage := m.reasonStage
		m.change(func(t *task.Task) {
			t.SetStage(stage, "", time.Now())
			t.Reason = v
		}, fmt.Sprintf("%s → %s%s", name, stage.Label(), map[bool]string{true: ", " + reasonLabel(stage) + " " + v, false: ""}[v != ""]))
	case inputLink:
		if v == "" {
			return
		}
		if !strings.Contains(v, "://") {
			v = "https://" + v
		}
		m.change(func(t *task.Task) { t.AddLink(v, "", "", time.Now()) }, "Added "+task.LinkLabel(v)+" to "+name)
	case inputRename:
		m.change(func(t *task.Task) { t.Title = v }, "Renamed to "+map[bool]string{true: v, false: "its tab number"}[v != ""])
	}
}

func (m *model) startNote() {
	r := m.selected()
	if r == nil {
		return
	}
	m.mode = modeNote
	m.note.SetValue(r.task.Note)
	m.note.Focus()
	m.sizeNote()
}

func (m *model) sizeNote() {
	w := detailWidth - 2
	if !m.wide() {
		w = m.width - 4
	}
	m.note.SetWidth(max(10, w))
	m.note.SetHeight(max(3, m.bodyHeight()-8))
}

func (m *model) finishNote(save bool) {
	m.mode = modeList
	m.note.Blur()
	if !save {
		m.msg = "Note not changed"
		return
	}
	v := strings.TrimSpace(m.note.Value())
	m.change(func(t *task.Task) { t.Note = v }, "Note saved")
}

func (m *model) closeSelected() {
	r := m.selected()
	if r == nil {
		return
	}
	name := r.task.Name(r.pos)
	if r.task.Stage != task.Done {
		m.msg = "Mark " + name + " done first (d), then close it."
		return
	}
	if err := m.env.Client.CloseTab(r.tab.ID); err != nil {
		m.msg = "Couldn't close the tab: " + err.Error()
		return
	}
	m.move(1)
	if live, err := reconcile.Run(m.env.host(), m.env.Store, m.env.Opts, time.Now(), nil); err == nil {
		m.live = live
	}
	m.refresh()
	m.msg = "Closed " + name
}

func (m *model) jump() tea.Cmd {
	r := m.selected()
	if r == nil {
		return nil
	}
	if err := m.env.Client.FocusTab(r.tab.ID); err != nil {
		m.msg = "Couldn't open the tab: " + err.Error()
		return nil
	}
	return tea.Quit
}

// Update.

func (m *model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width, m.height = msg.Width, msg.Height
		m.input.Width = max(10, m.width-30)
		m.sizeNote()
		return m, nil
	case refreshMsg:
		if m.mode == modeList || m.mode == modeUnlink {
			m.refresh()
		}
		return m, tick()
	case tea.MouseMsg:
		return m, m.mouse(msg)
	case tea.KeyMsg:
		switch m.mode {
		case modeInput:
			switch msg.String() {
			case "enter":
				m.finishInput()
				return m, nil
			case "esc":
				m.mode = modeList
				m.input.Blur()
				if m.inputKind == inputReason {
					stage := m.reasonStage
					m.change(func(t *task.Task) { t.SetStage(stage, "", time.Now()) }, stage.Label())
				}
				return m, nil
			}
			var cmd tea.Cmd
			m.input, cmd = m.input.Update(msg)
			return m, cmd
		case modeNote:
			switch msg.String() {
			case "ctrl+s":
				m.finishNote(true)
				return m, nil
			case "esc":
				m.finishNote(false)
				return m, nil
			}
			var cmd tea.Cmd
			m.note, cmd = m.note.Update(msg)
			return m, cmd
		case modeUnlink:
			m.mode = modeList
			if n, err := strconv.Atoi(msg.String()); err == nil {
				m.removeLink(n - 1)
			}
			return m, nil
		}
		return m, m.key(msg.String())
	}
	return m, nil
}

// Shifted digits on a US layout, for "show only this space".
var shiftedDigits = map[string]int{"!": 1, "@": 2, "#": 3, "$": 4, "%": 5, "^": 6, "&": 7, "*": 8, "(": 9, ")": 0}

func (m *model) key(k string) tea.Cmd {
	m.msg = ""
	if n, err := strconv.Atoi(k); err == nil && len(k) == 1 {
		if n == 0 {
			m.showAll()
		} else {
			m.toggleSpace(n)
		}
		return nil
	}
	if n, ok := shiftedDigits[k]; ok {
		if n == 0 {
			m.showAll()
		} else {
			m.onlySpace(n)
		}
		return nil
	}
	switch k {
	case "ctrl+c", "esc", "q", "t":
		return tea.Quit
	case "up", "k":
		m.move(-1)
	case "down", "j":
		m.move(1)
	case "pgup":
		m.move(-10)
	case "pgdown":
		m.move(10)
	case "home", "g":
		m.move(-len(m.rows))
	case "end", "G":
		m.move(len(m.rows))
	case "enter":
		return m.jump()
	case "a":
		m.setStage(task.Active)
	case "p":
		if r := m.selected(); r != nil {
			m.reasonStage = task.Paused
			m.startInput(inputReason, r.task.Reason, "PR review, QA, your own decision…")
		}
	case "v":
		if r := m.selected(); r != nil {
			m.reasonStage = task.Review
			m.startInput(inputReason, r.task.Reason, "approval, merge, testing…")
		}
	case "z":
		m.setStage(task.Parked)
	case "d":
		m.setStage(task.Done)
	case "o":
		m.setStage(task.Tool)
	case "f":
		m.togglePriority()
	case "n":
		m.startNote()
	case "l":
		m.startInput(inputLink, "", "https://github.com/… or linear.app/…")
	case "L":
		if r := m.selected(); r != nil && len(r.task.Links) > 0 {
			m.mode = modeUnlink
			m.msg = fmt.Sprintf("Remove which link? 1–%d", len(r.task.Links))
		}
	case "r":
		if r := m.selected(); r != nil {
			m.startInput(inputRename, r.task.Title, "task name (empty uses the tab number)")
		}
	case "x":
		m.closeSelected()
	case "?":
		m.help = !m.help
	}
	return nil
}

func (m *model) removeLink(i int) {
	r := m.selected()
	if r == nil || i < 0 || i >= len(r.task.Links) {
		return
	}
	label := r.task.Links[i].Display()
	m.change(func(t *task.Task) { _, _ = t.RemoveLink(strconv.Itoa(i + 1)) }, "Removed "+label)
}

// isDouble reports whether this click on target follows the last one quickly.
func (m *model) isDouble(target string) bool {
	now := time.Now()
	double := target == m.lastTarget && now.Sub(m.lastAt) < doubleClick
	m.lastTarget, m.lastAt = target, now
	if double {
		m.lastTarget = ""
	}
	return double
}

func (m *model) mouse(msg tea.MouseMsg) tea.Cmd {
	if m.mode != modeList {
		return nil
	}
	switch msg.Button {
	case tea.MouseButtonWheelUp:
		m.move(-1)
		return nil
	case tea.MouseButtonWheelDown:
		m.move(1)
		return nil
	}
	if msg.Action != tea.MouseActionPress || msg.Button != tea.MouseButtonLeft {
		return nil
	}
	x, y := msg.X, msg.Y
	switch {
	case y == m.filterY:
		for _, c := range m.chips {
			if x >= c.x0 && x < c.x1 {
				double := m.isDouble(c.target)
				switch {
				case c.target == "all":
					m.showAll()
				case double:
					m.onlySpace(spaceIndex(c.target))
				default:
					m.toggleSpace(spaceIndex(c.target))
				}
			}
		}
	case y == m.alertY:
		m.showAll()
	case y == m.footerY:
		for _, k := range m.keys {
			if x >= k.x0 && x < k.x1 {
				return m.key(k.target)
			}
		}
	default:
		if i, ok := m.linkAt[y]; ok && m.wide() && x >= m.width-3 {
			m.removeLink(i)
			return nil
		}
		if id, ok := m.rowAt[y]; ok {
			double := m.isDouble("row:" + id)
			m.sel = id
			if double {
				return m.jump()
			}
		}
	}
	return nil
}

// View.

func (m *model) wide() bool      { return m.width >= 100 }
func (m *model) bodyHeight() int { return max(3, m.height-5) }

func (m *model) View() string {
	if m.width == 0 {
		return ""
	}
	var b strings.Builder
	m.rowAt, m.linkAt = map[int]string{}, map[int]int{}
	m.alertY = -1

	// Header.
	shown := len(m.visibleSpaces())
	count := "all spaces"
	if shown < len(m.live.Workspaces) {
		count = fmt.Sprintf("%d of %d spaces", shown, len(m.live.Workspaces))
	}
	// herdr's popup frame already says "Tasks".
	count = fmt.Sprintf(" %s · %d tabs", count, len(m.rows))
	keys := "? keys · esc hide "
	b.WriteString(sDim.Render(count) + strings.Repeat(" ", max(1, m.width-ansi.StringWidth(count)-ansi.StringWidth(keys))) + sDim.Render(keys) + "\n")

	// Space filter.
	m.filterY = 1
	b.WriteString(m.filterLine() + "\n")
	b.WriteString(sDim.Render(strings.Repeat("─", m.width)) + "\n")

	// Body: list on the left, details on the right.
	m.listTop = 3
	h := m.bodyHeight()
	listW := m.width
	if m.wide() {
		listW = m.width - detailWidth - 1
	}
	list := m.listLines(listW, h)
	var detail []string
	if m.wide() {
		detail = m.detailLines(detailWidth-2, h)
	}
	sep := sDim.Render("│")
	for i := 0; i < h; i++ {
		line := ""
		if i < len(list) {
			line = list[i]
		}
		line = fit(line, listW)
		if m.wide() {
			d := ""
			if i < len(detail) {
				d = detail[i]
			}
			line += sep + " " + fit(d, detailWidth-1)
		}
		b.WriteString(line + "\n")
	}

	// Message and footer.
	if m.msg != "" {
		b.WriteString(fit(" "+m.msg, m.width) + "\n")
	} else {
		b.WriteString(sDim.Render(strings.Repeat("─", m.width)) + "\n")
	}
	m.footerY = 3 + h + 1
	b.WriteString(m.footer())
	return b.String()
}

func (m *model) filterLine() string {
	m.chips = m.chips[:0]
	x := 1
	var b strings.Builder
	b.WriteString(" ")
	add := func(target, text string, on bool) {
		st := lipgloss.NewStyle().Foreground(cDim)
		if on {
			st = lipgloss.NewStyle().Foreground(cFg).Background(cChipBG)
		}
		chip := st.Render(" " + text + " ")
		w := ansi.StringWidth(chip)
		m.chips = append(m.chips, hit{x, x + w, target})
		b.WriteString(chip + " ")
		x += w + 1
	}
	add("all", "0 All", len(m.state.Hidden) == 0)
	for i, w := range m.live.Workspaces {
		n, asking, you := 0, 0, 0
		for _, tab := range m.live.TabsIn(w.ID) {
			t, ok := m.state.Tasks[tab.ID]
			if !ok || !t.Stage.IsTask() {
				continue
			}
			n++
			switch task.GroupOf(t.Stage, tab.AgentStatus) {
			case task.Asking:
				asking++
			case task.YourTurn:
				you++
			}
		}
		text := fmt.Sprintf("%d %s %d", i+1, w.Label, n)
		if asking > 0 {
			text += sAsk.Render(fmt.Sprintf(" ?%d", asking))
		}
		if you > 0 {
			text += sYou.Render(fmt.Sprintf(" ●%d", you))
		}
		add(fmt.Sprintf("space:%d", i+1), text, !m.state.IsHidden(w.Label))
	}
	hint := "click toggles · double-click shows only one"
	if x+len(hint)+2 < m.width {
		b.WriteString(strings.Repeat(" ", m.width-x-ansi.StringWidth(hint)-1) + sDim.Render(hint))
	}
	return b.String()
}

func (m *model) listLines(w, h int) []string {
	var lines []string
	var ids []string // tab id per line, "" for headers

	// Work that needs you in hidden spaces.
	asking, waiting, names := 0, 0, map[string]bool{}
	for _, tab := range m.live.Tabs {
		ws, _ := m.live.Workspace(tab.WorkspaceID)
		t, ok := m.state.Tasks[tab.ID]
		if !ok || !m.state.IsHidden(ws.Label) {
			continue
		}
		switch task.GroupOf(t.Stage, tab.AgentStatus) {
		case task.Asking:
			asking++
			names[ws.Label] = true
		case task.YourTurn:
			waiting++
			names[ws.Label] = true
		}
	}
	alertLine := -1
	if asking+waiting > 0 {
		var ns []string
		for n := range names {
			ns = append(ns, n)
		}
		sort.Strings(ns)
		var parts []string
		if asking > 0 {
			parts = append(parts, plural(asking, "agent is", "agents are")+" asking")
		}
		if waiting > 0 {
			parts = append(parts, plural(waiting, "task is", "tasks are")+" your turn")
		}
		style := sYou
		if asking > 0 {
			style = sAsk
		}
		lines = append(lines, style.Render(fmt.Sprintf(" ● In hidden spaces (%s): %s. Click or press 0 to show all.", strings.Join(ns, ", "), strings.Join(parts, ", "))))
		ids = append(ids, "")
		alertLine = 0
	}

	spaceW := 4
	for _, r := range m.rows {
		spaceW = max(spaceW, ansi.StringWidth(r.ws.Label))
	}
	spaceW = min(spaceW, 14)
	const agentW, ageW = 11, 4
	rest := max(10, w-4-spaceW-agentW-ageW-8)
	nameW := rest * 6 / 10
	infoW := rest - nameW

	selLine := 0
	for _, g := range task.Groups {
		var rows []row
		for _, r := range m.rows {
			if r.group == g {
				rows = append(rows, r)
			}
		}
		title, hint, color := groupInfo(g)
		head := " " + lipgloss.NewStyle().Foreground(color).Bold(true).Render(title) + sDim.Render(fmt.Sprintf("  %d", len(rows)))
		if hint != "" && len(rows) > 0 {
			head += sDim.Render(" · " + hint)
		}
		if len(lines) > 0 && !(alertLine == 0 && len(lines) == 1) {
			lines = append(lines, "")
			ids = append(ids, "")
		}
		lines = append(lines, head)
		ids = append(ids, "")
		for _, r := range rows {
			sel := r.tab.ID == m.sel
			if sel {
				selLine = len(lines)
			}
			lines = append(lines, m.rowLine(r, sel, w, spaceW, nameW, agentW, ageW, infoW))
			ids = append(ids, r.tab.ID)
		}
	}

	// Scroll so the selected row stays in view.
	if selLine < m.listScroll {
		m.listScroll = selLine
	}
	if selLine >= m.listScroll+h {
		m.listScroll = selLine - h + 1
	}
	m.listScroll = max(0, min(m.listScroll, max(0, len(lines)-h)))
	end := min(len(lines), m.listScroll+h)
	for i := m.listScroll; i < end; i++ {
		y := m.listTop + i - m.listScroll
		if ids[i] != "" {
			m.rowAt[y] = ids[i]
		}
		if i == alertLine {
			m.alertY = y
		}
	}
	return lines[m.listScroll:end]
}

func (m *model) rowLine(r row, sel bool, w, spaceW, nameW, agentW, ageW, infoW int) string {
	bg := lipgloss.NewStyle()
	if sel {
		bg = bg.Background(cSelBG)
	}
	cell := func(text string, width int, fg lipgloss.TerminalColor) string {
		return bg.Foreground(fg).Render(fit(text, width))
	}
	agent, agentColor := agentLabel(r.tab.AgentStatus)
	if r.group == task.GTool {
		agent = "" // a tool tab's agent doesn't need you
	}
	name := r.task.Name(r.pos)
	nameColor := lipgloss.TerminalColor(cFg)
	if r.task.Title == "" {
		nameColor = cDim
	}
	info, infoColor := "", lipgloss.TerminalColor(cDim)
	switch {
	case r.task.Stage.TakesReason() && r.task.Reason != "":
		info, infoColor = r.task.Reason, stageColor(r.task.Stage)
	case len(r.task.Links) > 0:
		info, infoColor = "↗ "+r.task.Links[0].Display(), cLink
		if n := len(r.task.Links); n > 1 {
			info += fmt.Sprintf(" +%d", n-1)
		}
	}
	marker := " "
	if sel {
		marker = "▌"
	}
	pri := ""
	if r.task.Priority {
		pri = "!"
	}
	line := bg.Render(marker) +
		bg.Inherit(sAsk).Render(fit(pri, 2)) +
		cell(r.task.Stage.Glyph(), 2, stageColor(r.task.Stage)) +
		cell(r.ws.Label, spaceW, cDim) + bg.Render("  ") +
		cell(name, nameW, nameColor) + bg.Render(" ") +
		cell(agent, agentW, agentColor) + bg.Render(" ") +
		cell(task.Age(time.Since(r.task.Since)), ageW, cDim) + bg.Render("  ") +
		cell(info, infoW, infoColor)
	if pad := w - ansi.StringWidth(line); pad > 0 {
		line += bg.Render(strings.Repeat(" ", pad))
	}
	return line
}

func (m *model) detailLines(w, h int) []string {
	r := m.selected()
	if r == nil {
		return []string{sDim.Render("No task selected.")}
	}
	t := r.task
	var lines []string
	add := func(s ...string) { lines = append(lines, s...) }

	add(sBold.Render(fit(t.Name(r.pos), w)))
	add(sDim.Render(fit(r.ws.Label+" · tab "+r.tab.ID, w)))
	_, _, gc := groupInfo(r.group)
	line := lipgloss.NewStyle().Foreground(stageColor(t.Stage)).Render(t.Stage.Glyph() + " " + t.Stage.Label())
	if t.Stage == task.Active {
		// Only active tasks split further, by what the agent is doing.
		line += sDim.Render(" · ") + lipgloss.NewStyle().Foreground(gc).Render(groupWord(r.group))
	}
	if t.Priority {
		line = sAsk.Render("! priority") + sDim.Render(" · ") + line
	}
	add(line + sDim.Render(" · "+task.Age(time.Since(t.Since))))
	if t.Stage.TakesReason() && t.Reason != "" {
		for _, l := range wrap(reasonLabel(t.Stage)+" "+t.Reason, w) {
			add(lipgloss.NewStyle().Foreground(stageColor(t.Stage)).Render(l))
		}
	}
	add("")

	add(sDim.Render("NOTE") + strings.Repeat(" ", max(1, w-12)) + sDim.Render("n edit"))
	if m.mode == modeNote {
		add(strings.Split(m.note.View(), "\n")...)
		add(sDim.Render("ctrl+s save · esc cancel"))
	} else {
		if t.Note == "" && len(t.Log) == 0 {
			add(sDim.Render("No note yet."))
		}
		if t.Note != "" {
			add(wrap(t.Note, w)...)
		}
		for _, e := range t.Log {
			who := e.Who + " " + e.At.Local().Format("15:04")
			if time.Since(e.At) > 20*time.Hour {
				who = e.Who + " " + e.At.Local().Format("Mon 15:04")
			}
			body := wrap(e.Text, max(10, w-ansi.StringWidth(who)-1))
			for i, l := range body {
				prefix := strings.Repeat(" ", ansi.StringWidth(who))
				if i == 0 {
					prefix = sRun.Render(who)
				}
				add(prefix + " " + l)
			}
		}
	}
	add("")

	add(sDim.Render("LINKS") + strings.Repeat(" ", max(1, w-12)) + sDim.Render("l add"))
	if len(t.Links) == 0 {
		add(sDim.Render("No links yet."))
	}
	for i, l := range t.Links {
		label := l.Display()
		via := ""
		if l.By != "" {
			via = " via " + l.By
		}
		rest := max(0, w-ansi.StringWidth(label)-ansi.StringWidth(via)-6)
		url := strings.TrimPrefix(strings.TrimPrefix(l.URL, "https://"), "http://")
		line := sDim.Render(fmt.Sprintf("%d ", i+1)) + hyperlink(l.URL, sLink.Render(label)) + " " + sDim.Render(fit(url, rest)) + sRun.Render(via)
		m.linkAt[m.listTop+len(lines)] = i
		add(fit(line, w-2) + sDim.Render(" ×"))
	}
	if len(lines) > h {
		lines = lines[:h]
	}
	return lines
}

func (m *model) footer() string {
	m.keys = m.keys[:0]
	switch m.mode {
	case modeInput:
		label := map[inputKind]string{inputReason: m.reasonStage.Label() + ", " + reasonLabel(m.reasonStage), inputLink: "Add link", inputRename: "Name"}[m.inputKind]
		color := map[inputKind]lipgloss.Color{inputReason: stageColor(m.reasonStage), inputLink: cLink, inputRename: cFg}[m.inputKind]
		return fit(" "+lipgloss.NewStyle().Foreground(color).Render(label)+" "+m.input.View()+sDim.Render("  ↵ save · esc cancel"), m.width)
	case modeNote:
		return fit(sDim.Render(" Editing the note · ctrl+s save · esc cancel"), m.width)
	case modeUnlink:
		return fit(sDim.Render(" Press a link number to remove it, any other key to cancel"), m.width)
	}
	keys := [][2]string{{"enter", "↵ open"}, {"a", "a active"}, {"v", "v review"}, {"p", "p pause"}, {"z", "z park"}, {"d", "d done"}, {"o", "o tool"},
		{"f", "f priority"}, {"n", "n note"}, {"l", "l link"}, {"r", "r rename"}, {"x", "x close tab"}, {"?", "? more"}, {"esc", "esc hide"}}
	if m.help {
		keys = [][2]string{{"", "1–9 toggle space"}, {"", "shift+1–9 only that space"}, {"0", "0 all spaces"},
			{"L", "L remove link"}, {"", "j/k move"}, {"?", "? back"}, {"esc", "esc hide"}}
	}
	var b strings.Builder
	x := 1
	b.WriteString(" ")
	for _, k := range keys {
		key, rest, _ := strings.Cut(k[1], " ")
		chip := lipgloss.NewStyle().Background(cKeyBG).Render(sBold.Render(key) + " " + rest)
		w := ansi.StringWidth(chip)
		if x+w+1 > m.width {
			break
		}
		if k[0] != "" {
			m.keys = append(m.keys, hit{x, x + w, k[0]})
		}
		b.WriteString(chip + " ")
		x += w + 1
	}
	return b.String()
}

func plural(n int, one, many string) string {
	if n == 1 {
		return "1 " + one
	}
	return fmt.Sprintf("%d %s", n, many)
}

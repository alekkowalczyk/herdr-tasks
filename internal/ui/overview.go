// Package ui is the task overview that opens in a herdr popup.
package ui

import (
	"fmt"
	"slices"
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
	"github.com/alekkowalczyk/herdr-tasks/internal/view"
)

type Env struct {
	Client *herdr.Client
	Store  *store.Store
	Opts   reconcile.Options
	// The tab to select when the overview opens.
	CurrentTab string
	// Shown in the message line when the overview opens, such as why the
	// board couldn't start.
	Message string
	// OpenBoard opens a tab's task on the board, in the browser, and says
	// what you should know about it, or "" when there's nothing to say.
	OpenBoard func(tab string) string
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
	modeMove
	modeLinks  // the strip's list of every link
	modeSearch // typing a search, see search.go
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
	act   taskActions
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
	// The tab and links as they were when you pressed L, so the number you
	// press picks the link you were shown, even if a refresh changes the
	// list or the selection meanwhile.
	unlinkTab   string
	unlinkLinks []task.Link
	// The same for w: the task, its name, and the workspaces you were offered.
	moveTab  string
	moveName string
	moveTo   []herdr.Workspace
	// How many moves started here still run, and whether you asked to quit
	// meanwhile: quitting would cut a move short.
	moving    int
	quitMoved bool
	// Whether a question for the assistant is on its way.
	asking bool

	// Hit areas from the last render, for the mouse.
	rowAt      map[int]string
	linkAt     map[int]task.Link // the link each × removes, from linkTab
	linkTab    string
	chips      []hit
	keys       []hit
	filterY    int
	footerY    int
	alertY     int
	listTop    int
	listScroll int

	lastTarget string
	lastAt     time.Time

	// The search, while it's open: what you type, what it reads besides
	// the state file, and what it found.
	find    textinput.Model
	look    func() searchData
	data    searchData
	looking bool
	hits    map[string]view.Hit // by tab ID
	found   []row               // the rows found, best first
}

func newModel(env Env) (*model, error) {
	// Track any tabs created since the last sync before showing them.
	live, err := reconcile.Run(env.host(), env.Store, env.Opts, time.Now(), nil)
	if err != nil {
		return nil, err
	}
	st, err := env.Store.Load()
	if err != nil {
		return nil, err
	}
	return modelOf(env, live, st), nil
}

// modelOf is the overview of live and st, as herdr and the state file have
// them.
func modelOf(env Env, live *herdr.Live, st *store.State) *model {
	m := &model{env: env, act: realActions(env.Client, env.Store, env.Opts), live: live, state: st,
		rowAt: map[int]string{}, linkAt: map[int]task.Link{}, alertY: -1, look: realLook(env.Client)}
	m.sel = env.CurrentTab
	m.msg = env.Message
	m.rebuild()

	m.input = textinput.New()
	m.input.Prompt = ""
	m.find = textinput.New()
	m.find.Prompt = ""
	m.find.Placeholder = "a task's name, a pane's name, a recap, a note…"
	m.note = textarea.New()
	m.note.ShowLineNumbers = false
	m.note.Prompt = ""
	m.note.Placeholder = "What matters about this task?"
	m.note.CharLimit = 4000
	return m
}

type refreshMsg struct{}

// boardOpened says the board opened, with what OpenBoard had to say.
type boardOpened struct{ say string }

// assistantAsked says how asking the assistant about a task went.
type assistantAsked struct{ err error }

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
	for _, r := range view.Rows(m.state, m.live) {
		m.rows = append(m.rows, row{tab: r.Tab, task: r.Task, ws: r.Workspace, pos: r.Pos, group: r.Group})
	}
	if m.selected() == nil && len(m.rows) > 0 {
		m.sel = m.rows[0].tab.ID
	}
	m.search()
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
	rows := m.listed()
	if len(rows) == 0 {
		return
	}
	i := 0
	for k, r := range rows {
		if r.tab.ID == m.sel {
			i = k
		}
	}
	i = max(0, min(len(rows)-1, i+delta))
	m.sel = rows[i].tab.ID
}

// listed is the rows the list shows, in its order.
func (m *model) listed() []row {
	if m.searching() {
		return m.found
	}
	return m.rows
}

// change edits the selected task and syncs herdr (markers, order, sidebar).
func (m *model) change(fn func(t *task.Task), done string) {
	r := m.selected()
	if r == nil {
		return
	}
	m.changeTab(r.tab.ID, fn, done)
}

// changeTab is change for the task of tab id, whichever task is selected.
func (m *model) changeTab(id string, fn func(t *task.Task), done string) {
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
		v = task.LinkInput(v, "")
		if !task.SafeURL(v) {
			// Task.Add would refuse it.
			m.msg = "Not a valid link"
			return
		}
		done := "Added " + task.LinkLabel(v) + " to " + name
		added := false
		m.change(func(t *task.Task) { _, added = t.AddLink(v, "", "", time.Now()) }, done)
		if !added && m.msg == done {
			m.msg = "Already linked to " + name
		}
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

// startMove asks which workspace to move the selected task to.
func (m *model) startMove() {
	r := m.selected()
	if r == nil {
		return
	}
	wss := otherWorkspaces(m.live, r.tab.WorkspaceID)
	if len(wss) == 0 {
		m.msg = "No other workspace to move to."
		return
	}
	m.mode = modeMove
	m.moveTab, m.moveName, m.moveTo = r.tab.ID, r.task.Name(r.pos), wss
	m.msg = movePrompt(wss)
}

// moveTask moves the task of tab, named name, to ws, as a command.
func (m *model) moveTask(tab, name string, ws herdr.Workspace) tea.Cmd {
	to := task.CleanLabel(ws.Label)
	m.msg = "Moving " + name + " to " + to + "…"
	m.moving++
	mv := m.act.move
	return func() tea.Msg {
		newTab, err := mv(tab, ws.ID)
		return actionDone{done: "Moved " + name + " to " + to, tab: newTab, err: err}
	}
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
	return m.quit("enter")
}

// openBoard opens the selected task on the board, in the browser. Starting
// the board server takes a moment, so it runs as a command. The overview
// goes once the browser opens, unless there's something to say.
func (m *model) openBoard() tea.Cmd {
	r := m.selected()
	if r == nil || m.env.OpenBoard == nil {
		return nil
	}
	m.msg = "Opening the board…"
	open, tab := m.env.OpenBoard, r.tab.ID
	return func() tea.Msg { return boardOpened{open(tab)} }
}

// askAssistant asks the assistant about the selected task, which shows it.
// Starting Claude takes a moment, so it runs as a command. The overview goes
// once the assistant shows, unless something went wrong.
func (m *model) askAssistant() tea.Cmd {
	r := m.selected()
	if r == nil || m.asking {
		return nil
	}
	m.msg, m.asking = "Opening the assistant…", true
	ask, tab, from := m.act.ask, r.tab.ID, m.env.CurrentTab
	return func() tea.Msg { return assistantAsked{ask(tab, from)} }
}

// quit ends the overview, pressing k. While a move started here runs, it
// waits for the move, which quitting would cut short; a ctrl+c once you've
// asked to quit doesn't wait.
func (m *model) quit(k string) tea.Cmd {
	if m.moving == 0 || k == "ctrl+c" && m.quitMoved {
		return tea.Quit
	}
	m.quitMoved = true
	m.msg = "Moving… quit when it's done"
	return nil
}

// Update.

func (m *model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width, m.height = msg.Width, msg.Height
		m.input.Width = max(10, m.width-30)
		m.find.Width = max(10, m.width-50)
		m.sizeNote()
		return m, nil
	case refreshMsg:
		switch m.mode {
		case modeList, modeUnlink, modeMove:
			m.refresh()
		case modeSearch:
			m.refresh()
			return m, tea.Batch(tick(), m.lookAgain())
		}
		return m, tick()
	case searchLooked:
		m.looking = false
		if m.mode == modeSearch {
			m.data = msg.data
			m.search()
		}
		return m, nil
	case boardOpened:
		if msg.say == "" {
			return m, m.quit("b")
		}
		m.msg = msg.say
		return m, nil
	case assistantAsked:
		m.asking = false
		if msg.err == nil {
			return m, m.quit("A")
		}
		m.msg = msg.err.Error()
		return m, nil
	case actionDone:
		// Only a move ends with this. One that failed cancels a quit you
		// asked for meanwhile, so you see why.
		m.moving = max(0, m.moving-1)
		if msg.err != nil {
			m.quitMoved = false
		}
		if m.moving == 0 && m.quitMoved {
			return m, tea.Quit
		}
		// The selection follows the task to its new tab.
		if msg.tab != "" {
			m.sel = msg.tab
		}
		m.refresh()
		m.msg = msg.done
		if msg.err != nil {
			m.msg = msg.err.Error()
		}
		return m, nil
	case tea.MouseMsg:
		return m, m.mouse(msg)
	case tea.KeyMsg:
		switch m.mode {
		case modeSearch:
			return m, m.searchKey(msg)
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
			tab, links := m.unlinkTab, m.unlinkLinks
			m.unlinkTab, m.unlinkLinks = "", nil
			if n, err := strconv.Atoi(msg.String()); err == nil {
				if n < 1 || n > len(links) {
					m.msg = fmt.Sprintf("No link %d", n)
				} else {
					m.removeLink(tab, links[n-1])
				}
			}
			return m, nil
		case modeMove:
			m.mode = modeList
			tab, name, wss := m.moveTab, m.moveName, m.moveTo
			m.moveTab, m.moveName, m.moveTo = "", "", nil
			n, err := strconv.Atoi(msg.String())
			switch {
			case err != nil:
				m.msg = ""
			case n < 1 || n > len(wss):
				m.msg = fmt.Sprintf("No workspace %d", n)
			default:
				return m, m.moveTask(tab, name, wss[n-1])
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
		return m.quit(k)
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
	case "/":
		return m.startSearch()
	case "b":
		return m.openBoard()
	case "A":
		return m.askAssistant()
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
			m.unlinkTab, m.unlinkLinks = r.tab.ID, task.Grouped(r.task.Links)
			m.msg = fmt.Sprintf("Remove which link? 1–%d", len(m.unlinkLinks))
		}
	case "r":
		if r := m.selected(); r != nil {
			m.startInput(inputRename, r.task.Title, "task name (empty uses the tab number)")
		}
	case "w":
		m.startMove()
	case "m":
		if r := m.selected(); r != nil {
			m.msg = muteChannel(m.act.mute, r.task.Slack)
		}
	case "x":
		m.closeSelected()
	case "?":
		m.help = !m.help
	}
	return nil
}

// removeLink removes l, a link you were shown, from tab's task. It goes by
// l's URL in the task as saved now, since another writer, such as the Claude
// hook, may have changed the list since you saw it.
func (m *model) removeLink(tab string, l task.Link) {
	label := task.CleanLabel(l.Display())
	done := "Removed " + label
	gone := false
	m.changeTab(tab, func(t *task.Task) {
		// RemoveLink is given its number, in task.Grouped order, as a URL
		// could also read as a number or an issue ID.
		i := slices.IndexFunc(task.Grouped(t.Links), func(o task.Link) bool { return o.URL == l.URL })
		if i < 0 {
			gone = true
			return
		}
		_, _ = t.RemoveLink(strconv.Itoa(i + 1))
	}, done)
	if gone && m.msg == done {
		m.msg = label + " was removed already"
	}
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
	if m.mode != modeList && m.mode != modeSearch {
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
		if l, ok := m.linkAt[y]; ok && m.wide() && x >= m.width-3 {
			m.removeLink(m.linkTab, l)
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
	m.rowAt, m.linkAt = map[int]string{}, map[int]task.Link{}
	m.alertY = -1

	// Header.
	shown := len(m.visibleSpaces())
	count := "all spaces"
	if shown < len(m.live.Workspaces) {
		count = fmt.Sprintf("%d of %d spaces", shown, len(m.live.Workspaces))
	}
	// herdr's popup frame already says "Tasks".
	tabs := fmt.Sprintf("%d tabs", len(m.rows))
	if m.searching() {
		tabs = fmt.Sprintf("%d of %d tabs match", len(m.found), len(m.rows))
	}
	count = fmt.Sprintf(" %s · %s", count, tabs)
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
	if asking+waiting > 0 && !m.searching() {
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
	if m.searching() {
		// Where a search found a task takes more room.
		nameW = rest * 4 / 10
	}
	infoW := rest - nameW

	selLine := 0
	if m.searching() && len(m.found) == 0 {
		lines = append(lines, sDim.Render(" No task matches "+task.CleanLabel(m.find.Value())+"."))
		ids = append(ids, "")
	}
	for _, sec := range m.sections() {
		head := " " + lipgloss.NewStyle().Foreground(sec.color).Bold(true).Render(sec.title) + sDim.Render(fmt.Sprintf("  %d", len(sec.rows)))
		if sec.hint != "" && len(sec.rows) > 0 {
			head += sDim.Render(" · " + sec.hint)
		}
		if len(lines) > 0 && !(alertLine == 0 && len(lines) == 1) {
			lines = append(lines, "")
			ids = append(ids, "")
		}
		lines = append(lines, head)
		ids = append(ids, "")
		for _, r := range sec.rows {
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

// section is a part of the list: the tasks of a group, or, while you
// search, those found in one place.
type section struct {
	title, hint string
	color       lipgloss.Color
	rows        []row
}

func (m *model) sections() []section {
	if m.searching() {
		return m.foundSections()
	}
	var out []section
	for _, g := range task.Groups {
		sec := section{}
		sec.title, sec.hint, sec.color = groupInfo(g)
		for _, r := range m.rows {
			if r.group == g {
				sec.rows = append(sec.rows, r)
			}
		}
		out = append(out, sec)
	}
	return out
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
		info, infoColor = task.CleanLabel(r.task.Reason), stageColor(r.task.Stage)
	case len(r.task.Links) > 0:
		// The first of them as the strip and the LINKS list show them.
		info, infoColor = "↗ "+task.CleanLabel(task.Grouped(r.task.Links)[0].Display()), cLink
		if n := len(r.task.Links); n > 1 {
			info += fmt.Sprintf(" +%d", n-1)
		}
	}
	infoCell := cell(info, infoW, infoColor)
	// What a search found outside the name shows in its place.
	if h, ok := m.hits[r.tab.ID]; ok && m.searching() && h.Match > view.InName {
		infoCell = foundCell(h, infoW, bg)
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
		infoCell
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
			who := task.CleanLabel(e.Who) + " " + e.At.Local().Format("15:04")
			if time.Since(e.At) > 20*time.Hour {
				who = task.CleanLabel(e.Who) + " " + e.At.Local().Format("Mon 15:04")
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
	links := task.Grouped(t.Links)
	if m.mode == modeUnlink && r.tab.ID == m.unlinkTab {
		// The numbers you're choosing from don't move.
		links = m.unlinkLinks
	}
	if len(links) == 0 {
		add(sDim.Render("No links yet."))
	}
	m.linkTab = r.tab.ID
	section := ""
	for i, l := range links {
		// Each section's name heads its links. They're grouped, so each
		// shows once.
		if s := task.KindOf(l.URL).Section(); s != section {
			section = s
			add(sectionName(s))
		}
		// Everything here can come from a hand-edited state file, so it's
		// cleaned before it's measured or printed. An unsafe URL isn't shown
		// at all: a terminal could still find a link in its text.
		label := task.CleanLabel(l.Display())
		if section == "Slack" {
			label = linkItem(l) // the date, then the label, as on the strip
		}
		url := "unsafe link"
		if task.SafeURL(l.URL) {
			url = task.CleanLabel(strings.TrimPrefix(strings.TrimPrefix(l.URL, "https://"), "http://"))
		} else if l.Label == "" {
			label = url
		}
		via := ""
		if by := task.CleanLabel(l.By); by != "" {
			via = " via " + by
		}
		rest := max(0, w-ansi.StringWidth(label)-ansi.StringWidth(via)-6)
		line := sDim.Render(fmt.Sprintf("%d ", i+1)) + sLink.Render(hyperlink(l.URL, label)) + " " + sDim.Render(fit(url, rest)) + sRun.Render(via)
		m.linkAt[m.listTop+len(lines)] = l
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
	case modeMove:
		return fit(sDim.Render(" Press a workspace number to move the task there, any other key to cancel"), m.width)
	case modeSearch:
		return fit(" "+sBold.Render("/")+" "+m.find.View()+sDim.Render("  ↑↓ choose · ↵ open · esc back to the list"), m.width)
	}
	keys := [][2]string{{"enter", "↵ open"}, {"/", "/ search"}, {"a", "a active"}, {"v", "v review"}, {"p", "p pause"}, {"z", "z park"}, {"d", "d done"}, {"o", "o tool"},
		{"f", "f priority"}, {"n", "n note"}, {"l", "l link"}, {"r", "r rename"}, {"x", "x close tab"}, {"?", "? more"}, {"esc", "esc hide"}}
	if m.help {
		keys = [][2]string{{"b", "b open on the board"}, {"A", "A assistant"}, {"", "1–9 toggle space"}, {"", "shift+1–9 only that space"}, {"0", "0 all spaces"},
			{"L", "L unlink"}, {"w", "w move to workspace"}, {"m", "m mute channel"}, {"", "j/k move"}, {"?", "? back"}, {"esc", "esc hide"}}
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

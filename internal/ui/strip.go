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

	"github.com/alekkowalczyk/herdr-tasks/internal/assistant"
	"github.com/alekkowalczyk/herdr-tasks/internal/herdr"
	"github.com/alekkowalczyk/herdr-tasks/internal/move"
	"github.com/alekkowalczyk/herdr-tasks/internal/reconcile"
	"github.com/alekkowalczyk/herdr-tasks/internal/slack"
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
	m := &stripModel{c: c, st: st, opts: opts, pane: pane, bin: bin, binID: fileID(bin), rows: strip.Rows, act: realActions(c, st, opts)}
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

// taskActions are what the keys for moving, continuing, research, muting
// and asking the assistant do. Tests replace them.
type taskActions struct {
	move     func(tabID, workspace string) (newTab string, err error)
	cont     func(tabID string) error
	research func(tabID string) error
	mute     func(channel, place string) (bool, error)
	// ask asks the assistant about tabID's task and shows it; it goes back
	// to from, the tab you were in.
	ask func(tabID, from string) error
}

// realActions does them through herdr and the state in st's folder, as the
// move and slack commands do.
func realActions(c *herdr.Client, st *store.Store, opts reconcile.Options) taskActions {
	h := strip.Host{Client: c}
	return taskActions{
		move: func(tabID, to string) (string, error) {
			return move.Task(h, st, opts, time.Now(), tabID, to)
		},
		cont: func(tabID string) error {
			return slack.Continue(h, st, opts, tabID, syscall.Kill, time.Now())
		},
		research: func(tabID string) error {
			home, _ := os.UserHomeDir()
			set, err := slack.SettingsFrom(opts, home)
			if err != nil {
				return err
			}
			// The guard's hook runs this binary.
			exe, err := os.Executable()
			if err == nil {
				exe, err = filepath.EvalSymlinks(exe)
			}
			if err != nil {
				return err
			}
			return slack.StartResearch(h, st, opts, set, tabID, exe, st.Dir, time.Now())
		},
		mute: func(channel, place string) (bool, error) {
			added := false
			err := slack.Update(st.Dir, func(s *slack.State) error {
				added = s.Mute(channel, place)
				return nil
			})
			return added, err
		},
		ask: func(tabID, from string) error {
			cfg, err := assistant.ConfigFor(opts, st.Dir)
			if err != nil {
				return err
			}
			return assistant.Show(h, st, opts, cfg, tabID, from, time.Now())
		},
	}
}

// actionDone ends a task action that ran as a command.
type actionDone struct {
	done string // the message when it worked
	tab  string // the task's tab afterwards, as a move changes it
	err  error
}

// muteChannel mutes the channel of a, a task's Slack ask, and says how that went.
func muteChannel(mute func(channel, place string) (bool, error), a *task.SlackAsk) string {
	if a == nil || a.Channel == "" {
		return "Only a Slack task's channel can be muted."
	}
	id, place := task.CleanLabel(a.Channel), task.CleanLabel(a.Place)
	if place == "" {
		place = id
	}
	added, err := mute(a.Channel, a.Place)
	switch {
	case err != nil:
		return "Couldn't mute: " + err.Error()
	case !added:
		return place + " is muted already"
	}
	return "Muted " + place + ". Undo: herdr-tasks slack unmute " + id
}

// otherWorkspaces are the session's workspaces but ws, in herdr's order.
func otherWorkspaces(live *herdr.Live, ws string) []herdr.Workspace {
	var out []herdr.Workspace
	for _, w := range live.Workspaces {
		if w.ID != ws {
			out = append(out, w)
		}
	}
	return out
}

// movePrompt numbers the workspaces a task can move to.
func movePrompt(wss []herdr.Workspace) string {
	parts := make([]string, len(wss))
	for i, w := range wss {
		parts[i] = fmt.Sprintf("%d %s", i+1, task.CleanLabel(w.Label))
	}
	return "Move to workspace: " + strings.Join(parts, "  ")
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
	act  taskActions

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
	// From the last render: how many links the links column leaves out, and
	// where its "+N"s are, by row.
	hidden int
	plusAt map[int]hit
	// The links as they were when you started unlinking, so the numbers you
	// read don't move when a refresh changes the list.
	unlinkLinks []task.Link
	// The workspaces you were offered when you pressed w, numbered from 1.
	moveTo []herdr.Workspace
	// A slow action, such as starting research, is running.
	busy bool
	// The all-links view from the last render, and the row of cells it
	// starts at, which ↑ and ↓ move.
	linksPage linkPage
	linksTop  int
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
		v = task.LinkInput(v, "")
		if !task.SafeURL(v) {
			// Task.Add would refuse it.
			m.msg = "Not a valid link"
			return nil
		}
		done := "Added " + task.LinkLabel(v)
		added := false
		cmd := m.change(func(t *task.Task) { _, added = t.AddLink(v, "", "", time.Now()) }, done)
		if !added && m.msg == done {
			m.msg = "Already linked"
		}
		return cmd
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

// startUnlink grows the strip to number every link, and asks which to remove.
func (m *stripModel) startUnlink() tea.Cmd {
	m.mode = modeUnlink
	m.unlinkLinks = task.Grouped(m.t.Links)
	m.input.SetValue("")
	m.input.Placeholder = fmt.Sprintf("1–%d", len(m.unlinkLinks))
	m.input.Width = 8
	m.input.Focus()
	m.rows = noteRows
	return m.fit()
}

// finishUnlink removes the link whose number you typed, or cancels. The
// number is one in the list you were shown, and the link goes by its URL, so
// the link you saw goes even if the list has changed since.
func (m *stripModel) finishUnlink(save bool) tea.Cmd {
	links := m.unlinkLinks
	m.unlinkLinks = nil
	m.mode = modeList
	m.input.Blur()
	m.rows = strip.Rows
	fit := m.fit()
	v := strings.TrimSpace(m.input.Value())
	if !save || v == "" || m.t == nil {
		return fit
	}
	n, err := strconv.Atoi(v)
	if err != nil || n < 1 || n > len(links) {
		m.msg = fmt.Sprintf("No link %s. This task has %d.", v, len(links))
		return fit
	}
	l := links[n-1]
	done := "Removed " + linkItem(l)
	gone := false
	cmd := m.change(func(t *task.Task) {
		_, err := t.RemoveLink(l.URL)
		gone = err != nil
	}, done)
	if gone && m.msg == done {
		m.msg = linkItem(l) + " was removed already"
	}
	return tea.Batch(fit, cmd)
}

// startLinks grows the strip to show every link, numbered as L numbers them.
func (m *stripModel) startLinks() tea.Cmd {
	m.mode = modeLinks
	m.rows = noteRows
	m.linksTop = 0
	return m.fit()
}

// finishLinks takes the strip back to its buttons.
func (m *stripModel) finishLinks() tea.Cmd {
	m.mode = modeList
	m.rows = strip.Rows
	return m.fit()
}

// startMove offers the session's other workspaces to move this task to.
func (m *stripModel) startMove() {
	live, err := m.c.Live()
	if err != nil {
		m.msg = "Can't reach herdr: " + err.Error()
		return
	}
	m.moveTo = otherWorkspaces(live, m.tab.WorkspaceID)
	if len(m.moveTo) == 0 {
		m.msg = "No other workspace to move to."
		return
	}
	m.mode = modeMove
	m.input.SetValue("")
	m.input.Placeholder = ""
	m.input.Width = 4
	m.input.Focus()
}

// finishMove moves this task to the workspace whose number you typed, or
// cancels. The move closes this tab, and this strip with it, once the task
// is in its new tab, which gets a strip of its own.
func (m *stripModel) finishMove(save bool) tea.Cmd {
	wss := m.moveTo
	m.moveTo = nil
	m.mode = modeList
	m.input.Blur()
	v := strings.TrimSpace(m.input.Value())
	if !save || v == "" || m.t == nil {
		return nil
	}
	n, err := strconv.Atoi(v)
	if err != nil || n < 1 || n > len(wss) {
		m.msg = fmt.Sprintf("No workspace %s. Pick 1–%d.", v, len(wss))
		return nil
	}
	ws := wss[n-1]
	tab, mv := m.tab.ID, m.act.move
	return m.work("Moving to "+task.CleanLabel(ws.Label)+"…", "", func() error {
		_, err := mv(tab, ws.ID)
		return err
	})
}

// work runs a slow action as a command, saying what it's doing meanwhile,
// and done once it has.
func (m *stripModel) work(doing, done string, fn func() error) tea.Cmd {
	m.msg, m.busy = doing, true
	return func() tea.Msg { return actionDone{done: done, err: fn()} }
}

// action runs a button or key.
func (m *stripModel) action(k string) tea.Cmd {
	if m.busy {
		switch k {
		case "c", "s", "w", "A":
			return nil // one at a time
		case "a", "v", "p", "z", "d", "o", "x", "m", "n", "l", "L", "r", "f":
			// The action may be moving the tab these change or close.
			m.msg = "Wait for it to finish"
			return nil
		}
	}
	m.msg = ""
	if m.t == nil {
		return nil
	}
	tab := m.tab.ID
	switch k {
	case "c":
		if m.t.Slack == nil || !m.t.Slack.CanContinue() {
			m.msg = "No locked research session in this tab."
			return nil
		}
		cont := m.act.cont
		return m.work("Continuing with your tools…", "Continued with your tools", func() error { return cont(tab) })
	case "s":
		switch {
		case m.t.Slack == nil:
			m.msg = "Only a Slack task has research."
		case !m.t.Slack.CanStart():
			m.msg = "Research already started."
		default:
			research := m.act.research
			return m.work("Starting research…", "Research started", func() error { return research(tab) })
		}
	case "m":
		m.msg = muteChannel(m.act.mute, m.t.Slack)
	case "w":
		m.startMove()
	case "A":
		// Showing the assistant takes you there, so there's nothing to say.
		ask := m.act.ask
		return m.work("Opening the assistant…", "", func() error { return ask(tab, tab) })
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
	case "f":
		if !m.t.Stage.TakesPriority() {
			m.msg = fmt.Sprintf("A %s task can't be a priority.", strings.ToLower(m.t.Stage.Label()))
			return nil
		}
		on := !m.t.Priority
		done := "Priority"
		if !on {
			done = "Not a priority"
		}
		return m.change(func(t *task.Task) { _ = t.SetPriority(on) }, done)
	case "n":
		return m.startNote()
	case "l":
		m.startInput(inputLink, "", "https://github.com/… or linear.app/…")
	case "L":
		if len(m.t.Links) > 0 {
			return m.startUnlink()
		}
	case "+":
		if len(m.t.Links) > 0 {
			return m.startLinks()
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
	case actionDone:
		m.busy = false
		m.msg = msg.done
		if msg.err != nil {
			m.msg = msg.err.Error()
		}
		return m, m.load
	case stripTick:
		// Not while an action runs: restarting would cut it short.
		if id := fileID(m.bin); id != 0 && id != m.binID && m.mode == modeList && !m.busy {
			m.restart = true
			return m, tea.Quit
		}
		return m, m.load
	case tea.MouseMsg:
		if m.mode != modeList || msg.Action != tea.MouseActionPress || msg.Button != tea.MouseButtonLeft {
			return m, nil
		}
		if p, ok := m.plusAt[msg.Y]; ok && msg.X >= p.x0 && msg.X < p.x1 {
			return m, m.action(p.target)
		}
		if msg.Y != m.height-1 {
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
		case modeLinks:
			switch msg.String() {
			case "+", "esc":
				return m, m.finishLinks()
			case "up", "k":
				m.linksTop = max(0, m.linksTop-1)
			case "down", "j":
				m.linksTop = min(m.linksTop+1, m.linksPage.maxTop)
			}
			return m, nil
		case modeUnlink, modeMove:
			finish := m.finishUnlink
			if m.mode == modeMove {
				finish = m.finishMove
			}
			switch msg.String() {
			case "enter":
				return m, finish(true)
			case "esc":
				return m, finish(false)
			}
			if msg.Type == tea.KeySpace || msg.Type == tea.KeyRunes && strings.Trim(string(msg.Runes), "0123456789") != "" {
				return m, nil // only numbers
			}
			var cmd tea.Cmd
			m.input, cmd = m.input.Update(msg)
			return m, cmd
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
	head := " "
	if t.Priority {
		head += sAsk.Render("! priority") + sDim.Render(" · ")
	}
	head += lipgloss.NewStyle().Foreground(stageColor(t.Stage)).Render(t.Stage.Glyph() + " " + t.Stage.Label())
	if t.Stage == task.Active {
		head += sDim.Render(" · ") + lipgloss.NewStyle().Foreground(gc).Bold(g == task.Asking).Render(groupWord(g))
	}
	head += sDim.Render(" · " + task.Age(time.Since(t.Since)))
	if t.Stage.TakesReason() && t.Reason != "" {
		head += sDim.Render(" · ") + lipgloss.NewStyle().Foreground(stageColor(t.Stage)).Render(reasonLabel(t.Stage)+" "+task.CleanLabel(t.Reason))
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
	if m.mode == modeUnlink || m.mode == modeLinks {
		list := numberedLinks(m.unlinkLinks, w, body, "herdr-tasks link rm")
		if m.mode == modeLinks {
			// As they are now: nothing here takes a number.
			m.linksPage = pagedLinks(task.Grouped(t.Links), w, body, m.linksTop)
			m.linksTop, list = m.linksPage.top, m.linksPage.lines
		}
		lines = append(lines, list...)
		for len(lines) <= body {
			lines = append(lines, "")
		}
		lines = append(lines, m.bottom(w))
		return strings.Join(lines, "\n")
	}

	// Note and agent lines on the left, links on the right. The links take
	// what the left side doesn't need, and never less than half the strip
	// or 52 cells, whichever is less.
	linkW := 0
	noteW := w - 3
	if len(t.Links) > 0 {
		linkW = max(min(52, w/2), w-4-max(30, leftWidth(t)))
		noteW = w - linkW - 4 // " " note " │ " links
	}
	var left []string
	if t.Note != "" {
		left = append(left, wrap(t.Note, noteW)...)
	}
	for _, e := range t.Log {
		who := logWho(e)
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
	col := linkRows(t.Links, linkW, body, false)
	m.hidden, m.plusAt = col.hidden, map[int]hit{}
	for i, r := range col.rows {
		if col.plus[i] > 0 {
			end := 1 + noteW + 3 + ansi.StringWidth(r)
			m.plusAt[1+i] = hit{end - col.plus[i], end, "+"}
		}
	}
	for i := 0; i < body; i++ {
		l, r := "", ""
		if i < len(left) {
			l = left[i]
		}
		if i < len(col.rows) {
			r = col.rows[i]
		}
		line := " " + fit(l, noteW)
		if linkW > 0 {
			line += sDim.Render(" │ ") + fit(r, linkW)
		}
		lines = append(lines, line)
	}
	lines = append(lines, m.bottom(w))
	return strings.Join(lines, "\n")
}

// logWho is who wrote a log entry and when, as the strip shows it.
func logWho(e task.Entry) string {
	return task.CleanLabel(e.Who) + " " + e.At.Local().Format("15:04")
}

// leftWidth is the widest line of the strip's left side before wrapping:
// the note's lines and the log's, or "No note yet.".
func leftWidth(t *task.Task) int {
	if t.Note == "" && len(t.Log) == 0 {
		return ansi.StringWidth("No note yet.")
	}
	widest := 0
	for _, l := range strings.Split(task.CleanText(t.Note), "\n") {
		widest = max(widest, ansi.StringWidth(l))
	}
	for _, e := range t.Log {
		who := ansi.StringWidth(logWho(e)) + 1
		for _, l := range strings.Split(task.CleanText(e.Text), "\n") {
			widest = max(widest, who+ansi.StringWidth(l))
		}
	}
	return widest
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
		return fit(" "+lipgloss.NewStyle().Foreground(cLink).Render("Remove link number")+" "+m.input.View()+sDim.Render("  ↵ remove · esc cancel"), w)
	case modeMove:
		return fit(" "+lipgloss.NewStyle().Foreground(cLink).Render(movePrompt(m.moveTo)+", then Enter")+" "+m.input.View()+sDim.Render("  esc cancel"), w)
	case modeLinks:
		more := ""
		if p := m.linksPage; p.scrolls {
			more = fmt.Sprintf(" %d–%d of %d · ↑↓ more", p.first, p.last, p.n)
		}
		return fit(" "+lipgloss.NewStyle().Foreground(cLink).Render("All links")+sDim.Render(more+" · + or esc to close"), w)
	}
	type button struct{ key, label string }
	buttons := []button{{"a", "active"}, {"v", "review"}, {"p", "pause"}, {"z", "park"}, {"d", "done"}, {"o", "tool"}}
	if m.t.Stage.TakesPriority() {
		buttons = append(buttons, button{"f", "priority"})
	}
	if a := m.t.Slack; a != nil && a.CanContinue() {
		buttons = append(buttons, button{"c", "continue"})
	}
	if a := m.t.Slack; a != nil && a.CanStart() {
		buttons = append(buttons, button{"s", "research"})
	}
	buttons = append(buttons, button{"n", "note"}, button{"l", "link"}, button{"r", "rename"})
	if len(m.t.Links) > 0 {
		buttons = append(buttons, button{"L", "unlink"})
	}
	if m.hidden > 0 {
		buttons = append(buttons, button{"+", "links"})
	}
	buttons = append(buttons, button{"w", "move"})
	if m.t.Slack != nil {
		buttons = append(buttons, button{"m", "mute"})
	}
	if m.t.Stage == task.Done {
		buttons = append(buttons, button{"x", "close tab"})
	}
	buttons = append(buttons, button{"A", "assistant"}, button{"enter", "tasks"}, button{"esc", "back"})
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
		if bt.key == "f" && m.t.Priority {
			st = st.Foreground(cAsk)
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

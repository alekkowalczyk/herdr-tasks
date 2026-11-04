// Package assistant is the assistant: a Claude Code session in a tab of its
// own that knows every task, and works with herdr and herdr-tasks when you
// ask it to. Its tab is a tool, not a task, and the Assistant on its record
// marks it. Claude starts with a guide to herdr-tasks in its system prompt,
// and before each of your messages its hook gives it the tasks as they are.
package assistant

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/alekkowalczyk/herdr-tasks/internal/claudelog"
	"github.com/alekkowalczyk/herdr-tasks/internal/herdr"
	"github.com/alekkowalczyk/herdr-tasks/internal/reconcile"
	"github.com/alekkowalczyk/herdr-tasks/internal/slack"
	"github.com/alekkowalczyk/herdr-tasks/internal/store"
	"github.com/alekkowalczyk/herdr-tasks/internal/strip"
	"github.com/alekkowalczyk/herdr-tasks/internal/task"
)

const (
	// Label names the assistant's tab, and the workspace it's made in when
	// there's none with that name.
	Label = "Assistant"
	// agentName is Claude's name in herdr, which herdr's agent commands take.
	agentName = "assistant"
	// lockName is the lock Open holds, so two opens at once make one tab.
	lockName = "assistant"
)

// Herdr is what the assistant needs of herdr. strip.Host has it all.
type Herdr interface {
	reconcile.Herdr
	slack.Layouts
	slack.AgentStarter
	CreateWorkspace(label, cwd string) (herdr.Created, error)
	CreateTab(workspaceID, label, cwd string) (herdr.Created, error)
	CloseTab(tabID string) error
	Panes() ([]herdr.Pane, error)
	FocusTab(id string) error
	TypeText(paneID, text string) error
}

var _ Herdr = strip.Host{}

// Config is how the assistant starts.
type Config struct {
	Exe       string // herdr-tasks, which Claude's hook runs
	Dir       string // the plugin's state folder; the guide and settings go in its assistant folder
	Cwd       string // where Claude starts, as assistant_cwd says
	Home      string // the home folder, for a Cwd that starts with ~
	ClaudeDir string // Claude Code's config folder, which has the session logs
}

var (
	// ErrOwnTab is why the assistant can't be asked about its own tab.
	ErrOwnTab = errors.New("That's the assistant's own tab, not a task")
	// ErrAsking is why nothing was typed into the assistant: Claude is asking
	// you something there, and typing could answer it.
	ErrAsking = errors.New("The assistant is waiting for your answer. Answer it first")
	// ErrNoBack is why going back from the assistant showed nothing.
	ErrNoBack = errors.New("No tab to go back to")
)

// Find is the assistant's tab in live, or "" when there's none. With two,
// as when you copied a state file, the first in herdr's order is it.
func Find(s *store.State, live *herdr.Live) string {
	for _, tab := range live.Tabs {
		if t, ok := s.Tasks[tab.ID]; ok && t.Assistant != nil {
			return tab.ID
		}
	}
	return ""
}

// Pane is the pane Claude runs in in the assistant's tab: the one running
// Claude, or else its first but the strip, by herdr's list of panes. It's
// false when the tab has none.
func Pane(tab string, panes []herdr.Pane) (herdr.Pane, bool) {
	var first *herdr.Pane
	for i, p := range panes {
		if p.TabID != tab || p.Label == strip.Label {
			continue
		}
		if p.Agent == "claude" {
			return p, true
		}
		if first == nil {
			first = &panes[i]
		}
	}
	if first == nil {
		return herdr.Pane{}, false
	}
	return *first, true
}

// Opened is the assistant's tab, and the pane Claude runs in.
type Opened struct {
	Tab, Pane string
	New       bool // the tab was made just now
	Started   bool // Claude was started just now
}

// Open finds the assistant's tab, or makes one, and starts Claude in it
// when nothing runs there: the session it had, or a new one. It doesn't
// show the tab in herdr. When the tab is there but Claude didn't start, it
// returns the tab with the error.
func Open(h Herdr, st *store.Store, opt reconcile.Options, cfg Config, now time.Time) (Opened, error) {
	unlock, err := st.Hold(lockName)
	if err != nil {
		return Opened{}, err
	}
	defer unlock()
	live, err := h.Live()
	if err != nil {
		return Opened{}, err
	}
	s, err := st.Load()
	if err != nil {
		return Opened{}, err
	}
	var o Opened
	if o.Tab = Find(s, live); o.Tab == "" {
		own := filepath.Join(cfg.Dir, "assistant")
		cwd, err := Cwd(cfg.Cwd, cfg.Home, own)
		if err != nil {
			return Opened{}, err
		}
		if err := os.MkdirAll(own, 0o700); err != nil {
			return Opened{}, err
		}
		if o.Tab, err = makeTab(h, st, opt, live, cwd, now); err != nil {
			return Opened{}, err
		}
		o.New = true
		if s, err = st.Load(); err != nil {
			return o, err
		}
	}
	id := s.Tasks[o.Tab].Assistant.SessionID
	panes, err := h.Panes()
	if err != nil {
		return o, err
	}
	p, ok := Pane(o.Tab, panes)
	if !ok {
		return o, fmt.Errorf("the assistant's tab %s has no pane", o.Tab)
	}
	o.Pane = p.ID
	// Whatever runs there, Claude or another program, is left alone.
	if p.Agent != "" {
		return o, nil
	}
	args, err := start(cfg, id)
	if err == nil {
		err = slack.StartClaude(h, agentName, p.ID, args)
	}
	switch {
	case err == nil:
		o.Started = true
	case slack.MayHaveLaunched(err):
		o.Started = true
		err = fmt.Errorf("Claude may not have finished starting in the assistant's tab: %w", err)
	default:
		err = fmt.Errorf("Claude didn't start in the assistant's tab: %w", err)
	}
	return o, err
}

// makeTab makes the assistant's tab, in the first workspace called Label, in
// any case, or in a new one, and saves its record: a tool, with a new
// Claude session. A tab whose record can't be saved is closed again.
func makeTab(h Herdr, st *store.Store, opt reconcile.Options, live *herdr.Live, cwd string, now time.Time) (string, error) {
	var c herdr.Created
	var err error
	ws := ""
	for _, w := range live.Workspaces {
		if strings.EqualFold(w.Label, Label) {
			ws = w.ID
			break
		}
	}
	if ws != "" {
		c, err = h.CreateTab(ws, Label, cwd)
	} else if c, err = h.CreateWorkspace(Label, cwd); err == nil {
		// A workspace comes with a tab herdr names. With stage markers off,
		// only the label keeps the record's title.
		_ = h.RenameTab(c.TabID, Label)
	}
	if err != nil {
		return "", fmt.Errorf("couldn't make the assistant's tab: %w", err)
	}
	_, err = reconcile.Run(h, st, opt, now, func(s *store.State, _ *herdr.Live) error {
		t, ok := s.Tasks[c.TabID]
		if !ok {
			return fmt.Errorf("herdr doesn't list the new tab %s", c.TabID)
		}
		t.Title = Label
		t.SetStage(task.Tool, "", now)
		t.Assistant = &task.Assistant{SessionID: slack.NewSessionID()}
		return nil
	})
	if err != nil && !saved(st, c.TabID) {
		_ = h.CloseTab(c.TabID)
		return "", err
	}
	// Saved, the tab is the assistant's: what went wrong was elsewhere, as
	// a strip that couldn't be added to another tab.
	return c.TabID, nil
}

// saved reports whether the state has tab as the assistant's.
func saved(st *store.Store, tab string) bool {
	s, err := st.Load()
	if err != nil {
		return false
	}
	t, ok := s.Tasks[tab]
	return ok && t.Assistant != nil
}

// sessionID is what a Claude session ID looks like.
var sessionID = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)

// KeepSession keeps id as the session the assistant resumes: Claude's hook
// says which it runs, and /clear starts another. Anything that isn't a
// session ID is left out, as it goes on the command line.
func KeepSession(st *store.Store, live *herdr.Live, id string) error {
	if !sessionID.MatchString(id) {
		return nil
	}
	s, err := st.Load()
	if err != nil {
		return err
	}
	if a := Find(s, live); a == "" || s.Tasks[a].Assistant.SessionID == id {
		return nil
	}
	return st.Update(func(s *store.State) error {
		if a := Find(s, live); a != "" {
			s.Tasks[a].Assistant.SessionID = id
		}
		return nil
	})
}

// start writes the guide and the settings, and gives claude's arguments for
// session id: a resume once it has a log, which it has after its first
// message, or else a new session with that ID.
func start(cfg Config, id string) ([]string, error) {
	dir := filepath.Join(cfg.Dir, "assistant")
	guide, settings := filepath.Join(dir, "guide.md"), filepath.Join(dir, "settings.json")
	if err := slack.WritePrivate(guide, []byte(Guide)); err != nil {
		return nil, err
	}
	if err := slack.WritePrivate(settings, Settings(cfg.Exe)); err != nil {
		return nil, err
	}
	log, err := claudelog.Path(cfg.ClaudeDir, id)
	if err != nil {
		return nil, err
	}
	return Args(id, log != "", guide, settings), nil
}

// Args are claude's arguments for the assistant's session id, which it
// resumes when resume is set. Claude asks before anything its settings
// don't allow, whatever your default mode is. --system-prompt-snapshot off
// gives a resumed session the guide as it is now.
func Args(id string, resume bool, guide, settings string) []string {
	session := "--session-id"
	if resume {
		session = "--resume"
	}
	return []string{
		session, id,
		"--name", Label,
		"--append-system-prompt-file", guide,
		"--settings", settings,
		"--permission-mode", "manual",
		"--system-prompt-snapshot", "off",
	}
}

// Ask opens the assistant and types a line about tab's task into it, for
// you to finish: "About Login loop (w1:t2): ". It types nothing while
// Claude asks you something there, as typing could answer it.
func Ask(h Herdr, st *store.Store, opt reconcile.Options, cfg Config, tab string, now time.Time) (Opened, error) {
	live, err := h.Live()
	if err != nil {
		return Opened{}, err
	}
	s, err := st.Load()
	if err != nil {
		return Opened{}, err
	}
	t, ok := s.Tasks[tab]
	if _, there := live.Tab(tab); !ok || !there {
		return Opened{}, fmt.Errorf("%w %s", reconcile.ErrNoTab, tab)
	}
	if t.Assistant != nil {
		return Opened{}, ErrOwnTab
	}
	line := Prompt(t.Name(live.Position(tab)), tab)
	o, err := Open(h, st, opt, cfg, now)
	if err != nil {
		return o, err
	}
	panes, err := h.Panes()
	if err != nil {
		return o, err
	}
	if p, ok := Pane(o.Tab, panes); ok && p.AgentStatus == "blocked" {
		return o, ErrAsking
	}
	return o, h.TypeText(o.Pane, line)
}

// Prompt is the line Ask types about a task.
func Prompt(name, tab string) string {
	return "About " + task.CleanLabel(name) + " (" + task.CleanLabel(tab) + "): "
}

// Toggle shows the assistant's tab, opening it first, and keeps from as the
// tab to go back to. From the assistant's own tab, it shows that one again.
// It returns the tab it showed.
func Toggle(h Herdr, st *store.Store, opt reconcile.Options, cfg Config, from string, now time.Time) (string, error) {
	live, err := h.Live()
	if err != nil {
		return "", err
	}
	s, err := st.Load()
	if err != nil {
		return "", err
	}
	if a := Find(s, live); a != "" && a == from {
		back := s.Tasks[a].Assistant.Back
		if _, ok := live.Tab(back); !ok || back == a {
			return "", ErrNoBack
		}
		return back, h.FocusTab(back)
	}
	o, err := Open(h, st, opt, cfg, now)
	if o.Tab == "" {
		return "", err
	}
	// Shown even when Claude didn't start, so you see why.
	return o.Tab, errors.Join(err, keepBack(st, o.Tab, from), h.FocusTab(o.Tab))
}

// Show is Ask about tab, then the assistant's tab, which goes back to from,
// the tab you were in. It's shown even when nothing was typed, so you see
// why: Claude didn't start, or asks you something.
func Show(h Herdr, st *store.Store, opt reconcile.Options, cfg Config, tab, from string, now time.Time) error {
	o, err := Ask(h, st, opt, cfg, tab, now)
	if o.Tab == "" {
		return err
	}
	return errors.Join(err, keepBack(st, o.Tab, from), h.FocusTab(o.Tab))
}

// keepBack keeps from as the tab the assistant's tab goes back to.
func keepBack(st *store.Store, assistant, from string) error {
	if from == "" || from == assistant {
		return nil
	}
	return st.Update(func(s *store.State) error {
		if t, ok := s.Tasks[assistant]; ok && t.Assistant != nil {
			t.Assistant.Back = from
		}
		return nil
	})
}

// ConfigFor is how the assistant starts, with the plugin's settings and
// state folder: Claude's hook runs this binary, and Claude starts in
// assistant_cwd. A bad assistant_cwd stops only a new tab, so you can
// still go back from the assistant.
func ConfigFor(opts reconcile.Options, stateDir string) (Config, error) {
	exe, err := os.Executable()
	if err == nil {
		exe, err = filepath.EvalSymlinks(exe)
	}
	if err != nil {
		return Config{}, err
	}
	home, _ := os.UserHomeDir()
	return Config{Exe: exe, Dir: stateDir, Cwd: opts.AssistantCwd, Home: home, ClaudeDir: claudelog.ConfigDir()}, nil
}

// Cwd is where the assistant starts, from assistant_cwd: own, its own
// folder, for "", so Claude's own tools read nothing else without asking;
// the home folder for ~, a folder in it for ~/…, or an absolute path.
func Cwd(setting, home, own string) (string, error) {
	switch {
	case setting == "":
		return own, nil
	case setting == "~":
		return home, nil
	case strings.HasPrefix(setting, "~/"):
		return filepath.Join(home, setting[2:]), nil
	case !filepath.IsAbs(setting):
		return "", fmt.Errorf("assistant_cwd must be an absolute path or start with ~/, got %q", setting)
	}
	return setting, nil
}

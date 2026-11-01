// Command herdr-tasks gives herdr tabs a task stage, a note, and links, and
// shows every task in an overview popup.
package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/alekkowalczyk/herdr-tasks/internal/board"
	"github.com/alekkowalczyk/herdr-tasks/internal/claudehook"
	"github.com/alekkowalczyk/herdr-tasks/internal/herdr"
	"github.com/alekkowalczyk/herdr-tasks/internal/linear"
	"github.com/alekkowalczyk/herdr-tasks/internal/move"
	"github.com/alekkowalczyk/herdr-tasks/internal/reconcile"
	"github.com/alekkowalczyk/herdr-tasks/internal/setup"
	"github.com/alekkowalczyk/herdr-tasks/internal/slack"
	"github.com/alekkowalczyk/herdr-tasks/internal/store"
	"github.com/alekkowalczyk/herdr-tasks/internal/strip"
	"github.com/alekkowalczyk/herdr-tasks/internal/tailnet"
	"github.com/alekkowalczyk/herdr-tasks/internal/task"
	"github.com/alekkowalczyk/herdr-tasks/internal/ui"
)

const version = "0.11.0"

const usage = `herdr-tasks: task stages, notes, and links for herdr tabs

Inside a herdr pane, commands apply to that pane's tab. Use --tab ID for another one.

  herdr-tasks set <stage> [reason]   stage: active, review, paused, parked, done, tool
                                     (review and paused take what they wait for)
  herdr-tasks review [waiting for]   your part is done; waiting on approval, merge, or tests
  herdr-tasks pause [reason]         on hold until something happens
  herdr-tasks tool                   not a task: a tab for a tool or a running app
  herdr-tasks priority [on|off]      a red ! in the sidebar, which lists it even
                                     when paused or parked (done ends it)
  herdr-tasks note set <text>                             replace your note
  herdr-tasks note add <text> [--as NAME]                 add a line (agents use this)
  herdr-tasks note clear
  herdr-tasks link add <url> [--label TEXT] [--as NAME]   attach a URL or a local file
  herdr-tasks link rm <number|url|issue ID>
  herdr-tasks show [--json]                               this tab's task
  herdr-tasks list [--json]                               every task
  herdr-tasks move <workspace>                            move this task, with its panes, to another workspace
  herdr-tasks rename <name>                               name the task (empty: tab number)

  herdr-tasks setup [--no-config]   put herdr-tasks on PATH, add herdr config, sync
                                    (herdr plugin install runs this for you)
  herdr-tasks remove [--purge]      undo setup, give tabs their names back, and
                                    uninstall the plugin (--purge also deletes notes
                                    and the Linear key)

  herdr-tasks board [--session NAME] [--print]
                                    open a session's task board in your browser
                                    (board = true; this session's, or the list outside
                                    herdr); --print prints its address, and the
                                    tailnet one when shared, instead
  herdr-tasks board --new-token     sign every browser out of the board
  herdr-tasks linear-key [--delete] save a Linear API key, for issue status on the
                                    board (read from stdin, or typed hidden)

  herdr-tasks overview      the overview screen (herdr opens it in a popup)
  herdr-tasks strip         the strip along the bottom of a tab (runs in its pane)
  herdr-tasks strip-toggle  show or hide this tab's strip
  herdr-tasks sidebar-toggle  switch the sidebar between each space's tasks and stage counts
  herdr-tasks open          open the overview popup
  herdr-tasks status        one line for herdr's tab bar
  herdr-tasks sync          redo markers, tab order, and sidebar counts
  herdr-tasks hook          run by herdr on startup and tab events
  herdr-tasks serve         the task board's server (board and the startup hook
                            start it)
  herdr-tasks claude-hook   run by Claude Code on prompts, replies, and tool
                            calls, to link the Linear issues, Slack links, and
                            pages they name to the task (claude_links = true)
  herdr-tasks slack-guard   run by Claude Code before each tool call in a Slack
                            check, topic check, or research session, to keep
                            it to reading and answering
  herdr-tasks slack status          the Slack watcher: its settings, last check, and any
                                    problem (slack_watch = true)
  herdr-tasks slack fetch           check Slack for asks now, even with the watcher off
  herdr-tasks slack skipped         the asks that made no task: off-topic, or maybe
                                    answered already
  herdr-tasks slack take <n>        make the task for a skipped ask, by its number
                                    or its link
  herdr-tasks slack mutes           the channels it ignores
  herdr-tasks slack unmute <channel>  stop ignoring a channel you muted from a card
  herdr-tasks slack research        start research for this tab's Slack ask
  herdr-tasks slack continue        end its research session and resume it with your
                                    own settings and tools
  herdr-tasks config        print the herdr config lines setup adds
  herdr-tasks config --merge     read a herdr config on stdin, print it with the
                                 herdr-tasks lines added (for a config on another
                                 machine, such as the one herdr --remote uses)
  herdr-tasks config --unmerge   the same, with the herdr-tasks lines taken out
  herdr-tasks version
`

func main() {
	if err := run(os.Args[1:]); err != nil {
		if !errors.Is(err, errShown) {
			fmt.Fprintln(os.Stderr, "herdr-tasks:", err)
		}
		code := 1
		var e exitError
		if errors.As(err, &e) {
			code = e.code
		}
		os.Exit(code)
	}
}

// exitError makes main exit with code instead of 1.
type exitError struct {
	code int
	err  error
}

func (e exitError) Error() string { return e.err.Error() }
func (e exitError) Unwrap() error { return e.err }

type app struct {
	client *herdr.Client
	store  *store.Store
	opts   reconcile.Options
	flags  flags
}

type flags struct {
	tab, as, label string
	json           bool
	args           []string
}

func parseFlags(args []string) (flags, error) {
	var f flags
	for i := 0; i < len(args); i++ {
		a := args[i]
		name, val, hasVal := strings.Cut(a, "=")
		take := func() (string, error) {
			if hasVal {
				return val, nil
			}
			if i+1 >= len(args) {
				return "", fmt.Errorf("%s needs a value", name)
			}
			i++
			return args[i], nil
		}
		var err error
		switch name {
		case "--tab":
			f.tab, err = take()
		case "--as":
			f.as, err = take()
		case "--label":
			f.label, err = take()
		case "--json":
			f.json = true
		default:
			f.args = append(f.args, a)
		}
		if err != nil {
			return f, err
		}
	}
	return f, nil
}

func (f flags) text() string { return strings.TrimSpace(strings.Join(f.args, " ")) }

func newApp(args []string) (*app, error) {
	f, err := parseFlags(args)
	if err != nil {
		return nil, err
	}
	opts, err := reconcile.LoadOptions()
	if err != nil {
		return nil, fmt.Errorf("reading config: %w", err)
	}
	c := herdr.New()
	return &app{client: c, store: store.New(herdr.SessionKey(c.SocketPath)), opts: opts, flags: f}, nil
}

func run(args []string) error {
	if len(args) == 0 {
		fmt.Print(usage)
		return nil
	}
	cmd, rest := args[0], args[1:]
	switch cmd {
	case "help", "-h", "--help":
		fmt.Print(usage)
		return nil
	case "version", "--version":
		fmt.Println("herdr-tasks", version)
		return nil
	case "config":
		return configCmd(rest)
	case "claude-hook":
		return runClaudeHook()
	case "slack-guard":
		// Claude Code blocks a tool call when its PreToolUse hook exits 2.
		if err := slack.Guard(rest, os.Stdin, os.Stdout); err != nil {
			return exitError{code: 2, err: err}
		}
		return nil
	case "linear-key":
		return linearKey(rest, os.Stdin, os.Stdout, linear.SaveKey, linear.DeleteKey, store.Dir())
	}
	a, err := newApp(rest)
	if err != nil {
		return err
	}
	switch cmd {
	case "overview":
		return a.overview()
	case "open":
		return a.client.OpenPluginPane(pluginID(), "overview")
	case "board":
		return a.openBoard(rest)
	case "serve":
		return a.serve()
	case "status":
		return a.status()
	case "sync":
		_, err := reconcile.Run(a.host(), a.store, a.opts, time.Now(), nil)
		return err
	case "hook":
		return a.hook()
	case "setup":
		return a.setup(slices.Contains(rest, "--no-config"))
	case "remove", "uninstall":
		return a.remove(slices.Contains(rest, "--purge"))
	case "set", "stage":
		return a.set()
	case "pause":
		a.flags.args = append([]string{"paused"}, a.flags.args...)
		return a.set()
	case "review":
		a.flags.args = append([]string{"review"}, a.flags.args...)
		return a.set()
	case "tool":
		a.flags.args = append([]string{"tool"}, a.flags.args...)
		return a.set()
	case "priority":
		return a.priority()
	case "strip":
		return ui.RunStrip(a.client, a.store, a.opts)
	case "strip-toggle":
		return a.stripToggle()
	case "sidebar-toggle":
		return a.sidebarToggle()
	case "strip-fit": // resize a strip pane; for troubleshooting
		return a.host().Fit(a.flags.text())
	case "note":
		return a.note()
	case "link":
		return a.link()
	case "move":
		return a.move()
	case "slack":
		return a.slack()
	case "rename":
		title := task.CleanLabel(a.flags.text())
		return a.change(func(t *task.Task) error { t.Title = title; return nil })
	case "show":
		return a.show()
	case "list", "ls":
		return a.list()
	}
	return fmt.Errorf("unknown command %q (see herdr-tasks help)", cmd)
}

// targetTab is the tab a command applies to: --tab, else the tab of the pane
// it runs in, else herdr's focused tab.
func (a *app) targetTab() (string, error) {
	if a.flags.tab != "" {
		return a.flags.tab, nil
	}
	if p := os.Getenv("HERDR_PANE_ID"); p != "" {
		// Ask herdr, since the pane may have moved since it started.
		if id, err := paneTab(a.client, a.store, p); err == nil {
			return id, nil
		}
	}
	for _, env := range []string{"HERDR_TAB_ID", "HERDR_ACTIVE_TAB_ID"} {
		if id := os.Getenv(env); id != "" {
			return id, nil
		}
	}
	live, err := a.client.Live()
	if err != nil {
		return "", err
	}
	if t, ok := live.FocusedTab(); ok {
		return t.ID, nil
	}
	return "", errors.New("no tab to apply this to; pass --tab ID")
}

// maxAliasHops is how many of a move's pane aliases paneTab follows.
const maxAliasHops = 5

// paneTab is the tab pane is in now. A process knows its pane by the ID it
// had when it started, and a move gives a pane a new one: when herdr doesn't
// know pane, the IDs moves gave it, kept in st, lead to the pane it is now.
func paneTab(c *herdr.Client, st *store.Store, pane string) (string, error) {
	tab, err := c.PaneTab(pane)
	if err == nil && tab != "" {
		return tab, nil
	}
	if err == nil {
		err = fmt.Errorf("no tab for pane %s", pane)
	}
	s, lerr := st.Load()
	if lerr != nil {
		return "", err
	}
	p := pane
	for range maxAliasHops {
		next, ok := s.PaneAliases[p]
		if !ok {
			break
		}
		p = next
		if tab, aerr := c.PaneTab(p); aerr == nil && tab != "" {
			return tab, nil
		}
	}
	return "", err
}

// change edits the target tab's task and syncs herdr.
func (a *app) change(fn func(t *task.Task) error) error {
	id, err := a.targetTab()
	if err != nil {
		return err
	}
	_, err = reconcile.ChangeTask(a.host(), a.store, a.opts, time.Now(), id, fn)
	return err
}

// who names the author of an agent's note or link.
func (a *app) who() string {
	if as := task.CleanLabel(a.flags.as); as != "" {
		return as
	}
	if os.Getenv("CLAUDECODE") == "1" {
		return "claude"
	}
	if os.Getenv("CODEX_SANDBOX") != "" || os.Getenv("CODEX_HOME") != "" {
		return "codex"
	}
	return "you"
}

func (a *app) set() error {
	if len(a.flags.args) == 0 {
		return errors.New("which stage? active, review, paused, parked, done, or tool")
	}
	s, ok := task.ParseStage(a.flags.args[0])
	if !ok {
		return fmt.Errorf("unknown stage %q; use active, review, paused, parked, done, or tool", a.flags.args[0])
	}
	reason := task.CleanLabel(strings.Join(a.flags.args[1:], " "))
	if reason != "" && !s.TakesReason() {
		return errors.New("only review and paused take a reason")
	}
	return a.change(func(t *task.Task) error {
		t.SetStage(s, reason, time.Now())
		return nil
	})
}

func (a *app) priority() error {
	on := true
	switch strings.Join(a.flags.args, " ") {
	case "", "on":
	case "off":
		on = false
	default:
		return errors.New("priority on|off")
	}
	return a.change(func(t *task.Task) error { return t.SetPriority(on) })
}

func (a *app) note() error {
	if len(a.flags.args) == 0 {
		return errors.New("note set|add|clear")
	}
	sub := a.flags.args[0]
	a.flags.args = a.flags.args[1:]
	// Agents write notes, so a control character can't get into one.
	text := strings.TrimSpace(task.CleanText(a.flags.text()))
	switch sub {
	case "set":
		return a.change(func(t *task.Task) error { t.Note = text; return nil })
	case "add":
		if text == "" {
			return errors.New("note add needs some text")
		}
		return a.change(func(t *task.Task) error {
			t.Log = append(t.Log, task.Entry{Who: a.who(), At: time.Now(), Text: text})
			return nil
		})
	case "clear":
		return a.change(func(t *task.Task) error { t.Note, t.Log = "", nil; return nil })
	}
	return fmt.Errorf("unknown note command %q; use set, add, or clear", sub)
}

func (a *app) link() error {
	if len(a.flags.args) < 2 {
		return errors.New("link add <url> | link rm <number|url|issue ID>")
	}
	sub, target := a.flags.args[0], a.flags.args[1]
	switch sub {
	case "add":
		by := a.who()
		if by == "you" {
			by = ""
		}
		cwd, _ := os.Getwd()
		return linkAdd(a.change, os.Stderr, target, cwd, a.flags.label, by)
	case "rm", "remove":
		return a.change(func(t *task.Task) error {
			_, err := t.RemoveLink(target)
			return err
		})
	}
	return fmt.Errorf("unknown link command %q; use add or rm", sub)
}

// linkAdd adds a link to the task through change, as by ("" for you). target
// is a URL, or the path of a file, relative to cwd. A URL Task.Add would
// refuse, with a control character or one that doesn't parse, is an error.
// An agent can't add back a link you removed. That isn't an error, as for the
// Claude links hook, but linkAdd says so on w, since nothing else would.
func linkAdd(change func(func(*task.Task) error) error, w io.Writer, target, cwd, label, by string) error {
	target = task.LinkInput(target, cwd)
	if !task.SafeURL(target) {
		// Quoted, so a control character in it isn't printed as it is.
		return fmt.Errorf("%q can't be saved as a link: it has a control character, or isn't a valid URL", target)
	}
	dismissed := false
	err := change(func(t *task.Task) error {
		if _, added := t.AddLink(target, label, by, time.Now()); !added && t.Dismissed(target, "") {
			dismissed = true
		}
		return nil
	})
	if err == nil && dismissed {
		fmt.Fprintf(w, "herdr-tasks: %s was removed from this task, so it wasn't added back; to restore it, run `herdr-tasks link add %s` yourself (outside an agent, without --as)\n", task.LinkLabel(target), target)
	}
	return err
}

func (a *app) show() error {
	id, err := a.targetTab()
	if err != nil {
		return err
	}
	live, err := reconcile.Run(a.host(), a.store, a.opts, time.Now(), nil)
	if err != nil {
		return err
	}
	st, err := a.store.Load()
	if err != nil {
		return err
	}
	t, ok := st.Tasks[id]
	if !ok {
		return fmt.Errorf("no tab %s", id)
	}
	if a.flags.json {
		return printJSON(t)
	}
	tab, _ := live.Tab(id)
	printTask(os.Stdout, t, id, tab.AgentStatus, live.Position(id), time.Now())
	return nil
}

// printTask prints a task as show does. Agents write much of it, and the
// state file can be edited by hand, so everything is cleaned of control
// characters before it reaches the terminal, and a link whose URL isn't
// task.SafeURL is shown as "unsafe link".
func printTask(w io.Writer, t *task.Task, id, agentStatus string, pos int, now time.Time) {
	fmt.Fprintf(w, "%s  (%s · %s)\n", t.Name(pos), task.CleanLabel(t.Workspace), id)
	if t.Priority {
		fmt.Fprint(w, "! ")
	}
	fmt.Fprintf(w, "%s %s · %s", t.Stage.Glyph(), t.Stage.Label(), task.Age(now.Sub(t.Since)))
	if g := task.GroupOf(t.Stage, agentStatus); g == task.YourTurn {
		fmt.Fprint(w, " · your turn")
	} else if g == task.Running {
		fmt.Fprint(w, " · agent running")
	} else if g == task.Asking {
		fmt.Fprint(w, " · agent asking")
	}
	if reason := task.CleanLabel(t.Reason); reason != "" {
		label := "until"
		if t.Stage == task.Review {
			label = "waiting for"
		}
		fmt.Fprintf(w, " · %s %s", label, reason)
	}
	fmt.Fprintln(w)
	if note := task.CleanText(t.Note); note != "" {
		fmt.Fprintln(w, "\nNote:")
		for _, l := range strings.Split(note, "\n") {
			fmt.Fprintln(w, "  "+l)
		}
	}
	if len(t.Log) > 0 {
		fmt.Fprintln(w, "\nAdded:")
		for _, e := range t.Log {
			fmt.Fprintf(w, "  %s %s  %s\n", task.CleanLabel(e.Who), e.At.Local().Format("Jan 2 15:04"), task.CleanText(e.Text))
		}
	}
	if len(t.Links) > 0 {
		// Numbered as `link rm` takes them.
		fmt.Fprintln(w, "\nLinks:")
		for i, l := range task.Grouped(t.Links) {
			line := task.CleanLabel(l.Display()) + "  " + l.URL
			if !task.SafeURL(l.URL) {
				// Not the URL itself: a terminal could still find a link in it.
				line = "unsafe link"
				if label := task.CleanLabel(l.Label); label != "" {
					line = label + "  " + line
				}
			}
			by := ""
			if b := task.CleanLabel(l.By); b != "" {
				by = "  (via " + b + ")"
			}
			fmt.Fprintf(w, "  %d. %s%s\n", i+1, line, by)
		}
	}
}

func (a *app) list() error {
	live, err := reconcile.Run(a.host(), a.store, a.opts, time.Now(), nil)
	if err != nil {
		return err
	}
	st, err := a.store.Load()
	if err != nil {
		return err
	}
	if a.flags.json {
		var out []*task.Task
		for _, tab := range live.Tabs {
			if t, ok := st.Tasks[tab.ID]; ok {
				out = append(out, t)
			}
		}
		return printJSON(out)
	}
	for _, tab := range live.Tabs {
		t, ok := st.Tasks[tab.ID]
		if !ok {
			continue
		}
		info := ""
		if reason := task.CleanLabel(t.Reason); reason != "" {
			info = "until " + reason
			if t.Stage == task.Review {
				info = "waiting for " + reason
			}
		} else if len(t.Links) > 0 {
			info = linkPreview(t)
		}
		pri := " "
		if t.Priority {
			pri = "!"
		}
		fmt.Printf("%s %s %-7s %-6s %-14s %-32s %s\n", pri, t.Stage.Glyph(), task.CleanLabel(string(t.Stage)), tab.ID, task.CleanLabel(t.Workspace), t.Name(live.Position(tab.ID)), info)
	}
	return nil
}

// linkPreview is "↗" and t's first link, cleaned, in the order show and
// the strip list them. t has links.
func linkPreview(t *task.Task) string {
	return "↗ " + task.CleanLabel(task.Grouped(t.Links)[0].Display())
}

// status prints one line for herdr's tab bar: the current space's stage
// counts, then the current task's pause reason or first link. It only reads
// the state file, since herdr runs it every few seconds.
func (a *app) status() error {
	line, err := a.statusLine()
	if err != nil {
		return err
	}
	fmt.Println(line)
	return nil
}

func (a *app) statusLine() (string, error) {
	st, err := a.store.Load()
	if err != nil {
		return "", err
	}
	ws := os.Getenv("HERDR_ACTIVE_WORKSPACE_ID")
	tab := os.Getenv("HERDR_ACTIVE_TAB_ID")
	if tab == "" {
		tab = os.Getenv("HERDR_TAB_ID")
	}
	if ws == "" {
		if i := strings.IndexByte(tab, ':'); i > 0 {
			ws = tab[:i]
		}
	}
	counts := map[task.Stage]int{}
	for _, t := range st.Tasks {
		if t.WorkspaceID() == ws {
			counts[t.Stage]++
		}
	}
	var parts []string
	for _, s := range []task.Stage{task.Active, task.Review, task.Paused, task.Parked} {
		if counts[s] > 0 {
			parts = append(parts, fmt.Sprintf("%s %d", s.Glyph(), counts[s]))
		}
	}
	line := strings.Join(parts, " ")
	if t, ok := st.Tasks[tab]; ok {
		// herdr shows this in its tab bar, so it's cleaned as the strip is.
		extra := ""
		switch reason := task.CleanLabel(t.Reason); {
		case t.Stage.TakesReason() && reason != "":
			extra = t.Stage.Glyph() + " " + reason
		case len(t.Links) > 0:
			extra = linkPreview(t)
		}
		if extra != "" {
			line += "  " + extra
		}
	}
	if slackTrouble(a.opts, a.store.Session, a.store.Dir) {
		line += "  Slack ⚠"
	}
	return line, nil
}

// hook runs on herdr startup and tab or space events.
func (a *app) hook() error {
	ev := reconcile.EventFromEnv()
	// Our own renames come back as events; skip them without touching herdr.
	if ev != nil && ev.Name == "tab.renamed" && ev.TabID != "" {
		if st, err := a.store.Load(); err == nil {
			if t, ok := st.Tasks[ev.TabID]; ok && t.Applied == ev.Label {
				return nil
			}
		}
	}
	if ev != nil && ev.Name == "pane.agent_status_changed" {
		// Only the sidebar list depends on what agents are doing.
		return reconcile.RefreshTokens(a.host(), a.store)
	}
	if ev != nil && ev.Name == "startup" {
		a.opts.Startup = true
		if a.opts.Board || a.opts.SlackWatch {
			a.startBoard()
		}
	}
	_, err := reconcile.Run(a.host(), a.store, a.opts, time.Now(), nil)
	return err
}

func (a *app) overview() error {
	current := os.Getenv("HERDR_TAB_ID")
	var ctx struct {
		TabID string `json:"tab_id"`
	}
	if json.Unmarshal([]byte(os.Getenv("HERDR_PLUGIN_CONTEXT_JSON")), &ctx) == nil && ctx.TabID != "" {
		current = ctx.TabID
	}
	return ui.Run(ui.Env{Client: a.client, Store: a.store, Opts: a.opts, CurrentTab: current, Message: os.Getenv("HERDR_TASKS_MESSAGE")})
}

func printJSON(v any) error {
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	return enc.Encode(v)
}

// setup does what a plugin manifest can't: puts herdr-tasks on PATH, adds the
// key binding, tab bar status, and sidebar rows to herdr's config, and syncs
// every running herdr session. herdr's plugin install runs it after building.
func (a *app) setup(noConfig bool) error {
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	if exe, err = filepath.EvalSymlinks(exe); err != nil {
		return err
	}
	command, notes, err := setup.InstallBinary(exe, store.Dir())
	if err != nil {
		return fmt.Errorf("installing herdr-tasks into %s: %w", setup.BinDir(), err)
	}
	if command == setup.PathCommand {
		fmt.Printf("herdr-tasks: installed %s/herdr-tasks\n", setup.BinDir())
	}

	if !noConfig {
		path := setup.ConfigPath()
		before, err := setup.ReadConfig(path)
		if err != nil {
			return err
		}
		after, more, err := setup.Apply(before, command)
		notes = append(notes, more...)
		switch {
		case err != nil:
			notes = append(notes, err.Error()+". Run `herdr-tasks config` to see the lines to add yourself.")
		case after == before:
			fmt.Printf("herdr-tasks: %s already has the herdr-tasks lines\n", path)
		default:
			backup, err := setup.WriteConfig(path, before, after)
			if err != nil {
				return fmt.Errorf("updating %s: %w", path, err)
			}
			fmt.Printf("herdr-tasks: added ctrl+b t, the tab bar status, and sidebar counts to %s\n", path)
			if backup != "" {
				fmt.Printf("herdr-tasks: your previous config is saved as %s\n", backup)
			}
		}
		note, err := claudeLinks(a.opts.ClaudeLinks, command)
		if err != nil {
			note = err.Error()
		}
		if note != "" {
			notes = append(notes, note)
		}
	}

	sessions := herdr.RunningSessions()
	for _, c := range sessions {
		name := herdr.SessionKey(c.SocketPath)
		st := store.New(name)
		if err := reconcile.Enable(st); err != nil {
			return err
		}
		// Sync before reloading, so the tab bar status has tasks to show
		// the first time herdr runs it.
		live, err := reconcile.Run(strip.Host{Client: c}, st, a.opts, time.Now(), nil)
		if err != nil {
			notes = append(notes, fmt.Sprintf("Couldn't sync session %s: %v", name, err))
		} else {
			fmt.Printf("herdr-tasks: session %s: %d tabs in %d spaces are tasks now\n", name, len(live.Tabs), len(live.Workspaces))
		}
		if err := c.ReloadConfig(); err != nil {
			notes = append(notes, fmt.Sprintf("Couldn't reload config in session %s: %v", name, err))
		}
	}
	if len(sessions) == 0 {
		fmt.Println("herdr-tasks: no herdr session is running; tabs get their markers when herdr starts")
	}
	for _, n := range notes {
		fmt.Println("herdr-tasks: note: " + n)
	}
	fmt.Println("herdr-tasks: ready. Press ctrl+b t in herdr for the task overview.")
	return nil
}

// remove undoes setup in every running session, then uninstalls the plugin.
// Notes and links stay on disk unless purge is set.
//
// It stops the board and takes the Claude links hook out before it changes
// anything else. When either fails, it stops there, with the board's token
// and this command still in place, so you can fix the cause and run remove
// again.
func (a *app) remove(purge bool) error {
	// First, while the board's token is still there to stop it with.
	if err := board.Stop(store.Dir()); err != nil {
		return fmt.Errorf("couldn't stop the task board, so nothing was removed; stop it (kill its pid) and run remove again: %w", err)
	}
	// Settings that don't parse only get a note: Claude Code can't run a hook
	// from them either.
	note, err := claudeLinks(false, "")
	if err != nil {
		return fmt.Errorf("couldn't take the Claude links hook out, so nothing else was removed: %w", err)
	}
	if note != "" {
		fmt.Println("herdr-tasks: note: " + note)
	}
	// Only the tailscale serve entries the board recorded, and only those
	// still as recorded. The ones left are named, as --purge deletes the
	// record of them.
	if err := board.Unshare(store.Dir(), tailscaleCLI); err != nil {
		note := "couldn't take the board off your tailnet: " + strings.ReplaceAll(err.Error(), "\n", "; ")
		if ports := board.RecordedPorts(store.Dir()); len(ports) > 0 {
			var offs []string
			for _, p := range ports {
				offs = append(offs, fmt.Sprintf("tailscale serve --https=%d off", p))
			}
			note += ". Check tailscale serve status, and take out the entries herdr-tasks made with: " + strings.Join(offs, "; ")
		}
		fmt.Println("herdr-tasks: note: " + note)
	}
	sessions := herdr.RunningSessions()
	// Stop hooks first, so they don't put markers back.
	for _, c := range sessions {
		_ = c.DisablePlugin(store.PluginID)
	}
	for _, c := range sessions {
		name := herdr.SessionKey(c.SocketPath)
		if err := reconcile.Clear(strip.Host{Client: c}, store.New(name)); err != nil {
			return fmt.Errorf("session %s: %w", name, err)
		}
		fmt.Printf("herdr-tasks: session %s: tab names are back to normal\n", name)
	}

	path := setup.ConfigPath()
	before, err := setup.ReadConfig(path)
	if err != nil {
		return err
	}
	if after := setup.RemoveBlocks(before); after != before {
		if err := os.WriteFile(path, []byte(after), 0o644); err != nil {
			return fmt.Errorf("updating %s: %w", path, err)
		}
		fmt.Printf("herdr-tasks: took the herdr-tasks lines out of %s\n", path)
	}
	for _, c := range sessions {
		_ = c.ReloadConfig()
	}

	herdrBin := os.Getenv("HERDR_BIN_PATH")
	if herdrBin == "" {
		herdrBin = "herdr"
	}
	if out, err := exec.Command(herdrBin, "plugin", "uninstall", store.PluginID, "--yes").CombinedOutput(); err != nil {
		if out2, err2 := exec.Command(herdrBin, "plugin", "unlink", store.PluginID).CombinedOutput(); err2 != nil {
			fmt.Printf("herdr-tasks: note: couldn't unregister the plugin: %s %s\n", strings.TrimSpace(string(out)), strings.TrimSpace(string(out2)))
		}
	}
	fmt.Println("herdr-tasks: plugin uninstalled")
	if err := setup.RemoveBinary(store.Dir()); err != nil {
		fmt.Printf("herdr-tasks: note: couldn't remove %s/herdr-tasks: %v\n", setup.BinDir(), err)
	}
	if purge {
		if err := deleteLinearKey(); err != nil {
			fmt.Printf("herdr-tasks: note: couldn't delete the Linear key: %v\n", err)
		}
		if err := os.RemoveAll(store.Dir()); err != nil {
			return err
		}
		fmt.Println("herdr-tasks: deleted saved notes and links")
	} else {
		fmt.Printf("herdr-tasks: notes and links are kept in %s\n", store.Dir())
	}
	return nil
}

// writeSettings is claudehook.WriteSettings; tests replace it.
var writeSettings = claudehook.WriteSettings

// deleteLinearKey is linear.DeleteKey, which on macOS runs security against
// the login keychain; tests replace it.
var deleteLinearKey = linear.DeleteKey

// tailscaleCLI is the tailscale command remove runs; tests replace it.
var tailscaleCLI board.Tailscale = tailnet.CLI{}

// claudeLinks adds the Claude links hook to Claude Code's settings when on,
// running command, or takes it out, and says so. When the settings don't
// parse, it leaves them alone and returns a note saying so. It returns an
// error when it couldn't change them otherwise, as when they changed while
// it edited them.
func claudeLinks(on bool, command string) (note string, err error) {
	path := claudehook.SettingsPath()
	edit, did := claudehook.Remove, "took the Claude links hook out of"
	if on {
		edit = func(src []byte) ([]byte, error) { return claudehook.Apply(src, claudehook.Command(command)) }
		did = "added the Claude links hook to"
	}
	changed, err := writeSettings(path, store.Dir(), on, edit)
	var bad *claudehook.ParseError
	switch {
	case errors.As(err, &bad):
		return fmt.Sprintf("your Claude settings don't parse, so the Claude links hook wasn't changed (%s: %v)", path, bad), nil
	case err != nil:
		return "", err
	case changed:
		fmt.Printf("herdr-tasks: %s %s\n", did, path)
	}
	return "", nil
}

// configCmd prints herdr config lines. With --merge or --unmerge it edits a
// config read from stdin, so a herdr --remote client on another machine can
// get the same lines: its key bindings, sidebar layout, and tab bar come from
// its own config.
func configCmd(args []string) error {
	// The tab bar status command runs on the machine where herdr runs, so
	// it can always use this machine's path.
	const status = "$HOME/.local/bin/herdr-tasks"
	switch {
	case slices.Contains(args, "--merge"), slices.Contains(args, "--unmerge"):
		in, err := io.ReadAll(os.Stdin)
		if err != nil {
			return err
		}
		if slices.Contains(args, "--unmerge") {
			fmt.Print(setup.RemoveBlocks(string(in)))
			return nil
		}
		out, notes, err := setup.Apply(string(in), status)
		if err != nil {
			return err
		}
		for _, n := range notes {
			fmt.Fprintln(os.Stderr, "herdr-tasks: note: "+n)
		}
		fmt.Print(out)
		return nil
	}
	out, _, err := setup.Apply("", status)
	fmt.Print(out)
	return err
}

func (a *app) host() strip.Host { return strip.Host{Client: a.client} }

// stripToggle shows or hides the strip of the target tab.
func (a *app) stripToggle() error {
	id, err := a.targetTab()
	if err != nil {
		return err
	}
	host := a.host()
	_, err = reconcile.Run(host, a.store, a.opts, time.Now(), func(s *store.State, _ *herdr.Live) error {
		t, ok := s.Tasks[id]
		if !ok {
			return fmt.Errorf("no tab %s", id)
		}
		if t.StripPane != "" {
			if err := host.Close(t.StripPane); err != nil {
				return err
			}
			t.StripPane = ""
			t.SetStrip(false)
			return nil
		}
		t.SetStrip(true) // reconciliation adds it
		return nil
	})
	return err
}

// move moves the target tab's task, with its panes, to another workspace.
func (a *app) move() error {
	to := strings.TrimSpace(a.flags.text())
	if to == "" {
		return errors.New("move <workspace>")
	}
	id, err := a.targetTab()
	if err != nil {
		return err
	}
	return moveAndSay(os.Stdout, a.store, to, func() (string, error) {
		return move.Task(a.host(), a.store, a.opts, time.Now(), id, to)
	})
}

// moveAndSay runs a move and confirms it, even when only some panes moved.
// The workspace label comes from the moved task, else it's what you typed.
func moveAndSay(w io.Writer, st *store.Store, to string, do func() (string, error)) error {
	tab, err := do()
	if tab == "" {
		return err
	}
	if s, lerr := st.Load(); lerr == nil {
		if t, ok := s.Tasks[tab]; ok && t.Workspace != "" {
			to = t.Workspace
		}
	}
	fmt.Fprintln(w, "herdr-tasks: moved to "+to)
	return err
}

// sidebarToggle switches the sidebar between each space's tasks and stage
// counts, in every running herdr session.
func (a *app) sidebarToggle() error {
	s, err := a.store.Load()
	if err != nil {
		return err
	}
	counts := !s.SidebarCounts
	for _, c := range herdr.RunningSessions() {
		st := store.New(herdr.SessionKey(c.SocketPath))
		_, err := reconcile.Run(strip.Host{Client: c}, st, a.opts, time.Now(), func(s *store.State, _ *herdr.Live) error {
			s.SidebarCounts = counts
			return nil
		})
		if err != nil {
			fmt.Fprintf(os.Stderr, "herdr-tasks: session %s: %v\n", herdr.SessionKey(c.SocketPath), err)
		}
	}
	return nil
}

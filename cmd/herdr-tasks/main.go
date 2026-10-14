// Command herdr-tasks gives herdr tabs a task stage, a note, and links, and
// shows every task in an overview popup.
package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/alekkowalczyk/herdr-tasks/internal/herdr"
	"github.com/alekkowalczyk/herdr-tasks/internal/reconcile"
	"github.com/alekkowalczyk/herdr-tasks/internal/setup"
	"github.com/alekkowalczyk/herdr-tasks/internal/store"
	"github.com/alekkowalczyk/herdr-tasks/internal/task"
	"github.com/alekkowalczyk/herdr-tasks/internal/ui"
)

const version = "0.1.1"

const usage = `herdr-tasks: task stages, notes, and links for herdr tabs

Inside a herdr pane, commands apply to that pane's tab. Use --tab ID for another one.

  herdr-tasks set <active|paused|parked|done> [reason]   set the tab's stage
  herdr-tasks pause [reason]                              pause the tab, with why
  herdr-tasks note set <text>                             replace your note
  herdr-tasks note add <text> [--as NAME]                 add a line (agents use this)
  herdr-tasks note clear
  herdr-tasks link add <url> [--label TEXT] [--as NAME]   attach a link
  herdr-tasks link rm <number|url>
  herdr-tasks show [--json]                               this tab's task
  herdr-tasks list [--json]                               every task
  herdr-tasks rename <name>                               name the task (empty: tab number)

  herdr-tasks setup [--no-config]   put herdr-tasks on PATH, add herdr config, sync
                                    (herdr plugin install runs this for you)
  herdr-tasks remove [--purge]      undo setup, give tabs their names back, and
                                    uninstall the plugin (--purge also deletes notes)

  herdr-tasks overview      the overview screen (herdr opens it in a popup)
  herdr-tasks open          open the overview popup
  herdr-tasks status        one line for herdr's tab bar
  herdr-tasks sync          redo markers, tab order, and sidebar counts
  herdr-tasks hook          run by herdr on startup and tab events
  herdr-tasks config        print the herdr config lines setup adds
  herdr-tasks version
`

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "herdr-tasks:", err)
		os.Exit(1)
	}
}

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
		out, _, err := setup.Apply("", "herdr-tasks")
		fmt.Print(out)
		return err
	}
	a, err := newApp(rest)
	if err != nil {
		return err
	}
	switch cmd {
	case "overview":
		return a.overview()
	case "open":
		id := os.Getenv("HERDR_PLUGIN_ID")
		if id == "" {
			id = store.PluginID
		}
		return a.client.OpenPluginPane(id, "overview")
	case "status":
		return a.status()
	case "sync":
		_, err := reconcile.Run(a.client, a.store, a.opts, time.Now(), nil)
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
	case "note":
		return a.note()
	case "link":
		return a.link()
	case "rename":
		return a.change(func(t *task.Task) error { t.Title = a.flags.text(); return nil })
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
		if id, err := a.client.PaneTab(p); err == nil && id != "" {
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

// change edits the target tab's task and syncs herdr.
func (a *app) change(fn func(t *task.Task) error) error {
	id, err := a.targetTab()
	if err != nil {
		return err
	}
	_, err = reconcile.Run(a.client, a.store, a.opts, time.Now(), func(s *store.State, _ *herdr.Live) error {
		t, ok := s.Tasks[id]
		if !ok {
			return fmt.Errorf("no tab %s", id)
		}
		return fn(t)
	})
	return err
}

// who names the author of an agent's note or link.
func (a *app) who() string {
	if a.flags.as != "" {
		return a.flags.as
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
		return errors.New("which stage? active, paused, parked, or done")
	}
	s, ok := task.ParseStage(a.flags.args[0])
	if !ok {
		return fmt.Errorf("unknown stage %q; use active, paused, parked, or done", a.flags.args[0])
	}
	reason := strings.TrimSpace(strings.Join(a.flags.args[1:], " "))
	if reason != "" && s != task.Paused {
		return errors.New("only paused tasks take a reason")
	}
	return a.change(func(t *task.Task) error {
		t.SetStage(s, reason, time.Now())
		return nil
	})
}

func (a *app) note() error {
	if len(a.flags.args) == 0 {
		return errors.New("note set|add|clear")
	}
	sub := a.flags.args[0]
	a.flags.args = a.flags.args[1:]
	text := a.flags.text()
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
		return errors.New("link add <url> | link rm <number|url>")
	}
	sub, target := a.flags.args[0], a.flags.args[1]
	switch sub {
	case "add":
		if !strings.Contains(target, "://") {
			target = "https://" + target
		}
		by := a.who()
		if by == "you" {
			by = ""
		}
		return a.change(func(t *task.Task) error {
			t.AddLink(target, a.flags.label, by, time.Now())
			return nil
		})
	case "rm", "remove":
		return a.change(func(t *task.Task) error {
			_, err := t.RemoveLink(target)
			return err
		})
	}
	return fmt.Errorf("unknown link command %q; use add or rm", sub)
}

func (a *app) show() error {
	id, err := a.targetTab()
	if err != nil {
		return err
	}
	live, err := reconcile.Run(a.client, a.store, a.opts, time.Now(), nil)
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
	pos := live.Position(id)
	fmt.Printf("%s  (%s · %s)\n", t.Name(pos), t.Workspace, id)
	fmt.Printf("%s %s · %s", t.Stage.Glyph(), t.Stage.Label(), task.Age(time.Since(t.Since)))
	if g := task.GroupOf(t.Stage, tab.AgentStatus); g == task.YourTurn {
		fmt.Print(" · your turn")
	} else if g == task.Running {
		fmt.Print(" · agent running")
	}
	if t.Reason != "" {
		fmt.Printf(" · until %s", t.Reason)
	}
	fmt.Println()
	if t.Note != "" {
		fmt.Println("\nNote:")
		for _, l := range strings.Split(t.Note, "\n") {
			fmt.Println("  " + l)
		}
	}
	if len(t.Log) > 0 {
		fmt.Println("\nAdded:")
		for _, e := range t.Log {
			fmt.Printf("  %s %s  %s\n", e.Who, e.At.Local().Format("Jan 2 15:04"), e.Text)
		}
	}
	if len(t.Links) > 0 {
		fmt.Println("\nLinks:")
		for i, l := range t.Links {
			by := ""
			if l.By != "" {
				by = "  (via " + l.By + ")"
			}
			fmt.Printf("  %d. %s  %s%s\n", i+1, l.Display(), l.URL, by)
		}
	}
	return nil
}

func (a *app) list() error {
	live, err := reconcile.Run(a.client, a.store, a.opts, time.Now(), nil)
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
		if t.Reason != "" {
			info = "until " + t.Reason
		} else if len(t.Links) > 0 {
			info = "↗ " + t.Links[0].Display()
		}
		fmt.Printf("%s %-7s %-6s %-14s %-32s %s\n", t.Stage.Glyph(), t.Stage, tab.ID, t.Workspace, t.Name(live.Position(tab.ID)), info)
	}
	return nil
}

// status prints one line for herdr's tab bar: the current space's stage
// counts, then the current task's pause reason or first link. It only reads
// the state file, since herdr runs it every few seconds.
func (a *app) status() error {
	st, err := a.store.Load()
	if err != nil {
		return err
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
	for _, s := range []task.Stage{task.Active, task.Paused, task.Parked} {
		if counts[s] > 0 {
			parts = append(parts, fmt.Sprintf("%s %d", s.Glyph(), counts[s]))
		}
	}
	line := strings.Join(parts, " ")
	if t, ok := st.Tasks[tab]; ok {
		extra := ""
		switch {
		case t.Stage == task.Paused && t.Reason != "":
			extra = "‖ " + t.Reason
		case len(t.Links) > 0:
			extra = "↗ " + t.Links[0].Display()
		}
		if extra != "" {
			line += "  " + extra
		}
	}
	fmt.Println(line)
	return nil
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
	_, err := reconcile.Run(a.client, a.store, a.opts, time.Now(), nil)
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
	return ui.Run(ui.Env{Client: a.client, Store: a.store, Opts: a.opts, CurrentTab: current})
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
	if command != exe {
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
	}

	sessions := herdr.RunningSessions()
	for _, c := range sessions {
		name := herdr.SessionKey(c.SocketPath)
		if err := c.ReloadConfig(); err != nil {
			notes = append(notes, fmt.Sprintf("Couldn't reload config in session %s: %v", name, err))
		}
		st := store.New(name)
		if err := reconcile.Enable(st); err != nil {
			return err
		}
		live, err := reconcile.Run(c, st, a.opts, time.Now(), nil)
		if err != nil {
			notes = append(notes, fmt.Sprintf("Couldn't sync session %s: %v", name, err))
			continue
		}
		fmt.Printf("herdr-tasks: session %s: %d tabs in %d spaces are tasks now\n", name, len(live.Tabs), len(live.Workspaces))
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
func (a *app) remove(purge bool) error {
	sessions := herdr.RunningSessions()
	// Stop hooks first, so they don't put markers back.
	for _, c := range sessions {
		_ = c.DisablePlugin(store.PluginID)
	}
	for _, c := range sessions {
		name := herdr.SessionKey(c.SocketPath)
		if err := reconcile.Clear(c, store.New(name)); err != nil {
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
		if err := os.RemoveAll(store.Dir()); err != nil {
			return err
		}
		fmt.Println("herdr-tasks: deleted saved notes and links")
	} else {
		fmt.Printf("herdr-tasks: notes and links are kept in %s\n", store.Dir())
	}
	return nil
}

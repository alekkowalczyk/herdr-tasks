package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"slices"
	"strings"
	"syscall"
	"time"

	"github.com/alekkowalczyk/herdr-tasks/internal/board"
	"github.com/alekkowalczyk/herdr-tasks/internal/reconcile"
	"github.com/alekkowalczyk/herdr-tasks/internal/slack"
	"github.com/alekkowalczyk/herdr-tasks/internal/store"
)

const slackUsage = "slack status|fetch|skipped|take <n>|mutes|unmute <channel>|research|continue"

// slackCmd is `herdr-tasks slack status|fetch|skipped|take|mutes|unmute`.
// The server, the fetch and the take are funcs, so tests can fake them.
type slackCmd struct {
	opts reconcile.Options
	dir  string // the state dir
	home string
	out  io.Writer
	now  time.Time

	health func(dir string) (board.Meta, error)
	ensure func(dir string, port int, exe, version string) error
	exe    func() (string, error)
	fetch  func(context.Context) (slack.Fetched, error)
	take   func(ctx context.Context, which string) (slack.Delivered, error)
}

// fetchTimeout is how long `slack fetch` waits for a running check, and
// then runs its own, at most: with slack_topic, a check runs a topic check
// for each of up to 20 asks. `slack take` waits as long.
const fetchTimeout = 15 * time.Minute

func (a *app) slack() error {
	args := a.flags.args
	if len(args) == 0 {
		return errors.New(slackUsage)
	}
	switch args[0] {
	case "research":
		return a.slackResearch()
	case "continue":
		return a.slackContinue()
	}
	home, _ := os.UserHomeDir()
	c := slackCmd{
		opts: a.opts, dir: a.store.Dir, home: home, out: os.Stdout, now: time.Now(),
		health: board.Health, ensure: board.Ensure, exe: executable,
	}
	c.fetch = func(ctx context.Context) (slack.Fetched, error) { return fetchNow(ctx, c.dir) }
	c.take = func(ctx context.Context, which string) (slack.Delivered, error) { return takeNow(ctx, c.dir, which) }
	switch args[0] {
	case "status":
		return c.status()
	case "fetch":
		return c.fetchAsks()
	case "skipped":
		return c.skipped()
	case "take":
		return c.takeAsk(strings.TrimSpace(strings.Join(args[1:], " ")))
	case "mutes":
		return c.mutes()
	case "unmute":
		return c.unmute(strings.TrimSpace(strings.Join(args[1:], " ")))
	}
	return errors.New(slackUsage)
}

// watcherFor is a watcher built as the server builds its own, for the state
// folder dir, logging to slack.log there.
func watcherFor(dir string) (*slack.Watcher, error) {
	exe, err := executable()
	if err != nil {
		return nil, err
	}
	return &slack.Watcher{Dir: dir, Exe: exe, Options: reconcile.LoadOptions, Log: slack.OpenLog(dir)}, nil
}

// fetchNow fetches Slack asks for the state folder dir.
func fetchNow(ctx context.Context, dir string) (slack.Fetched, error) {
	w, err := watcherFor(dir)
	if err != nil {
		return slack.Fetched{}, err
	}
	return w.Fetch(ctx)
}

// takeNow takes a skipped ask for the state folder dir.
func takeNow(ctx context.Context, dir, which string) (slack.Delivered, error) {
	w, err := watcherFor(dir)
	if err != nil {
		return slack.Delivered{}, err
	}
	return w.Take(ctx, which)
}

// notifyContext is signal.NotifyContext. Tests fake it.
var notifyContext = signal.NotifyContext

// waiting is the context of a command that may wait for a check: Ctrl-C,
// or 15 minutes, ends it. Once it has ended, Ctrl-C works as by default
// again, so a second one ends the command while it winds down.
func waiting() (context.Context, context.CancelFunc) {
	ctx, stop := notifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	ctx, cancel := context.WithTimeoutCause(ctx, fetchTimeout, errors.New("it took over 15 minutes"))
	context.AfterFunc(ctx, stop)
	return ctx, func() { cancel(); stop() }
}

// fetchAsks is `herdr-tasks slack fetch`: one check now, whether or not the
// watcher is on. Ctrl-C, or 15 minutes, ends it, and the check's claude with
// it.
func (c slackCmd) fetchAsks() error {
	ctx, cancel := waiting()
	defer cancel()
	fmt.Fprintln(c.out, "Fetching Slack asks…")
	got, err := c.fetch(ctx)
	if err != nil {
		return err
	}
	fmt.Fprintln(c.out, got.Summary(c.now))
	return nil
}

// skipped is `herdr-tasks slack skipped`: the asks that made no task, as
// off-topic or maybe answered already, newest first, each with the number
// `slack take` takes, which it keeps. Skips over 7 days old, which the
// watcher drops when it next saves, aren't listed.
func (c slackCmd) skipped() error {
	st, err := slack.Load(c.dir)
	if err != nil {
		return err
	}
	st.Prune(c.now)
	list := st.SkippedList()
	if len(list) == 0 {
		fmt.Fprintln(c.out, "No asks were skipped.")
		return nil
	}
	for _, k := range list {
		fmt.Fprintf(c.out, "#%d %s — %s\n", k.N, k.Label(c.now.Location()), k.Reason())
		fmt.Fprintf(c.out, "   %s\n", k.Ask.Permalink)
	}
	fmt.Fprintln(c.out, "herdr-tasks slack take <#> makes its task.")
	return nil
}

// takeAsk is `herdr-tasks slack take <n|link>`: it makes the task for a
// skipped ask, by its number in `slack skipped`, with or without the "#",
// its link, or its message ID.
func (c slackCmd) takeAsk(which string) error {
	if which == "" {
		return errors.New("slack take <n|link>")
	}
	ctx, cancel := waiting()
	defer cancel()
	d, err := c.take(ctx, which)
	if err != nil {
		return err
	}
	fmt.Fprintln(c.out, d.TookLine())
	return nil
}

func (a *app) slackResearch() error {
	home, _ := os.UserHomeDir()
	set, err := slack.SettingsFrom(a.opts, home)
	if err != nil {
		return err
	}
	id, err := a.targetTab()
	if err != nil {
		return err
	}
	exe, err := executable()
	if err != nil {
		return err
	}
	return slack.StartResearch(a.host(), a.store, a.opts, set, id, exe, store.Dir(), time.Now())
}

func (a *app) slackContinue() error {
	id, err := a.targetTab()
	if err != nil {
		return err
	}
	return slack.Continue(a.host(), a.store, a.opts, id, syscall.Kill, time.Now())
}

// slackTrouble reports whether the tab bar shows Slack ⚠: the watcher is on,
// it has a problem, and this herdr session is the one it works for. While
// that session isn't running, which is the problem itself, every session
// shows it.
func slackTrouble(opts reconcile.Options, session, dir string) bool {
	if !opts.SlackWatch {
		return false
	}
	st, err := slack.Load(dir)
	if err != nil {
		return false
	}
	if session == opts.SlackSessionName() {
		return st.Problem != ""
	}
	return st.Problem == slack.WaitingFor(opts.SlackSessionName())
}

func (c slackCmd) status() error {
	if !c.opts.SlackWatch {
		fmt.Fprintln(c.out, "Slack watcher: off (set slack_watch = true in "+configFile()+")")
		// What the watcher and fetches found still shows, and the watcher
		// isn't started. A problem on disk is from when it was on.
		st := slack.OnDisk(c.dir)
		st.Prune(c.now)
		c.lastCheck(st)
		if topic := strings.TrimSpace(c.opts.SlackTopic); topic != "" {
			fmt.Fprintln(c.out, "Topic: "+topic)
		}
		c.skippedCount(st)
		return nil
	}
	set, err := slack.SettingsFrom(c.opts, c.home)
	if err != nil {
		return err
	}
	st, err := slack.Load(c.dir)
	if err != nil {
		return err
	}
	// What the watcher drops when it next saves isn't counted; the file
	// stays as it is.
	st.Prune(c.now)
	which := "the only herdr session running"
	if set.Session != "" {
		which = "herdr session " + set.Session
	}
	fmt.Fprintf(c.out, "Slack watcher: on, for %s, workspace %s\n", which, set.Workspace)

	checks := "every " + c.opts.SlackEvery + ", " + c.opts.SlackHours
	if !strings.EqualFold(strings.TrimSpace(c.opts.SlackHours), "always") {
		if set.Hours.Open(c.now) {
			checks += " (open now)"
		} else {
			checks += " (closed now; next " + set.Hours.Next(c.now).Format("Mon 15:04") + ")"
		}
	}
	fmt.Fprintln(c.out, "Checks: "+checks)
	if set.Topic != "" {
		fmt.Fprintln(c.out, "Topic: "+set.Topic)
	}

	c.lastCheck(st)
	// A problem can sit on disk while the watcher is off; this is only on.
	if st.Problem != "" {
		fmt.Fprintln(c.out, "Problem: "+slack.ProblemText(st.Problem, st.ProblemSince, c.now))
	}
	started := 0
	for _, t := range st.Starts {
		if c.now.Sub(t) <= time.Hour {
			started++
		}
	}
	fmt.Fprintf(c.out, "Research sessions started this hour: %d of %d\n", started, set.MaxNew)
	if muted := mutedLine(st.Muted, set.Skip); muted != "" {
		fmt.Fprintln(c.out, "Muted: "+muted)
	}
	c.skippedCount(st)

	// The server runs the watcher. Start it even with board = false: the
	// board command refuses then, so this is the way to start it mid-session.
	if _, err := c.health(c.dir); err != nil {
		exe, err := c.exe()
		if err == nil {
			err = c.ensure(c.dir, c.opts.Port(), exe, version)
		}
		if err != nil {
			return errors.New("couldn't start the watcher: " + boardTrouble(err, c.opts.Port()))
		}
		fmt.Fprintln(c.out, "Started the watcher.")
	}
	return nil
}

// lastCheck prints when Slack was last checked, and what that cost, once it
// was.
func (c slackCmd) lastCheck(st slack.State) {
	if !st.LastCheck.IsZero() {
		fmt.Fprintf(c.out, "Last check: %s ($%.3f)\n", st.LastCheck.In(c.now.Location()).Format("Jan 2 15:04"), st.LastCost)
	}
}

// skippedCount prints how many asks are on the skipped list, off-topic and
// maybe answered alike, when any are.
func (c slackCmd) skippedCount(st slack.State) {
	if n := len(st.Skipped); n > 0 {
		fmt.Fprintf(c.out, "Skipped: %d (herdr-tasks slack skipped)\n", n)
	}
}

// mutedLine lists the channels muted from cards, then those in
// slack_skip_channels.
func mutedLine(muted []slack.Mute, skip []string) string {
	var parts []string
	if len(muted) > 0 {
		names := make([]string, len(muted))
		for i, m := range muted {
			names[i] = mutedName(m)
		}
		parts = append(parts, strings.Join(names, ", "))
	}
	if len(skip) > 0 {
		parts = append(parts, "In config.toml: "+strings.Join(skip, ", "))
	}
	return strings.Join(parts, ". ")
}

func mutedName(m slack.Mute) string {
	if m.Name == "" {
		return m.ID
	}
	return m.Name + " (" + m.ID + ")"
}

func (c slackCmd) mutes() error {
	st, err := slack.Load(c.dir)
	if err != nil {
		return err
	}
	if len(st.Muted) == 0 && len(c.opts.SlackSkipChannels) == 0 {
		fmt.Fprintln(c.out, "No channels are muted.")
		return nil
	}
	if len(st.Muted) > 0 {
		fmt.Fprintln(c.out, "Muted from cards (herdr-tasks slack unmute <channel> undoes it):")
		for _, m := range st.Muted {
			fmt.Fprintln(c.out, "  "+mutedName(m))
		}
	}
	if len(c.opts.SlackSkipChannels) > 0 {
		fmt.Fprintln(c.out, "In slack_skip_channels in config.toml:")
		for _, k := range c.opts.SlackSkipChannels {
			fmt.Fprintln(c.out, "  "+k)
		}
	}
	return nil
}

var errNotInState = errors.New("not in the state")

func (c slackCmd) unmute(which string) error {
	if which == "" {
		return errors.New("slack unmute <channel>")
	}
	var gone slack.Mute
	err := slack.Update(c.dir, func(s *slack.State) error {
		before := slices.Clone(s.Muted)
		if !s.Unmute(which) {
			return errNotInState
		}
		for _, m := range before {
			if !slices.Contains(s.Muted, m) {
				gone = m
			}
		}
		return nil
	})
	var none slack.State
	switch {
	case err == nil:
		line := "herdr-tasks: unmuted " + which
		if none.IsMuted(gone.ID, gone.Name, c.opts.SlackSkipChannels) {
			line += "; it's still in slack_skip_channels in config.toml"
		}
		fmt.Fprintln(c.out, line)
		return nil
	case !errors.Is(err, errNotInState):
		return err
	}
	if none.IsMuted(which, which, c.opts.SlackSkipChannels) {
		return fmt.Errorf("%s is in slack_skip_channels in config.toml; take it out there", which)
	}
	return fmt.Errorf("%s isn't muted", which)
}

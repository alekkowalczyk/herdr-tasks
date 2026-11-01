package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"slices"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/alekkowalczyk/herdr-tasks/internal/board"
	"github.com/alekkowalczyk/herdr-tasks/internal/reconcile"
	"github.com/alekkowalczyk/herdr-tasks/internal/slack"
	"github.com/alekkowalczyk/herdr-tasks/internal/store"
)

var slackNow = time.Date(2026, 10, 30, 14, 40, 0, 0, time.UTC) // a Friday

func slackOpts() reconcile.Options {
	return reconcile.Options{
		SlackWatch: true, SlackSession: "work", SlackWorkspace: "Slack", SlackDMs: true,
		SlackEvery: "5m", SlackHours: "Mon-Fri 08:00-18:00", SlackMaxNew: 4,
		SlackSkipChannels: []string{"random"},
	}
}

// fakeSlack is the slack command with a fake server; calls lists what it
// asked the server to do.
type fakeSlack struct {
	slackCmd
	out   bytes.Buffer
	calls []string
	up    bool
}

func newFakeSlack(t *testing.T, opts reconcile.Options) *fakeSlack {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("HERDR_PLUGIN_STATE_DIR", dir)
	t.Setenv("HERDR_PLUGIN_CONFIG_DIR", t.TempDir())
	f := &fakeSlack{}
	f.slackCmd = slackCmd{
		opts: opts, dir: dir, home: "/home/u", out: &f.out, now: slackNow,
		health: func(string) (board.Meta, error) {
			if f.up {
				return board.Meta{}, nil
			}
			return board.Meta{}, errors.New("not running")
		},
		ensure: func(dir string, port int, exe, v string) error {
			f.calls = append(f.calls, "ensure "+exe)
			return nil
		},
		exe: func() (string, error) { return "/plugin/bin/herdr-tasks", nil },
	}
	return f
}

func saveSlack(t *testing.T, dir string, fn func(*slack.State)) {
	t.Helper()
	if err := slack.Update(dir, func(s *slack.State) error { fn(s); return nil }); err != nil {
		t.Fatal(err)
	}
}

func TestSlackStatusPrints(t *testing.T) {
	f := newFakeSlack(t, slackOpts())
	since := time.Date(2026, 10, 30, 14, 20, 0, 0, time.UTC)
	saveSlack(t, f.dir, func(s *slack.State) {
		s.On = true
		s.LastCheck, s.LastCost = time.Date(2026, 10, 30, 14, 32, 0, 0, time.UTC), 0.008
		s.Problem, s.ProblemSince = "can't reach Slack. Run /mcp in Claude Code to sign in again", &since
		s.Starts = []time.Time{slackNow.Add(-10 * time.Minute), slackNow.Add(-2 * time.Hour)}
		s.Mute("C0123ABCD", "#billing")
	})
	if err := f.status(); err != nil {
		t.Fatal(err)
	}
	want := `Slack watcher: on, for herdr session work, workspace Slack
Checks: every 5m, Mon-Fri 08:00-18:00 (open now)
Last check: Oct 30 14:32 ($0.008)
Problem: can't reach Slack since 14:20. Run /mcp in Claude Code to sign in again
Research sessions started this hour: 1 of 4
Muted: #billing (C0123ABCD). In config.toml: random
Started the watcher.
`
	if got := f.out.String(); got != want {
		t.Errorf("printed:\n%s\nwant:\n%s", got, want)
	}
	if len(f.calls) != 1 {
		t.Errorf("server calls %q, want one ensure", f.calls)
	}
}

func TestSlackStatusLeavesOutWhatDoesntApply(t *testing.T) {
	f := newFakeSlack(t, slackOpts())
	f.up = true
	f.opts.SlackSkipChannels = nil
	saveSlack(t, f.dir, func(s *slack.State) {
		s.Problem = "stale" // on disk from before; the watcher is off below
	})
	if err := f.status(); err != nil {
		t.Fatal(err)
	}
	got := f.out.String()
	for _, no := range []string{"Last check", "Muted", "Started", "Topic", "Skipped"} {
		if strings.Contains(got, no) {
			t.Errorf("printed %q with nothing to say about it:\n%s", no, got)
		}
	}
	if len(f.calls) != 0 {
		t.Errorf("started a server that answers: %q", f.calls)
	}

	f = newFakeSlack(t, reconcile.Options{})
	saveSlack(t, f.dir, func(s *slack.State) { s.Problem = "stale" })
	if err := f.status(); err != nil {
		t.Fatal(err)
	}
	got = f.out.String()
	if !strings.HasPrefix(got, "Slack watcher: off") || strings.Contains(got, "Problem") || len(f.calls) != 0 {
		t.Errorf("with slack_watch off, printed %q and called %q", got, f.calls)
	}
}

// TestSlackStatusWhileOff: with slack_watch off, status still says when
// Slack was last checked, the topic, and how many asks were skipped, of
// both kinds, as each applies. It doesn't start the watcher.
func TestSlackStatusWhileOff(t *testing.T) {
	opts := slackOpts()
	opts.SlackWatch = false
	opts.SlackTopic = "the acme billing service"
	f := newFakeSlack(t, opts)
	saveSlack(t, f.dir, func(s *slack.State) {
		s.LastCheck, s.LastCost = time.Date(2026, 10, 30, 14, 32, 0, 0, time.UTC), 0.008
		s.Problem = "stale"
		s.Skip(skippedAsk(time.Date(2026, 10, 28, 12, 59, 0, 0, time.UTC), false, "lunch?"), slack.Skip{Why: "lunch plans"}, slackNow.Add(-time.Hour))
		s.Skip(skippedAsk(time.Date(2026, 10, 29, 9, 0, 0, 0, time.UTC), false, "the refund?"), slack.Skip{Answered: true}, slackNow.Add(-30*time.Minute))
		// Over 7 days old: not counted.
		s.Skip(skippedAsk(time.Date(2026, 10, 20, 9, 0, 0, 0, time.UTC), false, "old news"), slack.Skip{Why: "old news"}, slackNow.Add(-8*24*time.Hour))
	})
	if err := f.status(); err != nil {
		t.Fatal(err)
	}
	want := "Slack watcher: off (set slack_watch = true in " + configFile() + ")\n" +
		"Last check: Oct 30 14:32 ($0.008)\n" +
		"Topic: the acme billing service\n" +
		"Skipped: 2 (herdr-tasks slack skipped)\n"
	if got := f.out.String(); got != want {
		t.Errorf("printed:\n%s\nwant:\n%s", got, want)
	}
	if len(f.calls) != 0 {
		t.Errorf("started the watcher while it's off: %q", f.calls)
	}
}

func TestSlackStatusStartsServerWithBoardOff(t *testing.T) {
	f := newFakeSlack(t, slackOpts()) // Board is false
	if err := f.status(); err != nil {
		t.Fatal(err)
	}
	if len(f.calls) != 1 || !strings.HasSuffix(f.out.String(), "Started the watcher.\n") {
		t.Errorf("calls %q, printed %q", f.calls, f.out.String())
	}
}

func TestUnmute(t *testing.T) {
	f := newFakeSlack(t, slackOpts())
	saveSlack(t, f.dir, func(s *slack.State) { s.Mute("C0123ABCD", "#billing") })

	if err := f.unmute("C0999ZZZZ"); err == nil || err.Error() != "C0999ZZZZ isn't muted" {
		t.Errorf("unknown channel: %v", err)
	}
	err := f.unmute("random")
	if err == nil || err.Error() != "random is in slack_skip_channels in config.toml; take it out there" {
		t.Errorf("channel only in config: %v", err)
	}
	if err := f.unmute("#billing"); err != nil {
		t.Fatal(err)
	}
	st, _ := slack.Load(f.dir)
	if len(st.Muted) != 0 {
		t.Errorf("still muted: %v", st.Muted)
	}
	if err := f.unmute("#billing"); err == nil {
		t.Error("unmuting twice worked")
	}
	if got := f.out.String(); got != "herdr-tasks: unmuted #billing\n" {
		t.Errorf("printed %q", got)
	}

	// A channel you muted that's in slack_skip_channels too stays muted.
	f.out.Reset()
	saveSlack(t, f.dir, func(s *slack.State) { s.Mute("C0999ZZZZ", "#random") })
	if err := f.unmute("C0999ZZZZ"); err != nil {
		t.Fatal(err)
	}
	if got, want := f.out.String(), "herdr-tasks: unmuted C0999ZZZZ; it's still in slack_skip_channels in config.toml\n"; got != want {
		t.Errorf("printed %q, want %q", got, want)
	}
}

func TestSlackMutes(t *testing.T) {
	f := newFakeSlack(t, slackOpts())
	saveSlack(t, f.dir, func(s *slack.State) { s.Mute("C0123ABCD", "#billing") })
	if err := f.mutes(); err != nil {
		t.Fatal(err)
	}
	got := f.out.String()
	if !strings.Contains(got, "  #billing (C0123ABCD)\n") || !strings.Contains(got, "  random\n") {
		t.Errorf("printed:\n%s", got)
	}
}

func TestSlackFetchCommand(t *testing.T) {
	f := newFakeSlack(t, reconcile.Options{}) // works with the watcher off
	from := time.Date(2026, 10, 30, 14, 20, 0, 0, time.UTC)
	fetched := slack.Fetched{From: from, New: 2, Research: 1, Joined: 1}
	calls := 0
	f.fetch = func(ctx context.Context) (slack.Fetched, error) {
		calls++
		if at, ok := ctx.Deadline(); !ok || time.Until(at) < 14*time.Minute || time.Until(at) > 15*time.Minute {
			t.Errorf("the fetch's deadline is %v away, want 15 minutes", time.Until(at))
		}
		return fetched, nil
	}
	if err := f.fetchAsks(); err != nil {
		t.Fatal(err)
	}
	want := "Fetching Slack asks…\n" + fetched.Summary(slackNow) + "\n"
	if got := f.out.String(); got != want || calls != 1 {
		t.Errorf("printed %q after %d fetches, want %q", got, calls, want)
	}
	if want != "Fetching Slack asks…\nFetched from 14:20: 2 new tasks, 1 with research; 1 added to a task.\n" {
		t.Errorf("the summary changed: %q", want)
	}

	f.out.Reset()
	f.fetch = func(context.Context) (slack.Fetched, error) {
		return slack.Fetched{}, errors.New("can't fetch: waiting for a herdr session")
	}
	if err := f.fetchAsks(); err == nil || err.Error() != "can't fetch: waiting for a herdr session" {
		t.Errorf("fetchAsks = %v, want the fetch's error", err)
	}
	if got := f.out.String(); got != "Fetching Slack asks…\n" {
		t.Errorf("printed %q", got)
	}
}

// TestWaitingRestoresCtrlC: once the context of a fetch or a take ends, by
// Ctrl-C or its time limit, the signals are handled as by default again, so
// a second Ctrl-C ends the process while the command winds down.
func TestWaitingRestoresCtrlC(t *testing.T) {
	old := notifyContext
	t.Cleanup(func() { notifyContext = old })
	var interrupt context.CancelFunc
	restored := make(chan struct{})
	var once sync.Once
	notifyContext = func(parent context.Context, sigs ...os.Signal) (context.Context, context.CancelFunc) {
		if !slices.Contains(sigs, os.Interrupt) || !slices.Contains(sigs, os.Signal(syscall.SIGTERM)) {
			t.Errorf("notified of %v, want Ctrl-C and SIGTERM", sigs)
		}
		ctx, cancel := context.WithCancel(parent)
		interrupt = cancel
		return ctx, func() { once.Do(func() { close(restored) }) }
	}
	ctx, cancel := waiting()
	defer cancel()
	select {
	case <-restored:
		t.Fatal("the signals were handled as by default while the command waited")
	case <-time.After(50 * time.Millisecond):
	}
	interrupt() // the first Ctrl-C
	<-ctx.Done()
	select {
	case <-restored:
	case <-time.After(time.Second):
		t.Fatal("once stopped, the signals are still caught: a second Ctrl-C wouldn't end the process")
	}
}

// skippedAsk is an ask posted at, in #billing, or in a DM with Rui when dm.
func skippedAsk(at time.Time, dm bool, text string) slack.Ask {
	ts := fmt.Sprintf("%d.000100", at.Unix())
	m := slack.Message{Channel: "C0123ABCD", Place: "#billing", AuthorID: "U0DANA001", Author: "Dana Reed", TS: ts, Text: text}
	kind, key := slack.KindMention, "C0123ABCD/"+ts
	if dm {
		m.Channel, m.Place, m.AuthorID, m.Author = "D0RUI0001", "DM", "U0RUI0001", "Rui Costa"
		kind, key = slack.KindDM, "D0RUI0001"
	}
	m.Permalink = "https://acme.slack.com/archives/" + m.Channel + "/p" + strings.Replace(ts, ".", "", 1)
	return slack.Ask{Message: m, Kind: kind, Key: key, Gist: "lunch", At: at}
}

func TestSlackSkippedAndTakeCommands(t *testing.T) {
	f := newFakeSlack(t, slackOpts())
	if err := f.skipped(); err != nil {
		t.Fatal(err)
	}
	if got := f.out.String(); got != "No asks were skipped.\n" {
		t.Errorf("printed %q with none skipped", got)
	}

	dana := skippedAsk(time.Date(2026, 10, 28, 12, 59, 0, 0, time.UTC), false, "<@U0ME00001> lunch on Friday, the usual place before the invoice review?")
	rui := skippedAsk(time.Date(2026, 10, 30, 14, 30, 0, 0, time.UTC), true, "book club tonight?")
	refund := skippedAsk(time.Date(2026, 10, 29, 9, 0, 0, 0, time.UTC), false, "<@U0ME00001> the refund?")
	saveSlack(t, f.dir, func(s *slack.State) {
		s.Skip(dana, slack.Skip{Why: "lunch plans"}, slackNow.Add(-time.Hour))
		s.Skip(refund, slack.Skip{Answered: true}, slackNow.Add(-30*time.Minute))
		s.Skip(rui, slack.Skip{Why: "a book club"}, slackNow.Add(-10*time.Minute))
		// Over 7 days old: neither listed nor counted.
		s.Skip(skippedAsk(time.Date(2026, 10, 20, 9, 0, 0, 0, time.UTC), false, "old news"), slack.Skip{Why: "old news"}, slackNow.Add(-8*24*time.Hour))
	})
	f.out.Reset()
	if err := f.skipped(); err != nil {
		t.Fatal(err)
	}
	// Each keeps the number it got when it was skipped.
	want := "#3 Oct 30 14:30 DM · Rui Costa: book club tonight? — off-topic: a book club\n" +
		"   " + rui.Permalink + "\n" +
		"#2 Oct 29 09:00 #billing · Dana Reed: the refund? — may be answered already\n" +
		"   " + refund.Permalink + "\n" +
		"#1 Oct 28 12:59 #billing · Dana Reed: lunch on Friday, the usual place before… — off-topic: lunch plans\n" +
		"   " + dana.Permalink + "\n" +
		"herdr-tasks slack take <#> makes its task.\n"
	if got := f.out.String(); got != want {
		t.Errorf("printed:\n%s\nwant:\n%s", got, want)
	}

	// Take prints where the ask went.
	var took []string
	for _, c := range []struct {
		d    slack.Delivered
		want string
	}{
		{slack.Delivered{Tab: "w2:t1", Research: true}, "Took it: new tab w2:t1, with research\n"},
		{slack.Delivered{Tab: "w2:t1"}, "Took it: new tab w2:t1, research held\n"},
		{slack.Delivered{Tab: "w3:t9", FollowUp: true}, "Took it: added to the task in w3:t9\n"},
		{slack.Delivered{Tab: "w2:t1", Research: true, ResearchErr: errors.New("Research didn't start: herdr: pane_busy:\n\tthe pane isn't\x1b at a shell prompt")},
			"Took it: new tab w2:t1, research didn't start: herdr: pane_busy: the pane isn't at a shell prompt\n"},
	} {
		f.out.Reset()
		f.take = func(ctx context.Context, which string) (slack.Delivered, error) {
			if _, ok := ctx.Deadline(); !ok {
				t.Error("the take has no deadline")
			}
			took = append(took, which)
			return c.d, nil
		}
		if err := f.takeAsk("2"); err != nil {
			t.Fatal(err)
		}
		if got := f.out.String(); got != c.want {
			t.Errorf("printed %q, want %q", got, c.want)
		}
	}
	if len(took) != 4 || took[0] != "2" {
		t.Errorf("took %q", took)
	}
	f.take = func(context.Context, string) (slack.Delivered, error) {
		return slack.Delivered{}, errors.New("no skipped ask 9")
	}
	f.out.Reset()
	if err := f.takeAsk("9"); err == nil || err.Error() != "no skipped ask 9" || f.out.Len() != 0 {
		t.Errorf("takeAsk(9) = %v, printed %q", err, f.out.String())
	}
	if err := f.takeAsk(""); err == nil || !strings.Contains(err.Error(), "slack take <n|link>") {
		t.Errorf("takeAsk() = %v, want the usage", err)
	}

	// Status shows the topic and how many were skipped.
	f.up = true
	f.opts.SlackTopic = "the acme billing service"
	f.out.Reset()
	if err := f.status(); err != nil {
		t.Fatal(err)
	}
	got := f.out.String()
	if !strings.Contains(got, "\nTopic: the acme billing service\n") || !strings.Contains(got, "\nSkipped: 3 (herdr-tasks slack skipped)\n") {
		t.Errorf("status:\n%s", got)
	}
	if !strings.Contains(slackUsage, "skipped|take <n>") {
		t.Errorf("usage = %q", slackUsage)
	}
}

func TestTabBarShowsSlackTrouble(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("HERDR_PLUGIN_STATE_DIR", dir)
	t.Setenv("HERDR_ACTIVE_TAB_ID", "w1:t1")
	line := func(session string, opts reconcile.Options) string {
		t.Helper()
		a := &app{store: &store.Store{Dir: dir, Session: session}, opts: opts}
		s, err := a.statusLine()
		if err != nil {
			t.Fatal(err)
		}
		return s
	}
	opts := slackOpts()

	if got := line("work", opts); strings.Contains(got, "Slack ⚠") {
		t.Errorf("with no problem: %q", got)
	}
	saveSlack(t, dir, func(s *slack.State) { s.Problem = "can't reach Slack" })
	if got := line("work", opts); !strings.HasSuffix(got, "  Slack ⚠") {
		t.Errorf("the slack_session's tab bar: %q", got)
	}
	if got := line("home", opts); strings.Contains(got, "Slack ⚠") {
		t.Errorf("another session's tab bar: %q", got)
	}
	opts.SlackSession = ""
	if got := line("home", opts); !strings.HasSuffix(got, "  Slack ⚠") {
		t.Errorf("with no slack_session: %q", got)
	}
	opts.SlackWatch = false
	if got := line("home", opts); strings.Contains(got, "Slack ⚠") {
		t.Errorf("with slack_watch off: %q", got)
	}
}

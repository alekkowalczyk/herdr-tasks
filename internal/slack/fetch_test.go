package slack

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/alekkowalczyk/herdr-tasks/internal/task"
)

func (r *rig) fetch(t *testing.T) Fetched {
	t.Helper()
	got, err := r.Fetch(context.Background())
	if err != nil {
		t.Fatalf("Fetch = %v; log:\n%s", err, r.log.String())
	}
	return got
}

// logLines are the lines logged, in order.
func (r *rig) logLines() []string {
	return strings.Split(strings.TrimSuffix(r.log.String(), "\n"), "\n")
}

func TestFetchWorksWithTheWatcherOff(t *testing.T) {
	r := newRig(t)
	r.opts.SlackWatch = false
	r.opts.SlackHours = "Mon-Fri 08:00-09:00" // closed at 14:00
	r.slack.mentions = []Message{billing("U0DANA001", tsAt(watchStart.Add(-time.Hour)), "", "<@U0ME00001> can the invoice fix ship Friday?")}
	got := r.fetch(t)

	if len(r.slack.runs) != 2 || r.slack.runs[0] != IdentifyPrompt || r.slack.runs[1] == IdentifyPrompt {
		t.Fatalf("runs = %q, want the identify run, then a check", r.slack.runs)
	}
	if r.tabs() != 1 {
		t.Fatalf("%d tabs, want 1; log:\n%s", r.tabs(), r.log.String())
	}
	st := r.state(t)
	if st.On || st.UserID != me || !st.LastCheck.Equal(watchStart) || st.Problem != "" {
		t.Errorf("state = %+v, want you identified, a check, and the watcher still off", st)
	}
	if got.New != 1 || got.Research != 1 || got.Joined != 0 || got.More {
		t.Errorf("Fetched = %+v", got)
	}
	if s := r.Status(); s != (Status{Text: "Slack: off", Fetch: true}) {
		t.Errorf("status = %+v, want off", s)
	}
	lines := r.logLines()
	if lines[0] != "fetching, as asked" || lines[len(lines)-1] != got.Summary(watchStart) {
		t.Errorf("log = %q, want the fetch first and its summary last", lines)
	}

	// The watcher, still off, doesn't check.
	r.clock.add(10 * time.Minute)
	r.tick()
	if len(r.slack.runs) != 2 {
		t.Errorf("a tick while off ran %q", r.slack.runs[2:])
	}
}

func TestFirstFetchGoesBack72Hours(t *testing.T) {
	r := newRig(t)
	back := watchStart.Add(-72 * time.Hour)
	r.slack.mentions = []Message{
		billing("U0DANA001", tsAt(watchStart.Add(-73*time.Hour)), "", "<@U0ME00001> too old"),
		billing("U0RUI0001", tsAt(watchStart.Add(-71*time.Hour)), "", "<@U0ME00001> can the invoice fix ship Friday?"),
	}
	got := r.fetch(t)
	if len(r.slack.runs) != 2 || !strings.Contains(r.slack.runs[1], fmt.Sprintf("uses after %d,", back.Unix())) {
		t.Fatalf("runs = %q, want a check from %v", r.slack.runs, back)
	}
	if !got.From.Equal(back) || got.New != 1 || r.tabs() != 1 {
		t.Errorf("Fetched = %+v, %d tabs; want one ask from %v on", got, r.tabs(), back)
	}
	if st := r.state(t); !st.Since.Equal(watchStart) || !st.LastCheck.Equal(watchStart) {
		t.Errorf("since %v, last check %v; want both %v", st.Since, st.LastCheck, watchStart)
	}

	// You were identified before, but there's no watermark: 72 h back too.
	r = newRig(t)
	if err := Update(r.Dir, func(s *State) error { s.UserID = me; return nil }); err != nil {
		t.Fatal(err)
	}
	r.fetch(t)
	if len(r.slack.runs) != 1 || !strings.Contains(r.slack.runs[0], fmt.Sprintf("uses after %d,", back.Unix())) {
		t.Errorf("runs = %q, want only a check from %v", r.slack.runs, back)
	}
}

func TestFetchContinuesFromTheWatermark(t *testing.T) {
	r := newRig(t)
	r.identified(t)
	r.clock.add(5 * time.Minute)
	r.tick()
	st := r.state(t)
	if !st.Since.Equal(st.LastCheck) || !st.Since.Equal(r.clock.now()) {
		t.Fatalf("since %v, last check %v", st.Since, st.LastCheck)
	}
	// A minute later: slack_every hasn't passed, and a fetch checks anyway.
	r.clock.add(time.Minute)
	r.slack.mentions = []Message{billing("U0DANA001", tsAt(r.clock.now().Add(-30*time.Second)), "", "<@U0ME00001> can the invoice fix ship Friday?")}
	got := r.fetch(t)
	from := st.Since.Add(-10 * time.Minute)
	if len(r.slack.runs) != 3 || !strings.Contains(r.slack.runs[2], fmt.Sprintf("uses after %d,", from.Unix())) {
		t.Fatalf("runs = %q, want a third run from %v", r.slack.runs, from)
	}
	if !got.From.Equal(from) || got.New != 1 {
		t.Errorf("Fetched = %+v", got)
	}
	if s := r.Status(); s != (Status{On: true, Fetch: true, Text: "Slack: last check 14:06"}) {
		t.Errorf("status = %+v", s)
	}
}

func TestFetchSaysThereIsMore(t *testing.T) {
	r := newRig(t)
	r.identified(t)
	for i := range 25 {
		at := watchStart.Add(time.Duration(i+1) * 10 * time.Second)
		r.slack.mentions = append(r.slack.mentions, billing("U0DANA001", tsAt(at), "", fmt.Sprintf("<@U0ME00001> question %d", i+1)))
	}
	// Your newest 20 messages are from 14:01:05 on: the 6 asks before them
	// may be answered.
	for i := range 20 {
		at := watchStart.Add(time.Minute + time.Duration(5+i)*time.Second)
		r.slack.own = append(r.slack.own, msg("C0OPS0001", "#ops", me, tsAt(at), "", "deploying"))
	}
	r.clock.add(5 * time.Minute)
	got := r.fetch(t)
	if !got.More || got.New != 14 || got.Unsure != 6 || got.Research != 4 || len(r.slack.runs) != 2 {
		t.Fatalf("Fetched = %+v after %d runs, want 14 new, 6 maybe answered, 4 with research, more, and one check", got, len(r.slack.runs))
	}
	want := "Fetched from 13:50: 14 new tasks, 4 with research; 6 may be answered (herdr-tasks slack skipped). There's more: fetch again."
	if s := got.Summary(r.clock.now()); s != want {
		t.Errorf("summary = %q\nwant      %q", s, want)
	}
	st := r.state(t)
	if !st.Since.Equal(atTS(r.slack.mentions[19].TS)) {
		t.Errorf("since = %v, want the 20th ask's time", st.Since)
	}
	if r.tabs() != 14 || len(st.Starts) != 4 || len(r.herdr.callsTo("StartAgent")) != 4 {
		t.Errorf("%d tabs, starts %v, %d agents; want 14 tabs, and the 4 starts the hour allows", r.tabs(), st.Starts, len(r.herdr.callsTo("StartAgent")))
	}
	if len(st.Skipped) != 6 {
		t.Errorf("%d skipped, want the 6 maybe answered", len(st.Skipped))
	}
	for i, k := range st.Skipped {
		if k.Ask.ID() != r.slack.mentions[i].ID() || !k.Answered || k.Why != "" {
			t.Errorf("skipped %d = %+v, want question %d, maybe answered", i, k, i+1)
		}
	}
	if n := strings.Count(r.log.String(), ": may be answered already; on the skipped list\n"); n != 6 {
		t.Errorf("logged %d maybe answered, want 6:\n%s", n, r.log.String())
	}

	// The next fetch takes the rest.
	got = r.fetch(t)
	if got.More || got.New != 5 || r.tabs() != 19 {
		t.Errorf("again: Fetched = %+v, %d tabs; want the other 5", got, r.tabs())
	}
}

// TestMaybeAnsweredGoesOnTheSkippedList: an ask you may have answered
// already makes no task. It goes on the skipped list, without a topic
// check, and the fetch counts it. One in a conversation that has a task
// joins that task. Taking one says it isn't answered: its research starts.
func TestMaybeAnsweredGoesOnTheSkippedList(t *testing.T) {
	r := newRig(t)
	r.opts.SlackTopic = billingTopic
	r.identified(t)
	thread := billing("U0DANA001", tsAt(watchStart.Add(time.Minute)), "", "<@U0ME00001> can the invoice fix ship Friday?")
	r.slack.mentions = []Message{thread}
	r.clock.add(5 * time.Minute)
	r.tick()
	if r.tabs() != 1 || len(r.slack.judged) != 1 {
		t.Fatalf("%d tabs, %d topic checks; want Dana's thread's; log:\n%s", r.tabs(), len(r.slack.judged), r.log.String())
	}
	threadTab := r.herdr.created[0].TabID

	// Rui asks in a DM, and Dana follows up in her thread. Then you write 20
	// messages elsewhere, so both may be answered.
	at := r.clock.now()
	rui := dm("U0RUI0001", tsAt(at.Add(10*time.Second)), "can you check the refund?")
	more := billing("U0DANA001", tsAt(at.Add(20*time.Second)), thread.TS, "<@U0ME00001> and the refund?")
	r.slack.mentions = append(r.slack.mentions, more)
	r.slack.dms = []Message{rui}
	for i := range 20 {
		r.slack.own = append(r.slack.own, msg("C0OPS0001", "#ops", me, tsAt(at.Add(time.Minute+time.Duration(i)*time.Second)), "", "deploying"))
	}
	r.clock.add(5 * time.Minute)
	r.log.Reset()
	got := r.fetch(t)
	if r.tabs() != 1 || len(r.slack.judged) != 1 {
		t.Fatalf("%d tabs, %d topic checks; want no new tab, and no topic check; log:\n%s", r.tabs(), len(r.slack.judged), r.log.String())
	}
	if got.New != 0 || got.Joined != 1 || got.Unsure != 1 || got.OffTopic != 0 || got.Research != 0 {
		t.Errorf("Fetched = %+v, want 1 joined and 1 maybe answered", got)
	}
	want := "Fetched from 13:55: 1 added to a task; 1 may be answered (herdr-tasks slack skipped)."
	if s := got.Summary(r.clock.now()); s != want {
		t.Errorf("summary = %q\nwant      %q", s, want)
	}
	st := r.state(t)
	if len(st.Skipped) != 1 {
		t.Fatalf("skipped = %+v, want Rui's DM", st.Skipped)
	}
	if k := st.Skipped[0]; k.Ask.ID() != rui.ID() || !k.Answered || k.Why != "" || !k.Ask.Unsure || !k.At.Equal(r.clock.now()) {
		t.Errorf("skipped = %+v, want Rui's DM, maybe answered", k)
	}
	if _, ok := st.Seen[rui.ID()]; !ok {
		t.Errorf("Rui's DM isn't seen")
	}
	if tk := loadTask(t, r.store, threadTab); len(tk.Links) != 2 {
		t.Errorf("Dana's task has links %+v, want her follow-up's too", tk.Links)
	}
	logged := r.log.String()
	if !strings.Contains(logged, "\nD0RUI0001: may be answered already; on the skipped list\n") ||
		!strings.Contains(logged, "\n"+thread.ID()+": added to the task in "+threadTab+"\n") {
		t.Errorf("log = %q", logged)
	}

	// Taking it says it isn't answered: its research starts.
	d, err := r.Take(context.Background(), rui.ID())
	if err != nil || d.Tab == "" || d.FollowUp || !d.Research {
		t.Fatalf("Take = %+v, %v; want a new tab with research", d, err)
	}
	if tk := loadTask(t, r.store, d.Tab); tk.Slack == nil || tk.Slack.Research == task.ResearchHeld {
		t.Errorf("task = %+v, want its research started", tk.Slack)
	}
	if len(r.herdr.callsTo("StartAgent")) != 2 {
		t.Errorf("%d research sessions, want Dana's and Rui's", len(r.herdr.callsTo("StartAgent")))
	}
}

func TestFetchWaitsForARunningCheck(t *testing.T) {
	r := newRig(t)
	r.identified(t)
	r.slack.mentions = []Message{billing("U0DANA001", tsAt(watchStart.Add(time.Minute)), "", "<@U0ME00001> can the invoice fix ship Friday?")}
	unlock, err := LockChecks(context.Background(), r.Dir, false)
	if err != nil {
		t.Fatal(err)
	}
	var released atomic.Bool
	time.AfterFunc(150*time.Millisecond, func() { released.Store(true); unlock() })
	run := r.Watcher.Claude
	r.Watcher.Claude = func(ctx context.Context, dir string, args []string, stdin string) ([]byte, error) {
		if !released.Load() {
			t.Error("the fetch ran a check while another held the lock")
		}
		return run(ctx, dir, args, stdin)
	}
	r.clock.add(time.Minute)
	got := r.fetch(t)
	if len(r.slack.runs) != 2 || got.New != 1 || r.tabs() != 1 {
		t.Errorf("runs %d, Fetched %+v, %d tabs; want one check and one tab", len(r.slack.runs), got, r.tabs())
	}
}

func TestFetchFailureWhileOff(t *testing.T) {
	r := newRig(t)
	r.identified(t)
	r.opts.SlackWatch = false
	r.tick()
	r.log.Reset()
	r.slack.signin = true
	r.clock.add(time.Minute)
	_, err := r.Fetch(context.Background())
	problem := "can't reach Slack. Run /mcp in Claude Code to sign in again"
	if err == nil || err.Error() != problem {
		t.Fatalf("Fetch = %v, want %q", err, problem)
	}
	if logged := r.log.String(); !strings.Contains(logged, problem+"\n") || strings.Contains(logged, "trying again") {
		t.Errorf("log = %q, want the problem without a next try", logged)
	}
	st := r.state(t)
	if st.Problem != problem || st.Failures != 1 || st.NextTry == nil || st.On {
		t.Errorf("state = %+v, want the problem and the backoff recorded", st)
	}
	if s := r.Status(); s != (Status{Text: "Slack: off", Fetch: true}) {
		t.Errorf("status = %+v, want off", s)
	}

	// An ask that didn't reach a task moves the watermark back before it.
	r.slack.signin = false
	asks := []Message{
		billing("U0DANA001", tsAt(watchStart.Add(time.Minute)), "", "<@U0ME00001> question 1"),
		billing("U0RUI0001", tsAt(watchStart.Add(2*time.Minute)), "", "<@U0ME00001> question 2"),
	}
	r.slack.mentions = asks
	r.clock.add(5 * time.Minute)
	r.herdr.fail = func(call string) error {
		if strings.HasPrefix(call, "CreateTab ") {
			return errors.New("herdr: busy")
		}
		return nil
	}
	r.log.Reset()
	if _, err := r.Fetch(context.Background()); err == nil || !strings.Contains(err.Error(), "couldn't make a task for an ask in #billing") {
		t.Fatalf("Fetch = %v, want the ask that didn't reach a task", err)
	}
	if st := r.state(t); !st.Since.Before(atTS(asks[1].TS)) || st.Since.Before(atTS(asks[0].TS)) {
		t.Errorf("since = %v, want just before the second ask", st.Since)
	}
	if strings.Contains(r.log.String(), "trying again") {
		t.Errorf("log = %q, want no next try while off", r.log.String())
	}

	// With slack_watch on in the config, a fetch that fails still names no
	// next try: no server may be running to make it.
	r.herdr.mu.Lock()
	r.herdr.fail = nil
	r.herdr.mu.Unlock()
	r.opts.SlackWatch = true
	r.slack.signin = true
	r.log.Reset()
	if _, err := r.Fetch(context.Background()); err == nil {
		t.Fatal("Fetch worked, signed out")
	}
	if logged := r.log.String(); !strings.Contains(logged, "\n"+problem+"\n") || strings.Contains(logged, "trying again") {
		t.Errorf("log = %q, want the problem without a next try", logged)
	}
	if s := r.Status(); !s.On || !s.Problem {
		t.Errorf("status = %+v, want the problem", s)
	}

	// The watcher's own failed check names it.
	r.clock.add(time.Hour)
	r.log.Reset()
	r.tick()
	if !strings.Contains(r.log.String(), problem+"; trying again at ") {
		t.Errorf("log = %q, want the next try", r.log.String())
	}
}

// TestFetchStoppedRecordsNoFailure: a fetch stopped from outside, as by
// Ctrl-C, says so, and isn't a failed check: no problem, no backoff.
func TestFetchStoppedRecordsNoFailure(t *testing.T) {
	r := newRig(t)
	r.identified(t)
	ctx, cancel := context.WithCancel(context.Background())
	run := r.Watcher.Claude
	r.Watcher.Claude = func(c context.Context, dir string, args []string, stdin string) ([]byte, error) {
		cancel()
		return run(c, dir, args, stdin)
	}
	r.clock.add(time.Minute)
	_, err := r.Fetch(ctx)
	if err == nil || err.Error() != "the fetch stopped: context canceled" {
		t.Fatalf("Fetch = %v, want it stopped", err)
	}
	if st := r.state(t); st.Problem != "" || st.Failures != 0 || st.NextTry != nil || !st.Since.Equal(watchStart) || !st.LastTry.Equal(r.clock.now()) {
		t.Errorf("state = %+v, want no failure, the watermark where it was, and the try recorded", st)
	}
	if !strings.HasSuffix(r.log.String(), "the fetch stopped: context canceled\n") {
		t.Errorf("log = %q", r.log.String())
	}
}

// TestFetchStoppedWhileWaiting: a fetch stopped while it waits for a check
// under way says it stopped, not that Slack is being checked.
func TestFetchStoppedWhileWaiting(t *testing.T) {
	r := newRig(t)
	r.identified(t)
	unlock, err := LockChecks(context.Background(), r.Dir, false)
	if err != nil {
		t.Fatal(err)
	}
	defer unlock()
	ctx, cancel := context.WithCancel(context.Background())
	time.AfterFunc(150*time.Millisecond, cancel)
	_, err = r.Fetch(ctx)
	if err == nil || err.Error() != "the fetch stopped: context canceled" {
		t.Fatalf("Fetch = %v, want it stopped", err)
	}
	if len(r.slack.runs) != 1 {
		t.Errorf("runs = %q, want no check", r.slack.runs[1:])
	}
	if st := r.state(t); st.Problem != "" || st.Failures != 0 {
		t.Errorf("state = %+v, want no failure", st)
	}
	if !strings.HasSuffix(r.log.String(), "the fetch stopped: context canceled\n") {
		t.Errorf("log = %q", r.log.String())
	}
}

func TestFetchedSummary(t *testing.T) {
	from := time.Date(2026, 10, 30, 14, 20, 0, 0, time.UTC) // a Friday
	now := time.Date(2026, 10, 31, 9, 0, 0, 0, time.UTC)
	for _, c := range []struct {
		f    Fetched
		want string
	}{
		{Fetched{From: from, New: 3, Research: 2, Joined: 1, OffTopic: 2, Unsure: 1, More: true},
			"Fetched from Fri 14:20: 3 new tasks, 2 with research; 1 added to a task; 2 off-topic (herdr-tasks slack skipped); " +
				"1 may be answered (herdr-tasks slack skipped). There's more: fetch again."},
		{Fetched{From: from, Unsure: 2}, "Fetched from Fri 14:20: 2 may be answered (herdr-tasks slack skipped)."},
		{Fetched{From: from}, "Fetched from Fri 14:20: nothing new."},
		{Fetched{From: from, New: 1}, "Fetched from Fri 14:20: 1 new task."},
		{Fetched{From: from, Joined: 2}, "Fetched from Fri 14:20: 2 added to tasks."},
		{Fetched{From: from, More: true}, "Fetched from Fri 14:20: nothing new. There's more: fetch again."},
		{Fetched{From: now.Add(-time.Hour), New: 2, Research: 2, Unsure: 0}, "Fetched from 08:00: 2 new tasks, 2 with research."},
	} {
		if got := c.f.Summary(now); got != c.want {
			t.Errorf("%+v:\n got %q\nwant %q", c.f, got, c.want)
		}
	}
}

// TestTakeDeliversASkippedAsk: taking a skipped ask makes its task, as a
// check would have, and takes it off the list. One that didn't reach a task
// stays on it.
func TestTakeDeliversASkippedAsk(t *testing.T) {
	r := newRig(t)
	r.opts.SlackTopic = billingTopic
	r.identified(t)
	lunch := dm("U0RUI0001", tsAt(watchStart.Add(time.Minute)), "lunch on Friday?")
	r.slack.dms = []Message{lunch}
	r.slack.verdicts = []verdict{{why: "lunch plans"}, {why: "coffee"}}
	r.clock.add(5 * time.Minute)
	r.tick()
	coffee := group("U0DANA001", tsAt(r.clock.now().Add(time.Minute)), "coffee later?")
	r.slack.groups = []Message{coffee}
	r.clock.add(5 * time.Minute)
	r.tick()
	if st := r.state(t); r.tabs() != 0 || len(st.Skipped) != 2 {
		t.Fatalf("%d tabs, skipped %+v; want both skipped; log:\n%s", r.tabs(), st.Skipped, r.log.String())
	}
	ctx := context.Background()
	judged := len(r.slack.judged)

	if _, err := r.Take(ctx, "9"); err == nil || err.Error() != "no skipped ask 9" || !errors.Is(err, ErrNoSkippedAsk) {
		t.Errorf("Take(9) = %v", err)
	}

	// A delivery that fails keeps the entry.
	r.herdr.fail = func(call string) error {
		if strings.HasPrefix(call, "CreateWorkspace") {
			return errors.New("herdr: busy")
		}
		return nil
	}
	if _, err := r.Take(ctx, "1"); err == nil || !strings.Contains(err.Error(), "herdr: busy") {
		t.Errorf("Take with herdr busy = %v", err)
	}
	if st := r.state(t); len(st.Skipped) != 2 || len(st.Starts) != 0 || r.tabs() != 0 {
		t.Errorf("after a failed take: %d skipped, %d starts, %d tabs; want it kept", len(st.Skipped), len(st.Starts), r.tabs())
	}
	r.herdr.mu.Lock()
	r.herdr.fail = nil
	r.herdr.mu.Unlock()

	// 1 is the first skipped: the lunch, though the coffee is newer.
	d, err := r.Take(ctx, "1")
	if err != nil || d.Tab == "" || d.FollowUp || !d.Research || d.Skipped != nil {
		t.Fatalf("Take(1) = %+v, %v; want a new tab with research", d, err)
	}
	st := r.state(t)
	if r.tabs() != 1 || len(st.Skipped) != 1 || st.Skipped[0].Ask.ID() != coffee.ID() || len(st.Starts) != 1 {
		t.Errorf("%d tabs, skipped %+v, starts %v; want the lunch's tab and start, and the coffee kept", r.tabs(), st.Skipped, st.Starts)
	}
	if tk := loadTask(t, r.store, d.Tab); tk.Slack == nil || tk.Slack.Key != lunch.Channel {
		t.Errorf("task = %+v, want the lunch's", tk.Slack)
	}
	if !strings.Contains(r.log.String(), lunch.Channel+": taken from the skipped list; new tab "+d.Tab+", with research\n") {
		t.Errorf("log = %q", r.log.String())
	}
	// 1 is no other ask's now.
	if _, err := r.Take(ctx, "1"); err == nil || err.Error() != "no skipped ask 1" {
		t.Errorf("Take(1) again = %v, want none", err)
	}
	if st := r.state(t); len(st.Skipped) != 1 || r.tabs() != 1 {
		t.Errorf("Take(1) again took %d tabs, left %+v", r.tabs(), st.Skipped)
	}

	// By permalink, with research held past slack_max_new.
	r.opts.SlackMaxNew = 1
	d, err = r.Take(ctx, coffee.Permalink)
	if err != nil || d.Tab == "" || d.Research {
		t.Errorf("Take(permalink) = %+v, %v; want a tab, research held", d, err)
	}
	if st := r.state(t); len(st.Skipped) != 0 || len(st.Starts) != 1 {
		t.Errorf("skipped %+v, starts %v", st.Skipped, st.Starts)
	}
	if _, err := r.Take(ctx, coffee.ID()); err == nil || err.Error() != "no skipped ask "+coffee.ID() {
		t.Errorf("taking it again = %v", err)
	}
	if len(r.slack.judged) != judged {
		t.Errorf("a take checked the topic")
	}

	// Take finds the session as a fetch does, and slack.log says why it
	// couldn't.
	running := r.running
	r.running = nil
	r.log.Reset()
	if _, err := r.Take(ctx, "1"); err == nil || err.Error() != "can't take it: waiting for herdr session work" {
		t.Errorf("Take with no session = %v", err)
	}
	if got := r.log.String(); got != "taking skipped ask 1, as asked\ncan't take it: waiting for herdr session work\n" {
		t.Errorf("log = %q", got)
	}
	r.running = running
}

// TestTakeIgnoresExpiredAsks: an ask skipped over 7 days ago, which the
// lists no longer show though the file still has it, can't be taken, by its
// number, link or ID. Take leaves the file as it is.
func TestTakeIgnoresExpiredAsks(t *testing.T) {
	r := newRig(t)
	r.opts.SlackTopic = billingTopic
	r.identified(t)
	lunch := dm("U0RUI0001", tsAt(watchStart.Add(time.Minute)), "lunch on Friday?")
	r.slack.dms = []Message{lunch}
	r.slack.verdicts = []verdict{{why: "lunch plans"}}
	r.clock.add(5 * time.Minute)
	r.tick()
	if st := r.state(t); len(st.Skipped) != 1 {
		t.Fatalf("skipped %+v, want the lunch", st.Skipped)
	}
	r.clock.add(7*24*time.Hour + time.Minute)
	for _, which := range []string{lunch.ID(), lunch.Permalink, "1"} {
		if _, err := r.Take(context.Background(), which); !errors.Is(err, ErrNoSkippedAsk) || err.Error() != "no skipped ask "+which {
			t.Errorf("Take(%s) = %v, want no skipped ask", which, err)
		}
	}
	if r.tabs() != 0 {
		t.Errorf("%d tabs, want none", r.tabs())
	}
	if st := r.state(t); len(st.Skipped) != 1 {
		t.Errorf("skipped %+v, want the file as it was", st.Skipped)
	}
}

// TestTakeStoppedWhileWaiting: a take stopped while it waits for a check
// under way says it stopped, not that Slack is being checked.
func TestTakeStoppedWhileWaiting(t *testing.T) {
	r := newRig(t)
	r.identified(t)
	unlock, err := LockChecks(context.Background(), r.Dir, false)
	if err != nil {
		t.Fatal(err)
	}
	defer unlock()
	ctx, cancel := context.WithCancel(context.Background())
	time.AfterFunc(150*time.Millisecond, cancel)
	if _, err := r.Take(ctx, "1"); err == nil || err.Error() != "the take stopped: context canceled" {
		t.Fatalf("Take = %v, want it stopped", err)
	}
	if !strings.HasSuffix(r.log.String(), "taking skipped ask 1, as asked\nthe take stopped: context canceled\n") {
		t.Errorf("log = %q", r.log.String())
	}
}

// TestTakeStoppedBeforeDelivering: a take stopped once it has the lock and
// the ask, before it delivers, makes nothing, and the ask stays on the list.
func TestTakeStoppedBeforeDelivering(t *testing.T) {
	r := newRig(t)
	r.opts.SlackTopic = billingTopic
	r.identified(t)
	lunch := dm("U0RUI0001", tsAt(watchStart.Add(time.Minute)), "lunch on Friday?")
	r.slack.dms = []Message{lunch}
	r.slack.verdicts = []verdict{{why: "lunch plans"}}
	r.clock.add(5 * time.Minute)
	r.tick()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	// Take reads the clock once it has the lock.
	r.Watcher.Now = func() time.Time {
		cancel()
		return r.clock.now()
	}
	r.log.Reset()
	if _, err := r.Take(ctx, "1"); err == nil || err.Error() != "the take stopped: context canceled" {
		t.Fatalf("Take = %v, want it stopped", err)
	}
	if st := r.state(t); r.tabs() != 0 || len(st.Skipped) != 1 || st.Skipped[0].Ask.ID() != lunch.ID() {
		t.Errorf("%d tabs, skipped %+v; want nothing made, and the ask kept", r.tabs(), st.Skipped)
	}
	if got := r.log.String(); got != "taking skipped ask 1, as asked\nthe take stopped: context canceled\n" {
		t.Errorf("log = %q", got)
	}
}

// TestTakeSaysWhenTheListWasntUpdated: a take whose task was made, but whose
// ask couldn't leave the skipped list, says the task was made.
func TestTakeSaysWhenTheListWasntUpdated(t *testing.T) {
	r := newRig(t)
	r.opts.SlackTopic = billingTopic
	r.identified(t)
	lunch := dm("U0RUI0001", tsAt(watchStart.Add(time.Minute)), "lunch on Friday?")
	r.slack.dms = []Message{lunch}
	r.slack.verdicts = []verdict{{why: "lunch plans"}}
	r.clock.add(5 * time.Minute)
	r.tick()
	// Once research starts, a folder takes the state file's place, so the
	// state can't be read or saved.
	stateFile := filepath.Join(r.Dir, "slack", "state.json")
	r.herdr.onStart = func(agentStart) {
		if err := os.Remove(stateFile); err != nil {
			t.Error(err)
		}
		if err := os.MkdirAll(filepath.Join(stateFile, "x"), 0o700); err != nil {
			t.Error(err)
		}
	}
	d, err := r.Take(context.Background(), "1")
	if err == nil || !strings.HasPrefix(err.Error(), "made the task in "+d.Tab+", but couldn't update the skipped list: ") || d.Tab == "" {
		t.Fatalf("Take = %+v, %v; want the task, and why the list wasn't updated", d, err)
	}
	if r.tabs() != 1 {
		t.Errorf("%d tabs, want the ask's", r.tabs())
	}
	if !strings.Contains(r.log.String(), lunch.Channel+": taken, but couldn't update the skipped list: ") {
		t.Errorf("log = %q", r.log.String())
	}
}

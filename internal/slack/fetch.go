package slack

import (
	"context"
	"errors"
	"fmt"
	"os"
	"slices"
	"strings"
	"time"

	"github.com/alekkowalczyk/herdr-tasks/internal/reconcile"
	"github.com/alekkowalczyk/herdr-tasks/internal/task"
)

// Fetched is what a fetch did. From is where its check started. New counts
// the new tabs, Research the research starts tried, Joined the follow-ups
// added to a task, and OffTopic and Unsure the asks put on the skipped list:
// those the topic check skipped, and those you may have answered already.
// More is a check that a full page, or the 20-ask cap, stopped before now.
// Cost is what its runs cost, topic checks included.
type Fetched struct {
	From                                    time.Time
	New, Research, Joined, Unsure, OffTopic int
	More                                    bool
	Cost                                    float64
}

// Summary says what the fetch did: "Fetched from Fri 14:20: 3 new tasks, 2
// with research; 1 added to a task; 2 off-topic (herdr-tasks slack
// skipped); 1 may be answered (herdr-tasks slack skipped). There's more:
// fetch again."
func (f Fetched) Summary(now time.Time) string {
	var parts []string
	if f.New > 0 {
		part := plural(f.New, "new task", "new tasks")
		if f.Research > 0 {
			part += fmt.Sprintf(", %d with research", f.Research)
		}
		parts = append(parts, part)
	}
	if f.Joined > 0 {
		parts = append(parts, plural(f.Joined, "added to a task", "added to tasks"))
	}
	if f.OffTopic > 0 {
		parts = append(parts, fmt.Sprintf("%d off-topic (herdr-tasks slack skipped)", f.OffTopic))
	}
	if f.Unsure > 0 {
		parts = append(parts, fmt.Sprintf("%d may be answered (herdr-tasks slack skipped)", f.Unsure))
	}
	did := "nothing new"
	if len(parts) > 0 {
		did = strings.Join(parts, "; ")
	}
	s := "Fetched from " + when(f.From, now) + ": " + did + "."
	if f.More {
		s += " There's more: fetch again."
	}
	return s
}

// plural is n and what it counts: "1 new task", "2 new tasks".
func plural(n int, one, many string) string {
	if n == 1 {
		return "1 " + one
	}
	return fmt.Sprintf("%d %s", n, many)
}

// Fetch runs one check now, as the watcher's, and delivers what it finds:
// outside slack_hours, before slack_every has passed, during a backoff, and
// while slack_watch is off. It waits for a check under way to end, and
// holds the lock until it's done. With no watermark, as the first time, it
// looks back 72 h. It's one check, not a loop: More says there's more.
// A check that failed is recorded as the watcher's is, with the backoff.
func (w *Watcher) Fetch(ctx context.Context) (Fetched, error) {
	w.logf("fetching, as asked")
	opts, err := w.options()
	if err != nil {
		return w.unfetched(err)
	}
	set, sess, err := w.target(opts, "can't fetch: ")
	if err != nil {
		return w.unfetched(err)
	}
	unlock, err := LockChecks(ctx, w.Dir, true)
	if err != nil {
		if ctx.Err() != nil {
			err = stopped(ctx)
		}
		return w.unfetched(err)
	}
	defer unlock()

	now := w.now()
	j := &job{on: opts.SlackWatch, fetch: true}
	st, err := Load(w.Dir)
	if err != nil {
		return w.unfetched(err)
	}
	back := now.Add(-catchUp)
	switch {
	case st.UserID == "":
		st, err = w.identify(ctx, j, set, back, now)
	case st.Since.IsZero():
		if st, err = w.save(func(s *State) bool { s.Since = back; return true }); err != nil {
			w.trouble(j, err.Error(), now)
		}
	}
	if err == nil {
		err = w.check(ctx, j, st, opts, set, sess, now)
	}
	if err != nil && ctx.Err() != nil {
		// Stopped from outside, which isn't a failed check: the watermark
		// stays, and the next check takes what this one didn't deliver.
		err = stopped(ctx)
		w.logf("%s", flat(err.Error()))
	}
	if err != nil {
		return j.got, err
	}
	w.logf("%s", j.got.Summary(now))
	return j.got, nil
}

// target is the settings, and the session asks go to, for a fetch or a
// take, or why there's none, after cant: "can't fetch: waiting for a herdr
// session". slack_cwd must be a folder.
func (w *Watcher) target(opts reconcile.Options, cant string) (Settings, Session, error) {
	home, _ := os.UserHomeDir()
	set, err := SettingsFrom(opts, home)
	if err != nil {
		return Settings{}, Session{}, err
	}
	sess, why := sessionFor(w.sessions(), set.Session)
	if why != "" {
		return Settings{}, Session{}, errors.New(cant + why)
	}
	if err := checkCwd(set.Cwd); err != nil {
		return Settings{}, Session{}, err
	}
	return set, sess, nil
}

// ErrNoSkippedAsk is a take of an ask that isn't on the skipped list.
var ErrNoSkippedAsk = errors.New("no skipped ask")

// Take makes the task for a skipped ask, which Deliver doesn't check for the
// topic again: which is its number, its permalink, or its message ID. One
// over 7 days old, which the lists no longer show, isn't on the list
// (ErrNoSkippedAsk). Taking an ask that may be answered already says it
// isn't, so its research starts as a check's would. It waits for a check
// under way to end. The ask leaves the list only once it reached a task;
// when the task was made but the list couldn't be saved, it says so.
func (w *Watcher) Take(ctx context.Context, which string) (Delivered, error) {
	w.logf("taking skipped ask %s, as asked", flat(which))
	opts, err := w.options()
	if err != nil {
		return w.untaken(err)
	}
	set, sess, err := w.target(opts, "can't take it: ")
	if err != nil {
		return w.untaken(err)
	}
	unlock, err := LockChecks(ctx, w.Dir, true)
	if err != nil {
		if ctx.Err() != nil {
			err = fmt.Errorf("the take stopped: %w", context.Cause(ctx))
		}
		return w.untaken(err)
	}
	defer unlock()
	st, err := Load(w.Dir)
	if err != nil {
		return w.untaken(err)
	}
	now := w.now()
	pick := State{Skipped: slices.Clone(st.Skipped)}
	pick.Prune(now)
	k, ok := pick.Take(which)
	if !ok {
		return Delivered{}, fmt.Errorf("%w %s", ErrNoSkippedAsk, which)
	}
	a := k.Ask
	a.Unsure = false
	research := st.CanStart(set.MaxNew, now)
	// Delivering doesn't stop midway, so a take stopped by now makes
	// nothing.
	if ctx.Err() != nil {
		return w.untaken(fmt.Errorf("the take stopped: %w", context.Cause(ctx)))
	}
	d, err := Deliver(sess.Herdr, sess.Store, opts, set, a, w.Exe, w.Dir, research, nil, now)
	if err != nil {
		w.logf("%s: no task made from the skipped list: %s", a.Key, flat(err.Error()))
		return Delivered{}, err
	}
	w.logf("%s: taken from the skipped list; %s", a.Key, delivered(d))
	if _, err := w.save(func(s *State) bool {
		s.Take(a.ID())
		if d.Research {
			s.Starts = append(s.Starts, now)
		}
		return true
	}); err != nil {
		w.logf("%s: taken, but couldn't update the skipped list: %s", a.Key, flat(err.Error()))
		did := "made the task in " + d.Tab
		if d.FollowUp {
			did = "added it to the task in " + d.Tab
		}
		return d, fmt.Errorf("%s, but couldn't update the skipped list: %w", did, err)
	}
	return d, nil
}

// TookLine says where a taken ask went, as `slack take` and the board say
// it: "Took it: new tab w2:t1, with research".
func (d Delivered) TookLine() string {
	switch {
	case d.FollowUp:
		return "Took it: added to the task in " + d.Tab
	case d.Research && d.ResearchErr != nil:
		why := strings.Join(strings.Fields(task.CleanText(d.ResearchErr.Error())), " ")
		return "Took it: new tab " + d.Tab + ", research didn't start: " + strings.TrimPrefix(why, "Research didn't start: ")
	case d.Research:
		return "Took it: new tab " + d.Tab + ", with research"
	}
	return "Took it: new tab " + d.Tab + ", research held"
}

// stopped is a fetch that ctx ended, as Ctrl-C does, and why.
func stopped(ctx context.Context) error {
	return fmt.Errorf("the fetch stopped: %w", context.Cause(ctx))
}

// unfetched logs why a fetch couldn't check, and returns it.
func (w *Watcher) unfetched(err error) (Fetched, error) {
	w.logf("%s", flat(err.Error()))
	return Fetched{}, err
}

// untaken logs why a take couldn't start, and returns it.
func (w *Watcher) untaken(err error) (Delivered, error) {
	w.logf("%s", flat(err.Error()))
	return Delivered{}, err
}

package slack

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/alekkowalczyk/herdr-tasks/internal/reconcile"
)

const (
	tickEvery = 30 * time.Second
	hourTime  = "15:04"
	dayTime   = "Mon 15:04"
)

// Status is what the watcher is doing, for the board's header. On is false
// while slack_watch is off. Fetch is whether the board offers a fetch: while
// the watcher is on, and while it's off once a check found who you are.
type Status struct {
	On, Problem, Fetch bool
	Text               string
}

// Watcher checks Slack for asks every slack_every within slack_hours, and
// delivers them to a herdr session: slack_session, or the only one running.
// It keeps its state in slack/state.json in Dir, and logs what it does.
type Watcher struct {
	Dir      string                            // the state folder
	Exe      string                            // herdr-tasks, for the guard
	Options  func() (reconcile.Options, error) // read each tick; nil: reconcile.LoadOptions
	Claude   Claude                            // nil: ExecClaude(FindClaude())
	Sessions func() []Session                  // nil: RunningSessions
	Now      func() time.Time                  // nil: time.Now
	Log      *log.Logger                       // nil: no log
	Changed  func()                            // called when Status changes; may be nil

	mu     sync.Mutex // guards status and said: a fetch may run while Run ticks
	status Status
	said   string // the line note logged last, while nothing else was logged since
}

// A job is one check, a tick's or a fetch's, and what it did. A fetch runs
// while the watcher is off too: it shows the watcher off then. A fetch's
// failed check never names a next try: no server may be running to make it.
type job struct {
	on      bool // slack_watch is on
	fetch   bool // a fetch's, not the watcher's
	got     Fetched
	judged  float64 // what the check's topic checks cost
	stopped bool    // a topic check was cut short because the check was stopped
}

// errUnchanged has save leave the state file as it is.
var errUnchanged = errors.New("unchanged")

// Run ticks now and every 30 s until ctx ends.
func (w *Watcher) Run(ctx context.Context) {
	t := time.NewTicker(tickEvery)
	defer t.Stop()
	for {
		w.Tick(ctx)
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// Status is what the last tick found.
func (w *Watcher) Status() Status {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.status
}

// Tick reads the settings and, when a check is due within the hours, checks
// Slack and delivers the asks it finds. Each way a tick can stop shows in
// the status, and goes to the log when it changes. A tick never waits for a
// fetch: while one runs, it checks nothing, and changes nothing.
func (w *Watcher) Tick(ctx context.Context) {
	now := w.now()
	opts, err := w.options()
	if err != nil {
		// A watcher that wasn't on has no log to keep.
		text := "reading config: " + flat(err.Error())
		st := OnDisk(w.Dir)
		w.set(Status{On: st.On, Problem: true, Fetch: st.On || st.UserID != "", Text: "Slack: " + text})
		if st.On {
			w.note(text)
		}
		return
	}
	if !opts.SlackWatch {
		w.turnOff()
		return
	}
	j := &job{on: true}
	home, _ := os.UserHomeDir()
	set, err := SettingsFrom(opts, home)
	if err != nil {
		w.trouble(j, err.Error(), now)
		return
	}
	st, err := w.turnOn(ctx, now)
	if errors.Is(err, ErrChecking) {
		w.set(Status{On: true, Fetch: true, Text: "Slack: a fetch is running"})
		w.note("a fetch is running")
		return
	}
	if err != nil {
		w.trouble(j, err.Error(), now)
		return
	}
	if !set.Hours.Open(now) {
		w.outside(st, set.Hours.Next(now))
		return
	}
	if !due(st, set, now) {
		w.set(statusOf(st, now))
		return
	}
	sess, why := sessionFor(w.sessions(), set.Session)
	if why != "" {
		w.trouble(j, why, now)
		return
	}
	if err := checkCwd(set.Cwd); err != nil {
		w.trouble(j, err.Error(), now)
		return
	}
	unlock, err := LockChecks(ctx, w.Dir, false)
	if errors.Is(err, ErrChecking) {
		w.note("a fetch is running")
		return
	}
	if err != nil {
		w.trouble(j, err.Error(), now)
		return
	}
	defer unlock()
	// A fetch may have checked since the state was read.
	if st, err = Load(w.Dir); err != nil {
		w.trouble(j, err.Error(), now)
		return
	}
	if !due(st, set, now) {
		w.set(statusOf(st, now))
		return
	}
	if st.UserID == "" || st.Since.IsZero() {
		_, _ = w.identify(ctx, j, set, now, now)
		return
	}
	_ = w.check(ctx, j, st, opts, set, sess, now)
}

// turnOff shows the watcher off, and records it in the state file, so
// turning it on again starts fresh. With no state file, it makes none.
func (w *Watcher) turnOff() {
	st := OnDisk(w.Dir)
	if st.On {
		if _, err := w.save(func(s *State) bool { s.On = false; return true }); err != nil {
			w.note(err.Error())
		} else {
			w.logf("turned off")
		}
	}
	w.set(offStatus(st))
}

// turnOn loads the state and, when the watcher was off, turns it on. When
// the last check, the watcher's or a fetch's, or one that was tried (a
// fetch stopped before it saved), was within 72 h, it goes on from the
// watermark, so the asks since then aren't lost. Otherwise it starts
// fresh, as the first time: the next check identifies you, and asks count
// from then on, with no backlog. Either way, a backoff
// and a problem from before are cleared. Starting fresh clears the
// watermark a fetch checks from, so it turns on only under check.lock:
// while a fetch runs, it's ErrChecking, and the next tick tries again. The
// state it returns is read again under that lock.
func (w *Watcher) turnOn(ctx context.Context, now time.Time) (State, error) {
	st, err := Load(w.Dir)
	if err != nil || st.On {
		return st, err
	}
	unlock, err := LockChecks(ctx, w.Dir, false)
	if err != nil {
		return st, err
	}
	defer unlock()
	st, err = w.save(func(s *State) bool {
		recent := func(t time.Time) bool { return !t.IsZero() && now.Sub(t) <= catchUp }
		if !recent(s.LastCheck) && !recent(s.LastTry) {
			s.Since = time.Time{}
		}
		s.On, s.LastTry, s.NextTry, s.Failures = true, time.Time{}, nil, 0
		s.Problem, s.ProblemSince = "", nil
		return true
	})
	switch {
	case err != nil:
	case st.Since.IsZero():
		w.logf("turned on")
	default:
		w.logf("turned on; asks from %s on count, as Slack was checked, or a check was tried, within 72 h", when(st.Since, now))
	}
	return st, err
}

// outside shows when the next check is. Nothing is wrong outside the hours,
// so a problem from before is cleared.
func (w *Watcher) outside(st State, next time.Time) {
	if st.Problem != "" {
		_, _ = w.save(func(s *State) bool {
			s.Problem, s.ProblemSince = "", nil
			return true
		})
	}
	text := "outside work hours; next check " + next.Format(dayTime)
	w.set(Status{On: true, Fetch: true, Text: "Slack: " + text})
	w.note(text)
}

// due reports whether a check is due: the backoff is over, and slack_every
// has passed since the last one started.
func due(st State, set Settings, now time.Time) bool {
	return (st.NextTry == nil || !now.Before(*st.NextTry)) && now.Sub(st.LastTry) >= set.Every
}

// sessionFor is the session asks go to, or why there's none.
func sessionFor(running []Session, name string) (Session, string) {
	if name != "" {
		if i := slices.IndexFunc(running, func(s Session) bool { return s.Name == name }); i >= 0 {
			return running[i], ""
		}
		return Session{}, "waiting for herdr session " + name
	}
	switch len(running) {
	case 0:
		return Session{}, "waiting for a herdr session"
	case 1:
		return running[0], ""
	}
	names := make([]string, len(running))
	for i, s := range running {
		names[i] = s.Name
	}
	slices.Sort(names)
	return Session{}, "set slack_session in config.toml; more than one herdr session runs (" + strings.Join(names, ", ") + ")"
}

// checkCwd says why slack_cwd, where research sessions start, can't be
// used, or is nil.
func checkCwd(cwd string) error {
	if fi, err := os.Stat(cwd); err != nil || !fi.IsDir() {
		return errors.New("slack_cwd " + cwd + " isn't a folder")
	}
	return nil
}

// identify runs the first check: it reads who you are, and asks count from
// start on, unless there's a watermark already. The watcher starts now, with
// no backlog; a fetch looks back 72 h. It returns the state then, or why it
// failed, which it has recorded.
func (w *Watcher) identify(ctx context.Context, j *job, set Settings, start, now time.Time) (State, error) {
	if err := w.started(j, now); err != nil {
		return State{}, err
	}
	out, err := w.run(ctx, "", time.Time{}, set.DMs)
	if ctx.Err() != nil {
		return State{}, ctx.Err()
	}
	if err == nil && out.UserID == "" {
		err = errors.New("Slack's profile didn't say who you are")
	}
	if err != nil {
		return State{}, w.failed(j, err, now, nil)
	}
	j.got.Cost += out.Cost
	st, err := w.save(func(s *State) bool {
		s.UserID = out.UserID
		if s.Since.IsZero() {
			s.Since = start
		}
		s.LastCheck, s.LastCost = now, out.Cost
		worked(s)
		return true
	})
	if err != nil {
		w.trouble(j, err.Error(), now)
		return State{}, err
	}
	w.logf("you're %s on Slack; asks from %s on count ($%.4f)", out.UserID, when(st.Since, now), out.Cost)
	w.show(j, statusOf(st, now))
	return st, nil
}

// check runs a check from the watermark, delivers the asks it finds, oldest
// first, and moves the watermark on. An ask that doesn't reach a task stops
// the delivery, and the next check, after the backoff, starts just before
// it. An ask you may have answered already, and with slack_topic an
// off-topic one, makes no task: it's seen, and kept on the skipped list.
// Only a check moves the watermark and the last check, and one runs at a
// time (LockChecks), so they're as the check started from when its asks are
// picked. What it did is in j.got; an error is why it stopped, which it has
// recorded.
func (w *Watcher) check(ctx context.Context, j *job, st State, opts reconcile.Options, set Settings, sess Session, now time.Time) error {
	from := From(st.Since, st.LastCheck, now)
	j.got.From = from
	if err := w.started(j, now); err != nil {
		return err
	}
	out, err := w.run(ctx, st.UserID, from, set.DMs)
	if ctx.Err() != nil {
		return ctx.Err()
	}
	if err == nil {
		for _, s := range out.Searches {
			for _, line := range s.Warnings {
				w.logf("%s", line)
			}
		}
	}
	var p Pick
	var cur State // mutes and seen messages as they are after the check
	if err == nil {
		if cur, err = Load(w.Dir); err == nil {
			cur.Since, cur.LastCheck = st.Since, st.LastCheck
			p, err = PickAsks(out, st.UserID, &cur, set, st.Since, now)
		}
	}
	if err != nil {
		return w.failed(j, err, now, nil)
	}
	j.got.Cost += out.Cost
	j.got.More = p.More
	w.logf("checked from %s: %d asks ($%.4f)", when(from, now), len(p.Asks), out.Cost)
	w.learn(out)
	if p.Gap != "" {
		w.logf("skipped the messages from %s: over 72 h ago", p.Gap)
	}
	if !p.Crowded.IsZero() {
		w.logf("more than %d messages at %s; skipping the rest of that second", pageSize, whenSecond(p.Crowded, now))
	}
	if len(p.Imitated) > 0 {
		w.logf("a message on this page imitates Slack's result layout; asks since %s may have been skipped: %s",
			whenSecond(p.ImitatedAfter, now), strings.Join(p.Imitated, " "))
	}
	for _, line := range p.Dropped {
		w.logf("dropped %s", line)
	}

	skip := w.skip(ctx, j, set)
	for _, a := range p.Asks {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		research := cur.CanStart(set.MaxNew, now)
		d, err := Deliver(sess.Herdr, sess.Store, opts, set, a, w.Exe, w.Dir, research, skip, now)
		if j.stopped {
			// The check was stopped during the ask's topic check: the ask
			// went nowhere and isn't seen, and the watermark stays, so the
			// next check takes it again.
			return ctx.Err()
		}
		if err != nil {
			w.logf("%s: no task made: %s", a.Key, flat(err.Error()))
			return w.failed(j, fmt.Errorf("couldn't make a task for an ask in %s: %w", place(a), err), now, func(s *State) {
				s.Since = a.At.Add(-time.Microsecond)
			})
		}
		w.logf("%s: %s", a.Key, delivered(d))
		j.count(d)
		if d.Research {
			cur.Starts = append(cur.Starts, now)
		}
		var full []Skipped
		_, err = w.save(func(s *State) bool {
			if s.Seen == nil {
				s.Seen = map[string]time.Time{}
			}
			s.Seen[a.ID()] = a.At
			if d.Skipped != nil {
				full = s.Skip(a, *d.Skipped, now)
			}
			if d.Research {
				s.Starts = append(s.Starts, now)
			}
			return true
		})
		if err != nil {
			w.trouble(j, err.Error(), now)
			return err
		}
		w.dropped(full, "over 50")
	}

	var expired []Skipped
	saved, err := w.save(func(s *State) bool {
		expired = s.Prune(now)
		if s.Seen == nil {
			s.Seen = map[string]time.Time{}
		}
		for _, id := range p.Seen {
			s.Seen[id] = posted(id, now)
		}
		s.Since, s.LastCheck, s.LastCost = p.Since, now, out.Cost+j.judged
		worked(s)
		return true
	})
	if err != nil {
		w.trouble(j, err.Error(), now)
		return err
	}
	w.dropped(expired, "over 7 days old")
	w.show(j, statusOf(saved, now))
	return nil
}

// dropped logs the asks that fell off the skipped list, and why. They're
// seen, so a check won't find them again: from now on, only slack.log has
// them.
func (w *Watcher) dropped(list []Skipped, why string) {
	for _, k := range list {
		w.logf("%s: dropped from the skipped list (%s)", k.Ask.Key, why)
	}
}

// skip is Deliver's check of whether to skip a check's ask that would make
// a task. One you may have answered already is skipped as that, without a
// topic check, which costs. With slack_topic, the others get one each.
func (w *Watcher) skip(ctx context.Context, j *job, set Settings) func(Ask) (Skip, bool) {
	offTopic := w.topicCheck(ctx, j, set)
	return func(a Ask) (Skip, bool) {
		switch {
		case a.Unsure:
			return Skip{Answered: true}, true
		case offTopic != nil:
			return offTopic(a)
		}
		return Skip{}, false
	}
}

// topicCheck is the off-topic check for a check's asks, or nil with no
// slack_topic. Each ask gets its own topic check, with the background read
// once, and its cost goes to j. A topic check that fails on its own lets
// the ask through, and slack.log says so. One cut short because ctx ended,
// as Ctrl-C, a fetch's time limit or the server stopping end it, skips the
// ask and sets j.stopped: the check delivers nothing more.
func (w *Watcher) topicCheck(ctx context.Context, j *job, set Settings) func(Ask) (Skip, bool) {
	if set.Topic == "" {
		return nil
	}
	home, _ := os.UserHomeDir()
	background := Background(set.Cwd, home)
	return func(a Ask) (Skip, bool) {
		v, err := w.judge(ctx, set.Topic, background, a)
		j.got.Cost += v.Cost
		j.judged += v.Cost
		if err != nil && ctx.Err() != nil {
			j.stopped = true
			return Skip{}, true
		}
		if err != nil {
			w.logf("%s: couldn't check the topic: %s; delivering it", a.Key, flat(err.Error()))
			return Skip{}, false
		}
		return Skip{Why: v.Why}, !v.Relevant
	}
}

// judge runs a topic check of a.
func (w *Watcher) judge(ctx context.Context, topic, background string, a Ask) (Verdict, error) {
	run, err := w.claude()
	if err != nil {
		return Verdict{}, err
	}
	return Judge(ctx, run, w.Dir, w.Exe, topic, background, a)
}

// count adds a delivered ask to what the job did.
func (j *job) count(d Delivered) {
	switch {
	case d.Skipped != nil && d.Skipped.Answered:
		j.got.Unsure++
	case d.Skipped != nil:
		j.got.OffTopic++
	case d.FollowUp:
		j.got.Joined++
	default:
		j.got.New++
		if d.Research {
			j.got.Research++
		}
	}
}

// learn saves the names of the channels a check's searches found messages
// in, for the labels of Slack links. A link works without one, so a name
// that isn't saved is only logged.
func (w *Watcher) learn(out Output) {
	var msgs []Message
	for _, s := range out.Searches {
		msgs = append(msgs, s.Messages...)
	}
	if err := LearnChannels(w.Dir, ChannelNames(msgs)); err != nil {
		w.logf("couldn't save the channels' names: %s", flat(err.Error()))
	}
}

// delivered says where an ask went, for the log.
func delivered(d Delivered) string {
	switch {
	case d.Skipped != nil && d.Skipped.Answered:
		return "may be answered already; on the skipped list"
	case d.Skipped != nil:
		return "off-topic (" + flat(d.Skipped.Why) + ")"
	case d.FollowUp:
		return "added to the task in " + d.Tab
	case d.Research && d.ResearchErr != nil:
		return "new tab " + d.Tab + ", research didn't start: " + strings.TrimPrefix(flat(d.ResearchErr.Error()), "Research didn't start: ")
	case d.Research:
		return "new tab " + d.Tab + ", with research"
	}
	return "new tab " + d.Tab + ", research held"
}

// posted is when the message with ID id was posted, from its ts; now when
// its ts isn't a Slack ts.
func posted(id string, now time.Time) time.Time {
	if i := strings.LastIndex(id, "/"); i >= 0 {
		if at, ok := tsTime(id[i+1:]); ok {
			return at
		}
	}
	return now
}

// run runs a check.
func (w *Watcher) run(ctx context.Context, me string, from time.Time, dms bool) (Output, error) {
	run, err := w.claude()
	if err != nil {
		return Output{}, err
	}
	return Check(ctx, run, w.Dir, w.Exe, me, from, dms)
}

// claude is what runs claude: w.Claude, or else the claude it finds. It
// looks each time, so one installed later is found, and refuses one too old
// for the guard's lock.
func (w *Watcher) claude() (Claude, error) {
	if w.Claude != nil {
		return w.Claude, nil
	}
	bin, err := FindClaude()
	if err != nil {
		return nil, err
	}
	if err := canLock(bin); err != nil {
		return nil, err
	}
	return ExecClaude(bin), nil
}

// started records that a check starts now.
func (w *Watcher) started(j *job, now time.Time) error {
	if _, err := w.save(func(s *State) bool { s.LastTry = now; return true }); err != nil {
		w.trouble(j, err.Error(), now)
		return err
	}
	return nil
}

// failed records a check that failed: it shows the problem and since when,
// and backs off. also, when it's set, changes the state as well. The log
// says when the watcher tries again; for a fetch's check, and while it's
// off, it doesn't. It returns err.
func (w *Watcher) failed(j *job, err error, now time.Time, also func(*State)) error {
	text := flat(err.Error())
	st, serr := w.save(func(s *State) bool {
		s.Failures++
		next := now.Add(backoff(s.Failures))
		s.NextTry = &next
		setProblem(s, text, now)
		if also != nil {
			also(s)
		}
		return true
	})
	if serr != nil {
		w.trouble(j, serr.Error(), now)
		return err
	}
	if j.on && !j.fetch {
		w.logf("%s; trying again at %s", text, when(*st.NextTry, now))
	} else {
		w.logf("%s", text)
	}
	w.show(j, statusOf(st, now))
	return err
}

// backoff is the wait after the nth failed check in a row.
func backoff(n int) time.Duration {
	switch n {
	case 1:
		return 5 * time.Minute
	case 2:
		return 10 * time.Minute
	case 3:
		return 20 * time.Minute
	}
	return 30 * time.Minute
}

// trouble shows a problem that keeps the watcher from checking, and records
// it in the state file, where the tab bar and slack status read it. It
// isn't a failed check, so nothing backs off: the next tick looks again.
func (w *Watcher) trouble(j *job, text string, now time.Time) {
	text = flat(text)
	_, _ = w.save(func(s *State) bool { return setProblem(s, text, now) })
	w.show(j, Status{On: true, Problem: true, Fetch: true, Text: "Slack: " + text})
	w.note(text)
}

// show sets the status a job found. While slack_watch is off, a fetch shows
// the watcher off, as a tick does.
func (w *Watcher) show(j *job, s Status) {
	if !j.on {
		s = offStatus(OnDisk(w.Dir))
	}
	w.set(s)
}

// offStatus is the status while slack_watch is off, with the state st. It
// offers a fetch once a check found who you are.
func offStatus(st State) Status {
	return Status{Text: "Slack: off", Fetch: st.UserID != ""}
}

// setProblem records a problem. Its since stays while the problem does. It
// reports whether anything changed.
func setProblem(s *State, text string, now time.Time) bool {
	if s.Problem == text && s.ProblemSince != nil {
		return false
	}
	s.Problem, s.ProblemSince = text, &now
	return true
}

// worked clears the problem and the backoff, after a check that worked.
func worked(s *State) {
	s.Failures, s.NextTry, s.Problem, s.ProblemSince = 0, nil, "", nil
}

// statusOf is the status the state shows: the problem and since when, or
// when the last check was.
func statusOf(st State, now time.Time) Status {
	switch {
	case st.Problem != "":
		return Status{On: true, Problem: true, Fetch: true, Text: "Slack: " + withSince(st.Problem, st.ProblemSince, now)}
	case !st.LastCheck.IsZero():
		return Status{On: true, Fetch: true, Text: "Slack: last check " + when(st.LastCheck, now)}
	}
	return Status{On: true, Fetch: true, Text: "Slack: no check yet"}
}

// withSince ends the problem's first sentence with since when:
// "can't reach Slack since 14:20. Run /mcp …".
func withSince(problem string, since *time.Time, now time.Time) string {
	if since == nil {
		return problem
	}
	at := " since " + when(*since, now)
	if i := strings.Index(problem, ". "); i >= 0 {
		return problem[:i] + at + problem[i:]
	}
	return strings.TrimSuffix(problem, ".") + at
}

// whenSecond is when, to the second: 14:32:10, or Fri 14:32:10.
func whenSecond(t, now time.Time) string {
	t = t.In(now.Location())
	if y, m, d := t.Date(); y == now.Year() && m == now.Month() && d == now.Day() {
		return t.Format(hourTime + ":05")
	}
	return t.Format(dayTime + ":05")
}

// when is t as the status shows it: 14:32 on now's day, Fri 14:32 on another.
func when(t, now time.Time) string {
	t = t.In(now.Location())
	if y, m, d := t.Date(); y == now.Year() && m == now.Month() && d == now.Day() {
		return t.Format(hourTime)
	}
	return t.Format(dayTime)
}

// OnDisk is the state file, or an empty state when there's none or it can't
// be read. It makes no file: there's none until Slack is turned on or
// fetched.
func OnDisk(dir string) State {
	if _, err := os.Stat(file(dir).Path()); err != nil {
		return State{}
	}
	st, err := Load(dir)
	if err != nil {
		return State{}
	}
	return st
}

// save changes the state file with fn, which reports whether it changed
// anything, and returns the state as it is then. Nothing is written when fn
// changed nothing.
func (w *Watcher) save(fn func(*State) bool) (State, error) {
	var st State
	err := Update(w.Dir, func(s *State) error {
		changed := fn(s)
		st = *s
		if !changed {
			return errUnchanged
		}
		return nil
	})
	if errors.Is(err, errUnchanged) {
		err = nil
	}
	return st, err
}

func (w *Watcher) set(s Status) {
	w.mu.Lock()
	changed := s != w.status
	w.status = s
	w.mu.Unlock()
	if changed && w.Changed != nil {
		w.Changed()
	}
}

// note logs line unless it's the line note logged last, with nothing logged
// since: why ticks stop is logged when it changes.
func (w *Watcher) note(line string) {
	w.mu.Lock()
	said := w.said
	w.said = line
	w.mu.Unlock()
	if line != said {
		w.print(line)
	}
}

// logf logs what happened.
func (w *Watcher) logf(format string, args ...any) {
	w.print(fmt.Sprintf(format, args...))
	w.mu.Lock()
	w.said = ""
	w.mu.Unlock()
}

func (w *Watcher) print(line string) {
	if w.Log != nil {
		w.Log.Print(line)
	}
}

func (w *Watcher) now() time.Time {
	if w.Now != nil {
		return w.Now()
	}
	return time.Now()
}

func (w *Watcher) options() (reconcile.Options, error) {
	if w.Options != nil {
		return w.Options()
	}
	return reconcile.LoadOptions()
}

func (w *Watcher) sessions() []Session {
	if w.Sessions != nil {
		return w.Sessions()
	}
	return RunningSessions()
}

// ProblemText is a problem with since when, as the board's header and
// slack status show it: "can't reach Slack since 14:20. Run /mcp …".
func ProblemText(problem string, since *time.Time, now time.Time) string {
	return withSince(problem, since, now)
}

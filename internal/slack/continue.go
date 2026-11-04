package slack

import (
	"errors"
	"fmt"
	"syscall"
	"time"

	"github.com/alekkowalczyk/herdr-tasks/internal/herdr"
	"github.com/alekkowalczyk/herdr-tasks/internal/reconcile"
	"github.com/alekkowalczyk/herdr-tasks/internal/store"
	"github.com/alekkowalczyk/herdr-tasks/internal/task"
)

// How Continue waits for the locked session to end. Tests shorten them.
var (
	pollEvery = 200 * time.Millisecond
	termWait  = 5 * time.Second // after SIGTERM
	killWait  = 5 * time.Second // after SIGKILL
)

var (
	errNoLocked    = errors.New("This tab has no locked research session")
	errContinued   = errors.New("You already continued this research session")
	errDidntStop   = errors.New("Claude didn't stop")
	errNotAtPrompt = errors.New("The pane isn't at a shell prompt; leave what's running there and try again")
)

// Continue ends a tab's locked research session and resumes the same
// conversation with your own settings and tools. It signals only a process
// whose arguments carry this task's session ID. When there's none, as when
// you quit Claude yourself, it resumes in the tab's first pane but its strip.
// A resume that launched Claude but didn't get it ready is returned as it
// is, and research is open all the same: the unlocked session is running.
// Research is recorded open only while the task's research is still the
// session Continue resumed.
func Continue(h Herdr, st *store.Store, opt reconcile.Options, tabID string, kill func(pid int, sig syscall.Signal) error, now time.Time) error {
	var id, stripPane string
	_, err := reconcile.ChangeTask(tasksOnly{h}, st, opt, now, tabID, func(t *task.Task) error {
		switch {
		case t.Slack == nil:
			return errNoAsk
		case t.Slack.Research == task.ResearchOpen:
			return errContinued
		case !t.Slack.CanContinue():
			return errNoLocked
		}
		id, stripPane = t.Slack.SessionID, t.StripPane
		return nil
	})
	if err != nil {
		return err
	}

	panes, err := WorkPanes(h, tabID, stripPane)
	if err != nil {
		return err
	}
	pane := panes[0]
	var own []herdr.Process
	for _, p := range panes {
		ps, err := h.Processes(p)
		if err != nil {
			return err
		}
		if own = ownProcesses(ps, id); len(own) > 0 {
			pane = p
			break
		}
	}
	if len(own) > 0 {
		if err := stop(h, pane, id, own, kill); err != nil {
			return err
		}
	}

	startErr := StartClaude(h, "slack-"+id[:8]+"-go", pane, []string{"--resume", id})
	if startErr != nil && !MayHaveLaunched(startErr) {
		return fmt.Errorf("%w: %v", errNotAtPrompt, startErr)
	}
	_, err = reconcile.ChangeTask(tasksOnly{h}, st, opt, now, tabID, func(t *task.Task) error {
		if t.Slack == nil || t.Slack.SessionID != id {
			return errOtherSession
		}
		t.Slack.Research = task.ResearchOpen
		t.Log = append(t.Log, task.Entry{Who: "you", At: now, Text: "Continued the research with your tools"})
		return nil
	})
	if errors.Is(err, errOtherSession) {
		err = nil
	}
	if startErr != nil {
		return startErr
	}
	return err
}

// ownProcesses are the processes that run session id: their arguments have
// --session-id id or --session-id=id. Nothing else is ever signalled.
func ownProcesses(ps []herdr.Process, id string) []herdr.Process {
	var own []herdr.Process
	for _, p := range ps {
		for i, a := range p.Argv {
			if a == "--session-id="+id || (a == "--session-id" && i+1 < len(p.Argv) && p.Argv[i+1] == id) {
				own = append(own, p)
				break
			}
		}
	}
	return own
}

// stop ends the session's processes: SIGTERM, then SIGKILL if they last.
func stop(h Herdr, pane, id string, procs []herdr.Process, kill func(int, syscall.Signal) error) error {
	for _, step := range []struct {
		sig  syscall.Signal
		wait time.Duration
	}{{syscall.SIGTERM, termWait}, {syscall.SIGKILL, killWait}} {
		for _, p := range procs {
			// A process that's already gone is what we want.
			if err := kill(p.PID, step.sig); err != nil && !errors.Is(err, syscall.ESRCH) {
				return fmt.Errorf("Couldn't stop Claude: %w", err)
			}
		}
		left, err := waitGone(h, pane, id, step.wait)
		if err != nil {
			return err
		}
		if len(left) == 0 {
			return nil
		}
		procs = left
	}
	return errDidntStop
}

// waitGone polls the pane until the session's processes are gone or wait is
// over, and returns those left.
func waitGone(h Herdr, pane, id string, wait time.Duration) ([]herdr.Process, error) {
	deadline := time.Now().Add(wait)
	for {
		ps, err := h.Processes(pane)
		if err != nil {
			return nil, err
		}
		left := ownProcesses(ps, id)
		if len(left) == 0 || !time.Now().Before(deadline) {
			return left, nil
		}
		time.Sleep(pollEvery)
	}
}

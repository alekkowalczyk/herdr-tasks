// Package reopen opens a task again after its tab closed: a new tab in its
// workspace and folder, with the task as it was, and its Claude session
// resumed there.
package reopen

import (
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/alekkowalczyk/herdr-tasks/internal/claudelog"
	"github.com/alekkowalczyk/herdr-tasks/internal/herdr"
	"github.com/alekkowalczyk/herdr-tasks/internal/reconcile"
	"github.com/alekkowalczyk/herdr-tasks/internal/slack"
	"github.com/alekkowalczyk/herdr-tasks/internal/store"
	"github.com/alekkowalczyk/herdr-tasks/internal/task"
)

// Herdr is what reopening needs of herdr. strip.Host has it all.
type Herdr interface {
	reconcile.Herdr
	slack.AgentStarter
	CreateWorkspace(label, cwd string) (herdr.Created, error)
	CreateTab(workspaceID, label, cwd string) (herdr.Created, error)
	CloseTab(tabID string) error
}

// ErrGone is why a closed task can't be reopened: it was reopened already,
// or it's older than the closed tasks the state keeps.
var ErrGone = errors.New("That closed task isn't kept any more: it was reopened, or closed too long ago")

// lockName is the lock Task holds, so one closed task is reopened once.
const lockName = "reopen"

// Reopened is the tab a closed task is in again.
type Reopened struct {
	Tab string
	// Claude resumed the task's session.
	Resumed bool
	// The task's folder, when it's gone: the tab opens where herdr opens
	// new tabs, and Claude isn't resumed, as it finds a session only in
	// its own folder.
	Gone string
}

// Say is what to tell you about it, for the task called name.
func (r Reopened) Say(name string) string {
	switch {
	case r.Resumed:
		return "Reopened " + name + " and resumed its Claude session"
	case r.Gone != "":
		return "Reopened " + name + ". Its folder " + r.Gone + " is gone, so its Claude session wasn't resumed"
	}
	return "Reopened " + name
}

// Task reopens the closed task id: it makes a tab in the task's workspace,
// or a workspace of that name when there's none, in the folder its Claude
// session ran in, or else the one its tab worked in. The task goes there as
// it was, and Claude resumes its session there, once it has a log. When the
// tab is there but Claude didn't start, it returns the tab with the error.
// claudeDir is Claude Code's config folder, which has the session logs.
func Task(h Herdr, st *store.Store, opt reconcile.Options, claudeDir, id string, now time.Time) (Reopened, error) {
	unlock, err := st.Hold(lockName)
	if err != nil {
		return Reopened{}, err
	}
	defer unlock()
	s, err := st.Load()
	if err != nil {
		return Reopened{}, err
	}
	c, ok := s.ClosedTask(id)
	if !ok {
		return Reopened{}, ErrGone
	}
	var r Reopened
	session, cwd := where(claudeDir, c.Task)
	if cwd != "" {
		if fi, err := os.Stat(cwd); err != nil || !fi.IsDir() {
			r.Gone, cwd, session = cwd, "", ""
		}
	}
	live, err := h.Live()
	if err != nil {
		return Reopened{}, err
	}
	made, err := makeTab(h, live, c.Task.Workspace, c.Task.Title, cwd)
	if err != nil {
		return Reopened{}, fmt.Errorf("couldn't make its tab: %w", err)
	}
	r.Tab = made.TabID
	_, err = reconcile.Run(h, st, opt, now, func(s *store.State, _ *herdr.Live) error {
		fresh, ok := s.Tasks[made.TabID]
		if !ok {
			return fmt.Errorf("herdr doesn't list the new tab %s", made.TabID)
		}
		t, ok := s.Unclose(id)
		if !ok {
			return ErrGone
		}
		// What herdr and this run gave the new tab stays; the rest is the task.
		t.TabID, t.Workspace, t.Applied, t.StripPane = made.TabID, fresh.Workspace, fresh.Applied, fresh.StripPane
		t.Log = append(t.Log, task.Entry{Who: "you", At: now, Text: "Reopened"})
		s.Tasks[made.TabID] = t
		return nil
	})
	if err != nil && !reopened(st, made.TabID, id) {
		_ = h.CloseTab(made.TabID)
		return Reopened{}, err
	}
	// Reopened, the tab is the task's: what went wrong was elsewhere, as a
	// strip that couldn't be added to another tab.
	if session == "" {
		return r, nil
	}
	err = slack.StartClaude(h, "task-"+session[:8], made.PaneID, []string{"--resume", session})
	if err != nil && !slack.MayHaveLaunched(err) {
		return r, fmt.Errorf("Claude didn't resume its session: %w", err)
	}
	r.Resumed = true
	// A Slack task's locked research runs with your own tools now.
	_, cerr := reconcile.ChangeTask(h, st, opt, now, made.TabID, func(t *task.Task) error {
		if a := t.Slack; a != nil && a.Research == task.ResearchLocked && a.SessionID == session {
			a.Research = task.ResearchOpen
		}
		return nil
	})
	if err != nil {
		return r, fmt.Errorf("Claude may not have finished resuming its session: %w", err)
	}
	return r, cerr
}

// where is the Claude session to resume for t, the one herdr last saw in its
// tab once it has a log, and the folder to open: the one that session ran
// in, or else the one its tab last worked in.
func where(claudeDir string, t *task.Task) (session, cwd string) {
	cwd = t.Cwd
	if t.Claude == "" {
		return "", cwd
	}
	log, err := claudelog.Path(claudeDir, t.Claude)
	if err != nil || log == "" {
		return "", cwd
	}
	if ran, err := claudelog.Cwd(log); err == nil && ran != "" {
		cwd = ran
	}
	return t.Claude, cwd
}

// makeTab makes the tab, called title, in the first workspace called label,
// in any case, or in a new one.
func makeTab(h Herdr, live *herdr.Live, label, title, cwd string) (herdr.Created, error) {
	for _, w := range live.Workspaces {
		if strings.EqualFold(w.Label, label) {
			return h.CreateTab(w.ID, title, cwd)
		}
	}
	c, err := h.CreateWorkspace(label, cwd)
	if err == nil && title != "" {
		// A workspace comes with a tab herdr names. With stage markers off,
		// only the label keeps the task's title.
		_ = h.RenameTab(c.TabID, title)
	}
	return c, err
}

// reopened reports whether the state has the closed task id in tab.
func reopened(st *store.Store, tab, id string) bool {
	s, err := st.Load()
	if err != nil {
		return false
	}
	_, closed := s.ClosedTask(id)
	_, ok := s.Tasks[tab]
	return ok && !closed
}

// Package move moves a task, with its panes, to another workspace.
package move

import (
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/alekkowalczyk/herdr-tasks/internal/herdr"
	"github.com/alekkowalczyk/herdr-tasks/internal/reconcile"
	"github.com/alekkowalczyk/herdr-tasks/internal/store"
)

// Herdr is the part of the herdr API a move needs.
type Herdr interface {
	reconcile.Herdr
	Layout(tabID string) (*herdr.Node, error)
	MovePaneToNewTab(paneID, workspaceID, label string) (herdr.Moved, error)
	MovePaneNextTo(paneID, tabID, target, split string, ratio float64) (herdr.Moved, error)
	CloseTab(tabID string) error
	Panes() ([]herdr.Pane, error)
}

// maxAliases is how many old pane IDs the state keeps.
const maxAliases = 200

// Workspace finds a workspace by ID, or by label in any case.
func Workspace(live *herdr.Live, which string) (herdr.Workspace, error) {
	for _, w := range live.Workspaces {
		if w.ID == which {
			return w, nil
		}
	}
	for _, w := range live.Workspaces {
		if strings.EqualFold(w.Label, which) {
			return w, nil
		}
	}
	labels := make([]string, 0, len(live.Workspaces))
	for _, w := range live.Workspaces {
		labels = append(labels, w.Label)
	}
	return herdr.Workspace{}, fmt.Errorf("No workspace %q; there are: %s", which, strings.Join(labels, ", "))
}

// Task moves a tab's task, with its panes, to another workspace of the same
// herdr session, and returns its new tab.
//
// The old tab closes last, once every pane has moved, the task is saved
// under its new tab, and reconcile has given that tab its strip: the strip
// in the old tab may be what's running this, and closing its tab ends it.
//
// herdr gives a moved pane a new ID, and the processes in it keep the old
// one in their env. The state keeps each old ID with its new one, in
// PaneAliases, so they still find their pane.
func Task(h Herdr, st *store.Store, opt reconcile.Options, now time.Time, tabID, to string) (string, error) {
	var newTab string
	var moveErr error
	removed := false
	err := st.Update(func(s *store.State) error {
		removed = s.Removed
		live, err := h.Live()
		if err != nil {
			return err
		}
		t, ok := s.Tasks[tabID]
		if !ok {
			return fmt.Errorf("no tab %s", tabID)
		}
		ws, err := Workspace(live, to)
		if err != nil {
			return err
		}
		if tab, ok := live.Tab(tabID); ok && tab.WorkspaceID == ws.ID {
			return fmt.Errorf("It's already in %s", ws.Label)
		}
		root, err := h.Layout(tabID)
		if err != nil {
			return err
		}
		var panes []string
		for _, p := range root.Leaves() {
			if p != t.StripPane {
				panes = append(panes, p)
			}
		}
		if len(panes) == 0 {
			return fmt.Errorf("That tab has no panes to move")
		}

		label := t.Name(live.Position(tabID))
		first, err := h.MovePaneToNewTab(panes[0], ws.ID, label)
		if err != nil {
			return err
		}
		newTab = first.TabID
		last := first.PaneID
		renamed := map[string]string{panes[0]: first.PaneID}
		moved := 1
		for _, p := range panes[1:] {
			m, err := h.MovePaneNextTo(p, newTab, last, "right", 0.5)
			if err != nil {
				moveErr = fmt.Errorf("Moved %d of %d panes; the rest are still in the old tab: %w", moved, len(panes), err)
				break
			}
			last = m.PaneID
			renamed[p] = m.PaneID
			moved++
		}
		keepAliases(h, s, renamed)

		// The task goes with its first pane, whatever happens to the rest.
		delete(s.Tasks, tabID)
		t.TabID = newTab
		t.Workspace = ws.Label
		t.Applied = label // what we named the tab, so it isn't read as your rename
		t.StripPane = ""
		t.StripOff = false
		s.Tasks[newTab] = t
		return nil
	})
	if err != nil {
		return "", err
	}

	closeOld := moveErr == nil
	var mutate reconcile.Mutate
	if closeOld && !removed {
		// Reconcile finds the old tab, now holding only its strip, as a new
		// tab; it isn't a task, and it closes next.
		mutate = func(s *store.State, _ *herdr.Live) error {
			delete(s.Tasks, tabID)
			return nil
		}
	}
	_, recErr := reconcile.Run(h, st, opt, now, mutate)
	if closeOld {
		if err := h.CloseTab(tabID); err != nil {
			moveErr = err
		}
	}
	if recErr != nil && moveErr == nil {
		return newTab, recErr
	}
	return newTab, moveErr
}

// keepAliases adds the panes a move renamed, old ID to new, to s's aliases.
// An alias to a pane that was renamed again now names its new ID, so one
// step finds a pane. Those whose pane is gone are dropped, and so is an old
// ID herdr uses again; when herdr can't list its panes, those wait for the
// next move. Past maxAliases, the others go before this move's, in the order
// of their old IDs.
func keepAliases(h Herdr, s *store.State, renamed map[string]string) {
	if s.PaneAliases == nil {
		s.PaneAliases = map[string]string{}
	}
	for old, now := range s.PaneAliases {
		if n, ok := renamed[now]; ok {
			s.PaneAliases[old] = n
		}
	}
	for old, now := range renamed {
		s.PaneAliases[old] = now
	}
	if panes, err := h.Panes(); err == nil {
		live := map[string]bool{}
		for _, p := range panes {
			live[p.ID] = true
		}
		for old, now := range s.PaneAliases {
			if !live[now] || live[old] {
				delete(s.PaneAliases, old)
			}
		}
	}
	if over := len(s.PaneAliases) - maxAliases; over > 0 {
		var others []string
		for old := range s.PaneAliases {
			if _, ok := renamed[old]; !ok {
				others = append(others, old)
			}
		}
		slices.Sort(others)
		for _, old := range others[:min(over, len(others))] {
			delete(s.PaneAliases, old)
		}
	}
	if len(s.PaneAliases) == 0 {
		s.PaneAliases = nil
	}
}

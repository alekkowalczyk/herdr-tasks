// Package reconcile brings herdr in line with the stored tasks: it tracks new
// tabs, drops closed ones, applies stage markers and ordering to the tab bar,
// and reports per-space counts for the sidebar.
package reconcile

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/BurntSushi/toml"

	"github.com/alekkowalczyk/herdr-tasks/internal/herdr"
	"github.com/alekkowalczyk/herdr-tasks/internal/store"
	"github.com/alekkowalczyk/herdr-tasks/internal/task"
)

// Herdr is the part of the herdr API reconciliation needs.
type Herdr interface {
	Live() (*herdr.Live, error)
	RenameTab(id, label string) error
	MoveTab(id string, from, to int) ([]herdr.Tab, error)
	ReportWorkspaceTokens(workspaceID string, tokens map[string]*string) error
}

type Options struct {
	// Put a stage marker in front of each tab name.
	Markers bool `toml:"markers"`
	// Order tabs active → paused → parked → done.
	SortTabs bool `toml:"sort_tabs"`
}

func ConfigDir() string {
	if d := os.Getenv("HERDR_PLUGIN_CONFIG_DIR"); d != "" {
		return d
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".config", "herdr", "plugins", "config", store.PluginID)
}

// LoadOptions reads config.toml from the plugin's config directory.
func LoadOptions() (Options, error) {
	opt := Options{Markers: true, SortTabs: true}
	path := filepath.Join(ConfigDir(), "config.toml")
	if _, err := toml.DecodeFile(path, &opt); err != nil && !errors.Is(err, os.ErrNotExist) {
		return opt, err
	}
	return opt, nil
}

// Event is the herdr event that triggered a hook, if any.
type Event struct {
	Name  string
	TabID string
	Label string
}

func EventFromEnv() *Event {
	name := os.Getenv("HERDR_PLUGIN_EVENT")
	if name == "" {
		return nil
	}
	ev := &Event{Name: name}
	var payload struct {
		Data struct {
			TabID string `json:"tab_id"`
			Label string `json:"label"`
			Tab   struct {
				TabID string `json:"tab_id"`
				Label string `json:"label"`
			} `json:"tab"`
		} `json:"data"`
	}
	if json.Unmarshal([]byte(os.Getenv("HERDR_PLUGIN_EVENT_JSON")), &payload) == nil {
		ev.TabID, ev.Label = payload.Data.TabID, payload.Data.Label
		if ev.TabID == "" {
			ev.TabID, ev.Label = payload.Data.Tab.TabID, payload.Data.Tab.Label
		}
	}
	return ev
}

// Mutate changes the state while reconciliation holds the lock. Every live
// tab already has a task when it runs.
type Mutate func(s *store.State, live *herdr.Live) error

// Run reconciles herdr with the stored tasks and returns what herdr looked
// like afterwards.
func Run(h Herdr, st *store.Store, opt Options, now time.Time, mutate Mutate) (*herdr.Live, error) {
	var live *herdr.Live
	err := st.Update(func(s *store.State) error {
		var err error
		if live, err = h.Live(); err != nil {
			return err
		}
		if s.Removed {
			if mutate != nil {
				return errors.New("herdr-tasks was removed; run `herdr-tasks setup` to turn it back on")
			}
			return nil
		}
		Track(s, live, now)
		if mutate != nil {
			if err := mutate(s, live); err != nil {
				return err
			}
		}
		if opt.SortTabs {
			if err := sortTabs(h, s, live); err != nil {
				return err
			}
		}
		if err := applyLabels(h, s, live, opt.Markers); err != nil {
			return err
		}
		return reportTokens(h, s, live)
	})
	return live, err
}

// Clear undoes what the plugin shows in herdr: tab names get their plain
// names back and sidebar counts go away. Tasks stay on disk. Until Enable,
// later runs (such as hooks still in flight) change nothing.
func Clear(h Herdr, st *store.Store) error {
	return st.Update(func(s *store.State) error {
		s.Removed = true
		live, err := h.Live()
		if err != nil {
			return err
		}
		if err := applyLabels(h, s, live, false); err != nil {
			return err
		}
		for _, w := range live.Workspaces {
			// No stages clears every token.
			if err := h.ReportWorkspaceTokens(w.ID, Tokens(nil)); err != nil {
				return err
			}
		}
		return nil
	})
}

// Enable turns the plugin back on after Clear.
func Enable(st *store.Store) error {
	return st.Update(func(s *store.State) error {
		s.Removed = false
		return nil
	})
}

// Track adds new tabs as active tasks, notices tabs you renamed, and drops
// tasks whose tabs are gone.
func Track(s *store.State, live *herdr.Live, now time.Time) {
	wsLabel := map[string]string{}
	for _, w := range live.Workspaces {
		wsLabel[w.ID] = w.Label
	}

	var untracked []herdr.Tab
	for _, tab := range live.Tabs {
		t, ok := s.Tasks[tab.ID]
		if !ok {
			untracked = append(untracked, tab)
			continue
		}
		// A label we didn't set means you renamed the tab.
		if tab.Label != t.Applied {
			t.Title = task.TitleFromLabel(tab.Label, live.Position(tab.ID))
			t.Applied = ""
		}
		t.Workspace = wsLabel[tab.WorkspaceID]
	}

	ids := make([]string, 0, len(s.Tasks))
	for id := range s.Tasks {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		if _, ok := live.Tab(id); ok {
			continue
		}
		t := s.Tasks[id]
		delete(s.Tasks, id)
		// herdr may hand a tab a new ID; keep the task if a new tab in the same
		// space carries the same name.
		for i, tab := range untracked {
			if t.Title != "" && wsLabel[tab.WorkspaceID] == t.Workspace &&
				task.TitleFromLabel(tab.Label, live.Position(tab.ID)) == t.Title {
				t.TabID, t.Applied = tab.ID, ""
				s.Tasks[tab.ID] = t
				untracked = append(untracked[:i], untracked[i+1:]...)
				break
			}
		}
	}

	for _, tab := range untracked {
		s.Tasks[tab.ID] = &task.Task{
			TabID:     tab.ID,
			Workspace: wsLabel[tab.WorkspaceID],
			Title:     task.TitleFromLabel(tab.Label, live.Position(tab.ID)),
			Stage:     task.Active,
			Since:     now,
		}
	}
}

func stageOf(s *store.State, tabID string) task.Stage {
	if t, ok := s.Tasks[tabID]; ok {
		return t.Stage
	}
	return task.Active
}

// sortTabs orders each workspace's tabs by stage, keeping your order within a
// stage, and updates live to match.
func sortTabs(h Herdr, s *store.State, live *herdr.Live) error {
	var all []herdr.Tab
	for _, w := range live.Workspaces {
		cur := live.TabsIn(w.ID)
		want := append([]herdr.Tab(nil), cur...)
		sort.SliceStable(want, func(i, j int) bool {
			return stageOf(s, want[i].ID).Order() < stageOf(s, want[j].ID).Order()
		})
		for to, tab := range want {
			from := indexOf(cur, tab.ID)
			if from == to {
				continue
			}
			moved, err := h.MoveTab(tab.ID, from, to)
			if err != nil {
				return err
			}
			if len(moved) == len(cur) {
				cur = moved
			} else {
				cur = append(cur[:from:from], cur[from+1:]...)
				cur = append(cur[:to], append([]herdr.Tab{tab}, cur[to:]...)...)
			}
		}
		all = append(all, cur...)
	}
	// Keep any tab whose workspace wasn't listed.
	for _, tab := range live.Tabs {
		if _, ok := live.Workspace(tab.WorkspaceID); !ok {
			all = append(all, tab)
		}
	}
	live.Tabs = all
	return nil
}

func indexOf(tabs []herdr.Tab, id string) int {
	for i, t := range tabs {
		if t.ID == id {
			return i
		}
	}
	return -1
}

// applyLabels puts each tab's stage marker in its name, or takes the markers
// off again when they're turned off.
func applyLabels(h Herdr, s *store.State, live *herdr.Live, markers bool) error {
	for i, tab := range live.Tabs {
		t, ok := s.Tasks[tab.ID]
		if !ok {
			continue
		}
		pos := live.Position(tab.ID)
		var want string
		switch {
		case markers:
			want = task.MarkedLabel(t.Stage, t.Title, pos)
		case t.Applied != "":
			want = t.Title
			if want == "" {
				want = strconv.Itoa(pos)
			}
		default:
			continue
		}
		if tab.Label != want {
			if err := h.RenameTab(tab.ID, want); err != nil {
				return err
			}
			live.Tabs[i].Label = want
		}
		if markers {
			t.Applied = want
		} else {
			t.Applied = ""
		}
	}
	return nil
}

// Tokens are the sidebar values for one space: "● 2 · ‖ 1 · ○ 1" as $tasks,
// plus one token per stage so each can get its own color.
func Tokens(stages []task.Stage) map[string]*string {
	counts := map[task.Stage]int{}
	for _, st := range stages {
		counts[st]++
	}
	tokens := map[string]*string{}
	var parts []string
	for _, st := range task.Stages {
		key := "tasks_" + string(st)
		if counts[st] == 0 {
			tokens[key] = nil
			continue
		}
		v := st.Glyph() + " " + strconv.Itoa(counts[st])
		tokens[key] = &v
		if st != task.Done {
			parts = append(parts, v)
		}
	}
	if len(parts) == 0 {
		tokens["tasks"] = nil
	} else {
		v := strings.Join(parts, " · ")
		tokens["tasks"] = &v
	}
	return tokens
}

func reportTokens(h Herdr, s *store.State, live *herdr.Live) error {
	for _, w := range live.Workspaces {
		var stages []task.Stage
		for _, tab := range live.TabsIn(w.ID) {
			stages = append(stages, stageOf(s, tab.ID))
		}
		if err := h.ReportWorkspaceTokens(w.ID, Tokens(stages)); err != nil {
			return err
		}
	}
	return nil
}

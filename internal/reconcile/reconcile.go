// Package reconcile brings herdr in line with the stored tasks: it tracks new
// tabs, drops closed ones, applies stage markers and ordering to the tab bar,
// and reports per-space counts for the sidebar.
package reconcile

import (
	"encoding/json"
	"errors"
	"fmt"
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
	// Order tabs active → in review → paused → parked → done.
	SortTabs bool `toml:"sort_tabs"`
	// Show each task's stage, note, and links in a strip along the bottom of its tab.
	Strip bool `toml:"strip"`

	// Set for the run after herdr starts, when strips need restarting.
	Startup bool `toml:"-"`
	// The strip pane running this, if any. It's never closed from here: the
	// strip saves first and then exits on its own.
	Self string `toml:"-"`
}

// Strips is implemented by hosts that can show strips (see package strip).
type Strips interface {
	Panes() ([]herdr.Pane, error)
	Open(tabID string) (string, error)
	Close(paneID string) error
	CloseTab(tabID string) error
	Relaunch(paneID string) error
	Rename(paneID, label string) error
}

// StripLabel and ParkingLabel match package strip; they're repeated here so
// reconciliation doesn't depend on it. Strips used to be labeled "Task".
const (
	StripLabel    = "Task strip"
	oldStripLabel = "Task"
	ParkingLabel  = "herdr-tasks parking"
)

func ConfigDir() string {
	if d := os.Getenv("HERDR_PLUGIN_CONFIG_DIR"); d != "" {
		return d
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".config", "herdr", "plugins", "config", store.PluginID)
}

// LoadOptions reads config.toml from the plugin's config directory.
func LoadOptions() (Options, error) {
	opt := Options{Markers: true, SortTabs: true, Strip: true}
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
	// A strip that couldn't be added shouldn't lose the rest of the run.
	var stripErr error
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
		sh, strips := h.(Strips)
		if strips {
			// Before your change, which can change which tabs want a strip.
			if panes, err := sh.Panes(); err == nil {
				noticeGoneStrips(s, live, panes)
			}
		}
		if mutate != nil {
			if err := mutate(s, live); err != nil {
				return err
			}
		}
		if strips {
			stripErr = syncStrips(sh, s, live, opt)
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
	if err == nil {
		err = stripErr
	}
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
		if sh, ok := h.(Strips); ok {
			for _, t := range s.Tasks {
				if t.StripPane != "" {
					_ = sh.Close(t.StripPane)
					t.StripPane = ""
				}
			}
		}
		for _, w := range live.Workspaces {
			// No stages and no rows clears every token.
			tokens := Tokens(nil)
			for k, v := range ListTokens(s, live, nil, false) {
				tokens[k] = v
			}
			if err := reportInChunks(h, w.ID, tokens); err != nil {
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
		if task.StripMarker(tab.Label) == ParkingLabel {
			continue // a pane waiting while a strip is added, not a task
		}
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
		if !st.IsTask() {
			continue // tool tabs aren't work
		}
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
		var tabs []herdr.Tab
		for _, tab := range live.TabsIn(w.ID) {
			stages = append(stages, stageOf(s, tab.ID))
			tabs = append(tabs, tab)
		}
		tokens := Tokens(stages)
		for k, v := range ListTokens(s, live, tabs, !s.SidebarCounts) {
			tokens[k] = v
		}
		if !s.SidebarCounts {
			// The list shows active, in-review, and priority tasks; the
			// counts line keeps only what the list hides, as "1 parked"
			// rather than "○ 1", which reads like a tab named 1.
			for _, st := range []task.Stage{task.Active, task.Review} {
				tokens["tasks_"+string(st)] = nil
			}
			counts := map[task.Stage]int{}
			for _, tab := range tabs {
				if t, ok := s.Tasks[tab.ID]; ok && t.Priority {
					continue
				}
				counts[stageOf(s, tab.ID)]++
			}
			for _, st := range []task.Stage{task.Paused, task.Parked, task.Done} {
				tokens["tasks_"+string(st)] = nil
				if counts[st] > 0 {
					v := strconv.Itoa(counts[st]) + " " + strings.ToLower(st.Label())
					tokens["tasks_"+string(st)] = &v
				}
			}
		}
		if err := reportInChunks(h, w.ID, tokens); err != nil {
			return err
		}
	}
	return nil
}

// reportInChunks sends tokens in reports of at most 16, herdr's limit.
func reportInChunks(h Herdr, workspaceID string, tokens map[string]*string) error {
	keys := make([]string, 0, len(tokens))
	for k := range tokens {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for len(keys) > 0 {
		n := min(16, len(keys))
		chunk := map[string]*string{}
		for _, k := range keys[:n] {
			chunk[k] = tokens[k]
		}
		if err := h.ReportWorkspaceTokens(workspaceID, chunk); err != nil {
			return err
		}
		keys = keys[n:]
	}
	return nil
}

// ListRows is how many task rows a space shows in the sidebar.
const ListRows = 7

// listHidden are the stages the sidebar list leaves out.
var listHidden = map[task.Group]bool{task.GPaused: true, task.GParked: true, task.GDone: true, task.GTool: true}

// ListTokens are the sidebar rows listing a space's tasks, priority tasks
// first, then most urgent first: $tN_ask for an agent asking, $tN_you when
// it's your turn, $tN_run while an agent works, $tN_pri for a priority task
// ("! ‖ name"), and $tN for the rest, so each can get its own color. Paused, parked, done,
// and tool tabs are left out unless they're a priority. With show false
// every row is cleared.
func ListTokens(s *store.State, live *herdr.Live, tabs []herdr.Tab, show bool) map[string]*string {
	type row struct {
		g    task.Group
		pri  bool
		text string
	}
	var rows []row
	if show {
		for _, tab := range tabs {
			t, ok := s.Tasks[tab.ID]
			if !ok {
				continue
			}
			g := task.GroupOf(t.Stage, tab.AgentStatus)
			if listHidden[g] && !t.Priority {
				continue
			}
			glyph := t.Stage.Glyph()
			switch g {
			case task.Asking:
				glyph = "?"
			case task.Running:
				// Color alone can't show it: a priority row is all red.
				glyph = "↻"
			}
			rows = append(rows, row{g, t.Priority, glyph + " " + t.Name(live.Position(tab.ID))})
		}
		order := map[task.Group]int{}
		for i, g := range task.Groups {
			order[g] = i
		}
		sort.SliceStable(rows, func(i, j int) bool {
			if rows[i].pri != rows[j].pri {
				return rows[i].pri
			}
			return order[rows[i].g] < order[rows[j].g]
		})
		if len(rows) > ListRows {
			more := fmt.Sprintf("+%d more", len(rows)-ListRows+1)
			rows = append(rows[:ListRows-1], row{task.GDone, false, more})
		}
	}
	tokens := map[string]*string{}
	for i := 1; i <= ListRows; i++ {
		base := "t" + strconv.Itoa(i)
		for _, k := range []string{"_pri", "_ask", "_you", "_run", ""} {
			tokens[base+k] = nil
		}
		if i > len(rows) {
			continue
		}
		v := rows[i-1].text
		if rows[i-1].pri {
			// In the row's own token: herdr puts " · " between two tokens.
			v = "! " + v
		}
		switch {
		case rows[i-1].g == task.Asking:
			tokens[base+"_ask"] = &v
		case rows[i-1].pri:
			tokens[base+"_pri"] = &v
		case rows[i-1].g == task.YourTurn:
			tokens[base+"_you"] = &v
		case rows[i-1].g == task.Running:
			tokens[base+"_run"] = &v
		default:
			tokens[base] = &v
		}
	}
	return tokens
}

// RefreshTokens updates the sidebar only, for when an agent's state changes.
func RefreshTokens(h Herdr, st *store.Store) error {
	s, err := st.Load()
	if err != nil || s.Removed {
		return err
	}
	live, err := h.Live()
	if err != nil {
		return err
	}
	return reportTokens(h, s, live)
}

// noticeGoneStrips handles strips that went away since the last run. If the
// tab still wanted one, you closed it, which hides the strip for that tab.
// Otherwise it closed itself, say after the tab became a tool.
func noticeGoneStrips(s *store.State, live *herdr.Live, panes []herdr.Pane) {
	tabOf := map[string]string{}
	for _, p := range panes {
		tabOf[p.ID] = p.TabID
	}
	for _, tab := range live.Tabs {
		t, ok := s.Tasks[tab.ID]
		if !ok || t.StripPane == "" || tabOf[t.StripPane] == tab.ID {
			continue
		}
		t.StripPane = ""
		if t.WantsStrip() {
			t.SetStrip(false)
		}
	}
}

// syncStrips gives every task a strip unless you turned it off, notices
// strips you closed, restarts strips after herdr restarts, and closes tabs
// where only a strip is left.
func syncStrips(sh Strips, s *store.State, live *herdr.Live, opt Options) error {
	panes, err := sh.Panes()
	if err != nil {
		return err
	}
	byID := map[string]herdr.Pane{}
	byTab := map[string][]herdr.Pane{}
	for _, p := range panes {
		byID[p.ID] = p
		byTab[p.TabID] = append(byTab[p.TabID], p)
	}

	var firstErr error
	for _, tab := range live.Tabs {
		t, ok := s.Tasks[tab.ID]
		if !ok || tab.Label == ParkingLabel {
			continue
		}
		if p, ok := byID[t.StripPane]; t.StripPane != "" && (!ok || p.TabID != tab.ID) {
			t.StripPane = "" // closed during this run, as by a toggle
		}
		if t.StripPane == "" {
			// Pick up a strip this state doesn't know about yet.
			for _, p := range byTab[tab.ID] {
				if p.Label == StripLabel || p.Label == oldStripLabel {
					t.StripPane = p.ID
				}
			}
		}

		want := opt.Strip && t.WantsStrip()
		switch {
		case !want && t.StripPane == opt.Self:
			// This strip is saving a change that hides it; it exits by itself.
		case !want && t.StripPane != "":
			// Strips are off, or the tab became a tool.
			if err := sh.Close(t.StripPane); err != nil && firstErr == nil {
				firstErr = err
			}
			t.StripPane = ""
		case !want:
		case t.StripPane != "":
			if len(byTab[tab.ID]) == 1 {
				// Everything else in the tab closed; don't let the strip keep it open.
				if err := sh.CloseTab(tab.ID); err != nil && firstErr == nil {
					firstErr = err
				}
				continue
			}
			if p := byID[t.StripPane]; p.Label != StripLabel {
				if err := sh.Rename(t.StripPane, StripLabel); err != nil && firstErr == nil {
					firstErr = err
				}
			}
			if opt.Startup {
				if err := sh.Relaunch(t.StripPane); err != nil && firstErr == nil {
					firstErr = err
				}
			}
		default:
			id, err := sh.Open(tab.ID)
			if id != "" {
				t.StripPane = id
			}
			if err != nil && firstErr == nil {
				firstErr = fmt.Errorf("adding the strip to %s: %w", tab.ID, err)
			}
		}
	}
	return firstErr
}

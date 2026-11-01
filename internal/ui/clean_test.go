package ui

import (
	"strings"
	"testing"
	"time"

	"github.com/alekkowalczyk/herdr-tasks/internal/herdr"
	"github.com/alekkowalczyk/herdr-tasks/internal/store"
	"github.com/alekkowalczyk/herdr-tasks/internal/strip"
	"github.com/alekkowalczyk/herdr-tasks/internal/task"
)

// agentTask is a task as an agent, or a hand-edited state file, can leave it:
// every text it shows holds terminal escapes.
func agentTask() *task.Task {
	return &task.Task{
		TabID: "w1:t1", Workspace: "Acme", Stage: task.Paused, Since: time.Now(),
		Title:  "Checkout\x1b]0;owned\x07",
		Reason: "QA \x1b[2J\x07sign-off",
		Note:   "hi \x1b]0;owned\x07 there\nsecond\tline",
		Log:    []task.Entry{{Who: "claude\x1b]52;c;aGVsbG8=\x07", At: time.Now(), Text: "x \x1b[2J y\u009b"}},
	}
}

// onlyOwnSequences fails the test when out holds an escape sequence the UI
// didn't make, or misses any of want once escapes and extra spaces are gone.
func onlyOwnSequences(t *testing.T, out string, want ...string) {
	t.Helper()
	for _, bad := range []string{"\x1b]0;", "\x1b]52;", "\x1b[2J", "\x07", "\u009b"} {
		if strings.Contains(out, bad) {
			t.Errorf("output has %q: %q", bad, out)
		}
	}
	if rest := ownSequences.ReplaceAllString(out, ""); strings.Contains(rest, "\x1b") {
		t.Errorf("output has an escape sequence it didn't make: %q", rest)
	}
	text := plain(out)
	for _, w := range want {
		if !strings.Contains(text, w) {
			t.Errorf("output doesn't show %q:\n%s", w, text)
		}
	}
}

func TestStripShowsAgentTextCleaned(t *testing.T) {
	m := &stripModel{t: agentTask(), tab: herdr.Tab{ID: "w1:t1"}, pos: 1, width: 120, height: 8, rows: strip.Rows}
	onlyOwnSequences(t, m.View(),
		"Checkout]0;owned", "until QA [2Jsign-off", "hi ]0;owned there", "second line", "claude]52;c;aGVsbG8=", "x [2J y")
}

func TestOverviewShowsAgentTextCleaned(t *testing.T) {
	tk := agentTask()
	m := &model{
		rowAt: map[int]string{}, linkAt: map[int]task.Link{}, alertY: -1, width: 140, height: 30, sel: tk.TabID,
		live: &herdr.Live{
			Workspaces: []herdr.Workspace{{ID: "w1", Label: "Acme", Number: 1}},
			Tabs:       []herdr.Tab{{ID: tk.TabID, WorkspaceID: "w1", Number: 1}},
		},
		state: &store.State{Tasks: map[string]*task.Task{tk.TabID: tk}},
	}
	m.rebuild()
	onlyOwnSequences(t, m.View(),
		"Checkout]0;owned", "until QA [2Jsign-off", "hi ]0;owned there", "second line", "claude]52;c;aGVsbG8=", "x [2J y")
}

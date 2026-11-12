package board

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/alekkowalczyk/herdr-tasks/internal/herdr"
	"github.com/alekkowalczyk/herdr-tasks/internal/reopen"
	"github.com/alekkowalczyk/herdr-tasks/internal/store"
	"github.com/alekkowalczyk/herdr-tasks/internal/task"
)

// A session's board has its closed tasks, newest first, but those in
// hidden spaces, with what reopening them brings back.
func TestBuildListsClosedTasks(t *testing.T) {
	live := &herdr.Live{Workspaces: []herdr.Workspace{{ID: "w1", Label: "acme"}}}
	state := &store.State{Tasks: map[string]*task.Task{}, Hidden: []string{"side project"}}
	state.Close(&task.Task{TabID: "w2:t1", Workspace: "side project", Title: "Hidden", Stage: task.Done}, now.Add(-3*time.Hour))
	state.Close(&task.Task{TabID: "w1:t4", Workspace: "acme", Stage: task.Paused, Reason: "a review", Since: now.Add(-48 * time.Hour)}, now.Add(-2*time.Hour))
	state.Close(&task.Task{TabID: "w1:t3", Workspace: "acme", Title: "Login loop", Stage: task.Done, Since: now.Add(-5 * time.Hour),
		Priority: true, Note: "retry after the deploy", Log: []task.Entry{{Who: "claude", At: now, Text: "fixed"}},
		Links: []task.Link{{URL: "https://linear.app/acme/issue/ACM-12"}}, Cwd: "/home/dev/acme", Claude: "0b6c1d2e-0000-4000-8000-000000000001",
		Slack: &task.SlackAsk{Place: "#billing", Channel: "C0123ABCD", Research: task.ResearchOpen}}, now.Add(-time.Hour))
	b := Build([]Snapshot{{Name: "work", State: state, Live: live}}, noSummary, nil, false, "0.7.0")
	closed := b.Sessions[0].Closed
	if len(closed) != 2 {
		t.Fatalf("closed = %+v", closed)
	}
	c := closed[0]
	if c.Session != "work" || c.ID != state.Closed[0].ID || c.Title != "Login loop" || c.SpaceLabel != "acme" || !c.Closed.Equal(now.Add(-time.Hour)) ||
		c.Stage != "done" || !c.Since.Equal(now.Add(-5*time.Hour)) || !c.Priority || c.Note != "retry after the deploy" || len(c.Log) != 1 ||
		len(c.Links) != 1 || c.Links[0].Kind != "linear" || c.Cwd != "/home/dev/acme" || !c.Claude || c.Slack == nil || c.Slack.Place != "#billing" {
		t.Fatalf("closed card = %+v", c)
	}
	if c := closed[1]; c.Title != "" || c.Reason != "a review" || c.Claude || c.Stage != "paused" {
		t.Fatalf("untitled closed card = %+v", c)
	}
	j, err := json.Marshal(closed[0])
	if err != nil {
		t.Fatal(err)
	}
	var v map[string]any
	if err := json.Unmarshal(j, &v); err != nil {
		t.Fatal(err)
	}
	if got, want := keys(t, v), "claude closed cwd id links log note priority session since slack spaceLabel stage title"; got != want {
		t.Errorf("closed card keys = %s, want %s", got, want)
	}
}

// postReopen posts body to /api/reopen.
func (ts *testServer) postReopen(t *testing.T, body string) (int, reopenReply) {
	t.Helper()
	req, err := http.NewRequest("POST", ts.url("/api/reopen"), strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+ts.key)
	req.Header.Set("Content-Type", "application/json")
	resp, err := plain.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	var reply reopenReply
	if err := json.Unmarshal(raw, &reply); err != nil {
		t.Fatalf("answer %d %q: %v", resp.StatusCode, raw, err)
	}
	return resp.StatusCode, reply
}

// The board reopens a closed task, and answers with its new tab and what to
// say about it; with the tab when Claude didn't resume, so the page shows it.
func TestReopenAction(t *testing.T) {
	work := newConn(t, "work", oneSpace(herdr.Tab{ID: "w1:t1"}), &task.Task{TabID: "w1:t1", Stage: task.Active, Since: now})
	var id string
	err := work.st.Update(func(s *store.State) error {
		s.Close(&task.Task{TabID: "w1:t2", Workspace: "acme", Title: "Login loop", Stage: task.Done}, now)
		id = s.Closed[0].ID
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	work.reopened = reopen.Reopened{Tab: "w1:t5", Resumed: true}
	ts := startServer(t, Config{Sessions: running(work)}, nil)
	ts.shows(t, "work", "w1:t1")

	body := fmt.Sprintf(`{"session":"work","id":%q}`, id)
	if code, reply := ts.postReopen(t, body); code != http.StatusOK || reply != (reopenReply{Tab: "w1:t5", Said: "Reopened Login loop and resumed its Claude session"}) {
		t.Errorf("reopen: %d %+v", code, reply)
	}
	if want := []string{"reopen " + id}; !slices.Equal(work.actions(), want) {
		t.Errorf("asked %q, want %q", work.actions(), want)
	}

	work.reopened, work.reopenErr = reopen.Reopened{Tab: "w1:t6"}, errors.New("Claude didn't resume its session: not at a prompt")
	if code, reply := ts.postReopen(t, body); code != http.StatusInternalServerError || reply.Tab != "w1:t6" || reply.Error != work.reopenErr.Error() {
		t.Errorf("when Claude didn't resume: %d %+v", code, reply)
	}
	work.reopened, work.reopenErr = reopen.Reopened{}, reopen.ErrGone
	if code, reply := ts.postReopen(t, body); code != http.StatusNotFound || reply.Error != reopen.ErrGone.Error() || reply.Tab != "" {
		t.Errorf("one that's gone: %d %+v", code, reply)
	}
	for body, want := range map[string]int{`{}`: 400, `{"session":"work"}`: 400, `reopen it`: 400, `{"session":"home","id":"abc"}`: 404} {
		if code, reply := ts.postReopen(t, body); code != want || reply.Error == "" {
			t.Errorf("%s: %d %+v, want %d with an error", body, code, reply, want)
		}
	}
	if n := len(work.actions()); n != 3 {
		t.Errorf("asked %d times, want 3", n)
	}
}

package board

import (
	"encoding/json"
	"fmt"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/alekkowalczyk/herdr-tasks/internal/claudelog"
	"github.com/alekkowalczyk/herdr-tasks/internal/herdr"
	"github.com/alekkowalczyk/herdr-tasks/internal/linear"
	"github.com/alekkowalczyk/herdr-tasks/internal/store"
	"github.com/alekkowalczyk/herdr-tasks/internal/task"
)

var now = time.Date(2026, 10, 31, 9, 0, 0, 0, time.UTC)

func noSummary(herdr.Pane) (claudelog.Summary, bool) { return claudelog.Summary{}, false }

func tabIDs(cards []Card) string {
	var ids []string
	for _, c := range cards {
		ids = append(ids, c.Tab)
	}
	return strings.Join(ids, " ")
}

func TestBuildLeavesOutToolsAndHiddenSpaces(t *testing.T) {
	live := &herdr.Live{
		Workspaces: []herdr.Workspace{{ID: "w1", Label: "acme"}, {ID: "w2", Label: "side project"}, {ID: "w3", Label: "docs"}},
		Tabs: []herdr.Tab{
			{ID: "w1:t1", WorkspaceID: "w1", AgentStatus: "idle"},
			{ID: "w1:t2", WorkspaceID: "w1"},
			{ID: "w1:t3", WorkspaceID: "w1"},
			{ID: "w2:t1", WorkspaceID: "w2", AgentStatus: "blocked"},
			{ID: "w3:t1", WorkspaceID: "w3"},
		},
	}
	state := &store.State{
		Hidden: []string{"side project"},
		Tasks: map[string]*task.Task{
			"w1:t1": {TabID: "w1:t1", Stage: task.Active, Since: now},
			"w1:t2": {TabID: "w1:t2", Title: "dev server", Stage: task.Tool, Since: now},
			// w1:t3 has no task yet.
			"w2:t1": {TabID: "w2:t1", Stage: task.Active, Since: now},
			"w3:t1": {TabID: "w3:t1", Title: "Write the guide", Stage: task.Parked, Since: now},
		},
	}
	b := Build([]Snapshot{{Name: "work", State: state, Live: live}}, noSummary, nil, false, "0.7.0")
	if b.Version != "0.7.0" || b.LinearKey || len(b.Sessions) != 1 {
		t.Fatalf("board = %+v", b)
	}
	s := b.Sessions[0]
	if want := []Space{{ID: "w1", Label: "acme"}, {ID: "w3", Label: "docs"}}; !reflect.DeepEqual(s.Spaces, want) {
		t.Fatalf("spaces = %+v", s.Spaces)
	}
	if got := tabIDs(s.Cards); got != "w1:t1 w3:t1" {
		t.Fatalf("cards = %s", got)
	}
	c := s.Cards[0]
	if c.Session != "work" || c.Title != "tab 1" || c.Space != "w1" || c.SpaceLabel != "acme" || c.Column != "yourturn" || c.Stage != "active" {
		t.Fatalf("card = %+v", c)
	}
	if c := s.Cards[1]; c.Title != "Write the guide" || c.Column != "parked" || c.SpaceLabel != "docs" {
		t.Fatalf("card = %+v", c)
	}
}

func TestBuildColumnsFollowAgentStatus(t *testing.T) {
	live := &herdr.Live{
		Workspaces: []herdr.Workspace{{ID: "w1", Label: "acme"}},
		Tabs: []herdr.Tab{
			{ID: "w1:t1", WorkspaceID: "w1", AgentStatus: "working"},
			{ID: "w1:t2", WorkspaceID: "w1", AgentStatus: "blocked"},
			{ID: "w1:t3", WorkspaceID: "w1", AgentStatus: "idle"},
			{ID: "w1:t4", WorkspaceID: "w1", AgentStatus: "working"},
			{ID: "w1:t5", WorkspaceID: "w1", AgentStatus: "done"},
		},
	}
	state := &store.State{Tasks: map[string]*task.Task{
		"w1:t1": {TabID: "w1:t1", Stage: task.Active, Since: now},
		"w1:t2": {TabID: "w1:t2", Stage: task.Active, Since: now},
		"w1:t3": {TabID: "w1:t3", Stage: task.Active, Since: now},
		// A stage other than Active keeps its own column whatever the agent does.
		"w1:t4": {TabID: "w1:t4", Stage: task.Review, Since: now, Reason: "a merge"},
		"w1:t5": {TabID: "w1:t5", Stage: task.Done, Since: now},
	}}
	b := Build([]Snapshot{{Name: "work", State: state, Live: live}}, noSummary, nil, false, "")
	var got []string
	for _, c := range b.Sessions[0].Cards {
		got = append(got, fmt.Sprintf("%s %s %s", c.Tab, c.Column, c.AgentStatus))
	}
	want := "[w1:t2 asking blocked w1:t3 yourturn idle w1:t1 running working w1:t4 review working w1:t5 done done]"
	if fmt.Sprint(got) != want {
		t.Fatalf("cards = %v", got)
	}
	if c := b.Sessions[0].Cards[3]; c.Stage != "review" || c.Reason != "a merge" {
		t.Fatalf("card = %+v", c)
	}
}

func TestBuildAgentsSkipStripAndStoppedAgents(t *testing.T) {
	live := &herdr.Live{
		Workspaces: []herdr.Workspace{{ID: "w1", Label: "acme"}},
		Tabs:       []herdr.Tab{{ID: "w1:t1", WorkspaceID: "w1", AgentStatus: "working"}, {ID: "w1:t2", WorkspaceID: "w1"}},
	}
	state := &store.State{Tasks: map[string]*task.Task{"w1:t1": {TabID: "w1:t1", Stage: task.Active, Since: now}}}
	claude := func(id string) *herdr.AgentSession {
		return &herdr.AgentSession{Source: "herdr:claude", Agent: "claude", Kind: "id", Value: id}
	}
	panes := []herdr.Pane{
		{ID: "w1:p1", TabID: "w1:t1", Agent: "claude", AgentStatus: "working", Session: claude("11111111-aaaa")},
		{ID: "w1:p2", TabID: "w1:t1", Label: "Task strip", Agent: "claude", Session: claude("22222222-bbbb")},
		// Claude exited: herdr keeps the session but clears the agent.
		{ID: "w1:p3", TabID: "w1:t1", AgentStatus: "unknown", Session: claude("33333333-cccc")},
		{ID: "w1:p4", TabID: "w1:t1", Agent: "codex", AgentStatus: "idle", Title: "Fix the build",
			Session: &herdr.AgentSession{Source: "herdr:codex", Agent: "codex", Kind: "id", Value: "44444444-dddd"}},
		{ID: "w1:p5", TabID: "w1:t1", Agent: "claude", AgentStatus: "idle", Title: "Reading files", Session: claude("55555555-eeee")},
		// Claude without a session ID.
		{ID: "w1:p6", TabID: "w1:t1", Agent: "claude", AgentStatus: "idle",
			Session: &herdr.AgentSession{Source: "herdr:claude", Agent: "claude", Kind: "name", Value: "scratch"}},
		// A tab without a task isn't on the board.
		{ID: "w1:p7", TabID: "w1:t2", Agent: "claude", Session: claude("77777777-ffff")},
	}
	recapAt := now.Add(-time.Hour)
	var asked []string
	summary := func(p herdr.Pane) (claudelog.Summary, bool) {
		asked = append(asked, p.ID)
		switch p.ID {
		case "w1:p1":
			return claudelog.Summary{Title: "Plan the board", Recap: "You're adding the board model.", RecapAt: recapAt,
				RecapStale: true, LastPrompt: "add the tests", LastReply: "Done."}, true
		case "w1:p5":
			return claudelog.Summary{Title: "Not this one", Recap: "Old recap."}, true
		}
		return claudelog.Summary{}, false
	}
	b := Build([]Snapshot{{Name: "work", State: state, Live: live, Panes: panes}}, summary, nil, false, "")

	if want := []string{"w1:p1", "w1:p5"}; !reflect.DeepEqual(asked, want) {
		t.Fatalf("summary asked for %v, want %v", asked, want)
	}
	want := []Agent{
		{Pane: "w1:p1", Agent: "claude", Status: "working", Title: "Plan the board", Recap: "You're adding the board model.",
			RecapAt: &recapAt, RecapStale: true, LastPrompt: "add the tests", LastReply: "Done."},
		{Pane: "w1:p4", Agent: "codex", Status: "idle", Title: "Fix the build"},
		{Pane: "w1:p5", Agent: "claude", Status: "idle", Title: "Reading files", Recap: "Old recap."},
		{Pane: "w1:p6", Agent: "claude", Status: "idle"},
	}
	if got := b.Sessions[0].Cards[0].Agents; !reflect.DeepEqual(got, want) {
		t.Fatalf("agents =\n%+v\nwant\n%+v", got, want)
	}
}

func TestBuildLinearStatus(t *testing.T) {
	live := &herdr.Live{
		Workspaces: []herdr.Workspace{{ID: "w1", Label: "acme"}},
		Tabs:       []herdr.Tab{{ID: "w1:t1", WorkspaceID: "w1"}},
	}
	tk := &task.Task{TabID: "w1:t1", Stage: task.Active, Since: now, Links: []task.Link{
		{URL: "https://linear.app/acme/issue/ABC-12/fix-the-login", By: "claude", Added: now},
		{URL: "https://linear.app/acme/issue/abc-13", Added: now},
		{URL: "https://github.com/acme/web/pull/7", Added: now},
		{URL: "https://example.com/spec", Label: "the spec", Added: now},
	}}
	state := &store.State{Tasks: map[string]*task.Task{"w1:t1": tk}}
	fetched := linear.Issue{ID: "ABC-12", Title: "Fix the login", URL: "https://linear.app/acme/issue/ABC-12/fix-the-login",
		State: "In Progress", StateType: "started", Color: "#f2c94c", Assignee: "ann"}
	issues := map[string]linear.Issue{"ABC-12": fetched, "ABC-99": {ID: "ABC-99"}}
	b := Build([]Snapshot{{Name: "work", State: state, Live: live}}, noSummary, issues, true, "")
	if !b.LinearKey {
		t.Fatal("LinearKey = false")
	}
	links := b.Sessions[0].Cards[0].Links
	want := []LinkView{
		{URL: "https://linear.app/acme/issue/ABC-12/fix-the-login", Label: "ABC-12", By: "claude", Kind: "linear", Linear: &fetched},
		{URL: "https://linear.app/acme/issue/abc-13", Label: "ABC-13", Kind: "linear", Linear: &linear.Issue{ID: "ABC-13"}},
		{URL: "https://github.com/acme/web/pull/7", Label: "PR #7", Kind: "other"},
		{URL: "https://example.com/spec", Label: "the spec", Kind: "other"},
	}
	if !reflect.DeepEqual(links, want) {
		t.Fatalf("links =\n%+v\nwant\n%+v", links, want)
	}
	j, err := json.Marshal(links[1])
	if err != nil {
		t.Fatal(err)
	}
	if string(j) != `{"url":"https://linear.app/acme/issue/abc-13","label":"ABC-13","kind":"linear","linear":{"id":"ABC-13"}}` {
		t.Fatalf("json = %s", j)
	}
}

func TestLinkViewKinds(t *testing.T) {
	live := &herdr.Live{
		Workspaces: []herdr.Workspace{{ID: "w1", Label: "acme"}},
		Tabs:       []herdr.Tab{{ID: "w1:t1", WorkspaceID: "w1"}},
	}
	page := "/tmp/acme/plan.html"
	artifact := "https://claude.ai/code/artifact/00000000-0000-4000-8000-000000000001"
	tk := &task.Task{TabID: "w1:t1", Stage: task.Active, Since: now, Links: []task.Link{
		{URL: "https://linear.app/acme/issue/ABC-12", Added: now},
		{URL: "https://acme.slack.com/archives/C0123ABCD/p1700000000123456", Added: now},
		{URL: "https://acme.enterprise.slack.com/archives/C0123ABCD", Added: now},
		{URL: artifact, Label: "The plan", By: "claude", Source: page, Added: now},
		{URL: "https://acme.github.io/demo/", Added: now},
		{URL: "file://" + page, By: "claude", Added: now},
		{URL: "http://localhost:4100", Added: now},
		{URL: "https://github.com/acme/web/pull/7", Added: now},
	}}
	state := &store.State{Tasks: map[string]*task.Task{"w1:t1": tk}}
	links := Build([]Snapshot{{Name: "work", State: state, Live: live}}, noSummary, nil, false, "").Sessions[0].Cards[0].Links

	var got []string
	for _, l := range links {
		got = append(got, l.Kind)
	}
	want := "[linear slack slack artifact hosted file local other]"
	if fmt.Sprint(got) != want {
		t.Fatalf("kinds = %v, want %s", got, want)
	}
	for i, l := range links {
		if (l.Path != "") != (i == 5) || (l.At != nil) != (i == 1) || (l.Source != "") != (i == 3) {
			t.Errorf("link %d = %+v", i, l)
		}
	}
	if links[5].Path != page || links[5].Label != "plan.html" {
		t.Errorf("the file link = %+v", links[5])
	}
	if at := links[1].At; at == nil || at.Unix() != 1700000000 || at.Nanosecond() != 123456000 {
		t.Errorf("the Slack message's time = %v", at)
	}
	if links[3].Source != page || links[3].Label != "The plan" {
		t.Errorf("the artifact = %+v", links[3])
	}

	// Path, at and source are left out when they're empty.
	for i, want := range map[int]string{
		1: "at kind label url",
		3: "by kind label source url",
		5: "by kind label path url",
		6: "kind label url",
	} {
		j, err := json.Marshal(links[i])
		if err != nil {
			t.Fatal(err)
		}
		var v any
		if err := json.Unmarshal(j, &v); err != nil {
			t.Fatal(err)
		}
		if got := keys(t, v); got != want {
			t.Errorf("link %d keys = %s, want %s", i, got, want)
		}
	}
}

func TestBuildSessionsInNameOrderEvenWhenUnread(t *testing.T) {
	live := &herdr.Live{Workspaces: []herdr.Workspace{{ID: "w1", Label: "acme"}}}
	snaps := []Snapshot{
		// Never read: the board shows the session with nothing in it.
		{Name: "work", Stale: true},
		{Name: "home", State: &store.State{Tasks: map[string]*task.Task{}}, Live: live},
	}
	b := Build(snaps, nil, nil, false, "")
	j, err := json.Marshal(b)
	if err != nil {
		t.Fatal(err)
	}
	want := `{"version":"","linearKey":false,"sessions":[` +
		`{"name":"home","spaces":[{"id":"w1","label":"acme"}],"cards":[]},` +
		`{"name":"work","stale":true,"spaces":[],"cards":[]}]}`
	if string(j) != want {
		t.Fatalf("json =\n%s\nwant\n%s", j, want)
	}
	if j, _ := json.Marshal(Build(nil, nil, nil, false, "")); string(j) != `{"version":"","linearKey":false,"sessions":[]}` {
		t.Fatalf("json = %s", j)
	}
}

// keys lists an object's keys in order.
func keys(t *testing.T, v any) string {
	t.Helper()
	m, ok := v.(map[string]any)
	if !ok {
		t.Fatalf("%v isn't an object", v)
	}
	var out []string
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return strings.Join(out, " ")
}

func TestBuildJSONFieldNames(t *testing.T) {
	live := &herdr.Live{
		Workspaces: []herdr.Workspace{{ID: "w1", Label: "acme"}},
		Tabs:       []herdr.Tab{{ID: "w1:t1", WorkspaceID: "w1", AgentStatus: "working"}},
	}
	tk := &task.Task{TabID: "w1:t1", Title: "Board model", Stage: task.Paused, Since: now, Reason: "a review",
		Priority: true, Note: "Keep it small.", Log: []task.Entry{{Who: "claude", At: now, Text: "started"}},
		Links: []task.Link{{URL: "https://linear.app/acme/issue/ABC-12", By: "claude", Added: now}}}
	state := &store.State{Tasks: map[string]*task.Task{"w1:t1": tk}}
	panes := []herdr.Pane{{ID: "w1:p1", TabID: "w1:t1", Agent: "claude", AgentStatus: "working",
		Session: &herdr.AgentSession{Agent: "claude", Kind: "id", Value: "11111111-aaaa"}}}
	summary := func(herdr.Pane) (claudelog.Summary, bool) {
		return claudelog.Summary{Title: "Plan", Recap: "Recap.", RecapAt: now, RecapStale: true, LastPrompt: "go", LastReply: "ok"}, true
	}
	issues := map[string]linear.Issue{"ABC-12": {ID: "ABC-12", Title: "Fix", URL: "https://linear.app/acme/issue/ABC-12",
		State: "Todo", StateType: "unstarted", Color: "#e2e2e2", Assignee: "ann"}}
	b := Build([]Snapshot{{Name: "work", State: state, Live: live, Panes: panes, Stale: true}}, summary, issues, true, "0.7.0")

	j, err := json.Marshal(b)
	if err != nil {
		t.Fatal(err)
	}
	var v map[string]any
	if err := json.Unmarshal(j, &v); err != nil {
		t.Fatal(err)
	}
	session := v["sessions"].([]any)[0]
	card := session.(map[string]any)["cards"].([]any)[0].(map[string]any)
	link := card["links"].([]any)[0].(map[string]any)
	checks := []struct {
		what string
		v    any
		want string
	}{
		{"board", v, "linearKey sessions version"},
		{"session", session, "cards name spaces stale"},
		{"space", session.(map[string]any)["spaces"].([]any)[0], "id label"},
		{"card", card, "agentStatus agents column links log note priority reason session since space spaceLabel stage tab title"},
		{"link", link, "by kind label linear url"},
		{"issue", link["linear"], "assignee color id state stateType title url"},
		{"agent", card["agents"].([]any)[0], "agent lastPrompt lastReply pane recap recapAt recapStale status title"},
		{"log entry", card["log"].([]any)[0], "at text who"},
	}
	for _, c := range checks {
		if got := keys(t, c.v); got != c.want {
			t.Errorf("%s keys = %s, want %s", c.what, got, c.want)
		}
	}
	if card["column"] != "paused" || card["stage"] != "paused" || card["title"] != "Board model" || card["since"] != "2026-10-31T09:00:00Z" {
		t.Errorf("card = %v", card)
	}

	// Empty optional fields are left out, and lists are [] rather than null.
	tk.Reason, tk.Note, tk.Log, tk.Links, tk.Priority = "", "", nil, nil, false
	b = Build([]Snapshot{{Name: "work", State: state, Live: live}}, noSummary, nil, false, "")
	j, err = json.Marshal(b.Sessions[0].Cards[0])
	if err != nil {
		t.Fatal(err)
	}
	if want := `"priority":false,"agentStatus":"working","links":[],"agents":[]}`; !strings.HasSuffix(string(j), want) {
		t.Errorf("json = %s, want it to end %s", j, want)
	}
}

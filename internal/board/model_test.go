package board

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/alekkowalczyk/herdr-tasks/internal/claudelog"
	"github.com/alekkowalczyk/herdr-tasks/internal/herdr"
	"github.com/alekkowalczyk/herdr-tasks/internal/linear"
	"github.com/alekkowalczyk/herdr-tasks/internal/slack"
	"github.com/alekkowalczyk/herdr-tasks/internal/store"
	"github.com/alekkowalczyk/herdr-tasks/internal/strip"
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

// A session says which tab its assistant is in, and how its Claude is,
// though the assistant is a tool. The space that holds only the assistant
// isn't one to show or move tasks to.
func TestBuildFindsTheAssistant(t *testing.T) {
	live := &herdr.Live{
		Workspaces: []herdr.Workspace{{ID: "w1", Label: "acme"}, {ID: "w2", Label: "Assistant"}},
		Tabs:       []herdr.Tab{{ID: "w1:t1", WorkspaceID: "w1"}, {ID: "w2:t1", WorkspaceID: "w2"}},
	}
	state := &store.State{Tasks: map[string]*task.Task{
		"w1:t1": {TabID: "w1:t1", Stage: task.Active, Since: now},
		"w2:t1": {TabID: "w2:t1", Title: "Assistant", Stage: task.Tool, Since: now, Assistant: &task.Assistant{SessionID: "s1"}},
	}}
	panes := []herdr.Pane{{ID: "w2:p1", TabID: "w2:t1", WorkspaceID: "w2", Agent: "claude", AgentStatus: "working"}}
	build := func() Session {
		return Build([]Snapshot{{Name: "work", State: state, Live: live, Panes: panes}}, noSummary, nil, false, "").Sessions[0]
	}
	s := build()
	if want := (&AssistantView{Tab: "w2:t1", Status: "working"}); !reflect.DeepEqual(s.Assistant, want) {
		t.Errorf("assistant = %+v, want %+v", s.Assistant, want)
	}
	if want := []Space{{ID: "w1", Label: "acme"}}; !reflect.DeepEqual(s.Spaces, want) {
		t.Errorf("spaces = %+v, want %+v", s.Spaces, want)
	}
	if got := tabIDs(s.Cards); got != "w1:t1" {
		t.Errorf("cards = %s", got)
	}

	// A task in the assistant's space shows the space. A Claude that
	// exited has no status, though herdr keeps the pane's last one.
	live.Tabs = append(live.Tabs, herdr.Tab{ID: "w2:t2", WorkspaceID: "w2"})
	state.Tasks["w2:t2"] = &task.Task{TabID: "w2:t2", Stage: task.Active, Since: now}
	panes[0].Agent = ""
	s = build()
	if s.Assistant == nil || s.Assistant.Status != "" || len(s.Spaces) != 2 {
		t.Errorf("assistant = %+v, spaces = %+v", s.Assistant, s.Spaces)
	}

	delete(state.Tasks, "w2:t1")
	if s = build(); s.Assistant != nil {
		t.Errorf("assistant = %+v, want none", s.Assistant)
	}
	if data, _ := json.Marshal(s); strings.Contains(string(data), `"assistant"`) {
		t.Errorf("a session without an assistant has one in JSON: %s", data)
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
		{ID: "w1:p1", TabID: "w1:t1", Label: "Board model", Agent: "claude", AgentStatus: "working", Session: claude("11111111-aaaa")},
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
		{Pane: "w1:p1", Name: "Board model", Agent: "claude", Status: "working", Title: "Plan the board", Recap: "You're adding the board model.",
			RecapAt: &recapAt, RecapStale: true, LastPrompt: "add the tests", LastReply: "Done."},
		{Pane: "w1:p4", Agent: "codex", Status: "idle", Title: "Fix the build"},
		{Pane: "w1:p5", Agent: "claude", Status: "idle", Title: "Reading files", Recap: "Old recap."},
		{Pane: "w1:p6", Agent: "claude", Status: "idle"},
	}
	if got := b.Sessions[0].Cards[0].Agents; !reflect.DeepEqual(got, want) {
		t.Fatalf("agents =\n%+v\nwant\n%+v", got, want)
	}
}

// A card has the names of its tab's panes in herdr, for the board's
// search: every pane with one, with an agent or not, but the strip.
func TestBuildCardsHaveTheirPanesNames(t *testing.T) {
	live := &herdr.Live{
		Workspaces: []herdr.Workspace{{ID: "w1", Label: "acme"}},
		Tabs:       []herdr.Tab{{ID: "w1:t1", WorkspaceID: "w1"}, {ID: "w1:t2", WorkspaceID: "w1"}},
	}
	state := &store.State{Tasks: map[string]*task.Task{
		"w1:t1": {TabID: "w1:t1", Stage: task.Active, Since: now},
		"w1:t2": {TabID: "w1:t2", Stage: task.Active, Since: now.Add(-time.Hour)},
	}}
	panes := []herdr.Pane{
		{ID: "w1:p1", TabID: "w1:t1", Label: "Board model", Agent: "claude"},
		{ID: "w1:p2", TabID: "w1:t1", Label: strip.Label},
		{ID: "w1:p3", TabID: "w1:t1", Label: "Dev server"},
		{ID: "w1:p4", TabID: "w1:t1"},
		{ID: "w1:p5", TabID: "w1:t2"},
	}
	b := Build([]Snapshot{{Name: "work", State: state, Live: live, Panes: panes}}, nil, nil, false, "")
	cards := b.Sessions[0].Cards
	if got, want := cards[0].Panes, []string{"Board model", "Dev server"}; !reflect.DeepEqual(got, want) {
		t.Errorf("panes = %q, want %q", got, want)
	}
	if got := cards[1].Panes; got != nil {
		t.Errorf("panes of a tab whose panes have no names = %q, want none", got)
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
		{URL: "https://github.com/acme/web/pull/7", Label: "PR #7", Kind: "github-pr", PR: &PR{Repo: "acme/web", Number: 7}},
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
		{URL: "https://github.com/acme/web/pull/7/files", Added: now},
		{URL: "https://example.com/spec", Added: now},
	}}
	state := &store.State{Tasks: map[string]*task.Task{"w1:t1": tk}}
	links := Build([]Snapshot{{Name: "work", State: state, Live: live}}, noSummary, nil, false, "").Sessions[0].Cards[0].Links

	var got []string
	for _, l := range links {
		got = append(got, l.Kind)
	}
	want := "[linear slack slack artifact hosted file local github-pr other]"
	if fmt.Sprint(got) != want {
		t.Fatalf("kinds = %v, want %s", got, want)
	}
	for i, l := range links {
		if (l.Path != "") != (i == 5) || (l.At != nil) != (i == 1) || (l.Source != "") != (i == 3) || (l.PR != nil) != (i == 7) {
			t.Errorf("link %d = %+v", i, l)
		}
	}
	if pr := links[7].PR; pr == nil || *pr != (PR{Repo: "acme/web", Number: 7}) {
		t.Errorf("the pull request's link has %+v", pr)
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
		`{"name":"work","stale":true,"spaces":[],"cards":[]}],"slackSession":""}`
	if string(j) != want {
		t.Fatalf("json =\n%s\nwant\n%s", j, want)
	}
	if j, _ := json.Marshal(Build(nil, nil, nil, false, "")); string(j) != `{"version":"","linearKey":false,"sessions":[],"slackSession":""}` {
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
	panes := []herdr.Pane{{ID: "w1:p1", TabID: "w1:t1", Label: "Planning", Agent: "claude", AgentStatus: "working",
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
		{"board", v, "linearKey sessions slackSession version"},
		{"session", session, "cards name spaces stale"},
		{"space", session.(map[string]any)["spaces"].([]any)[0], "id label"},
		{"card", card, "agentStatus agents column links log note panes priority reason session since space spaceLabel stage tab title"},
		{"link", link, "by kind label linear url"},
		{"issue", link["linear"], "assignee color id state stateType title url"},
		{"agent", card["agents"].([]any)[0], "agent lastPrompt lastReply name pane recap recapAt recapStale status title"},
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

func TestCardCarriesSlack(t *testing.T) {
	live := &herdr.Live{
		Workspaces: []herdr.Workspace{{ID: "w1", Label: "Slack inbox"}},
		Tabs:       []herdr.Tab{{ID: "w1:t1", WorkspaceID: "w1"}, {ID: "w1:t2", WorkspaceID: "w1"}},
	}
	state := &store.State{Tasks: map[string]*task.Task{
		"w1:t1": {TabID: "w1:t1", Title: "Dana Reed: refund for ABC-12", Stage: task.Active, Since: now,
			Slack: &task.SlackAsk{Key: "C0123ABCD/1793450000.000100", Channel: "C0123ABCD", Place: "#billing",
				Author: "Dana Reed", TS: "1793450000.000100", At: now, Permalink: "https://acme.slack.com/archives/C0123ABCD/p1793450000000100",
				SessionID: "0b5e1c2a-7d3f-4e8a-9c61-2f4d8a1b3c5e", Research: task.ResearchLocked}},
		"w1:t2": {TabID: "w1:t2", Stage: task.Active, Since: now},
	}}
	cards := Build([]Snapshot{{Name: "work", State: state, Live: live}}, noSummary, nil, false, "").Sessions[0].Cards
	if len(cards) != 2 {
		t.Fatalf("cards = %+v", cards)
	}
	if got, want := cards[0].Slack, (&CardSlack{Place: "#billing", Channel: "C0123ABCD", Research: "locked"}); !reflect.DeepEqual(got, want) {
		t.Fatalf("card.slack = %+v, want %+v", got, want)
	}
	j, err := json.Marshal(cards[0])
	if err != nil {
		t.Fatal(err)
	}
	var v map[string]any
	if err := json.Unmarshal(j, &v); err != nil {
		t.Fatal(err)
	}
	if got := keys(t, v["slack"]); got != "channel muted place research" {
		t.Errorf("card.slack keys = %s", got)
	}

	// Research that hasn't started is "", which the page reads.
	state.Tasks["w1:t1"].Slack.Research = ""
	cards = Build([]Snapshot{{Name: "work", State: state, Live: live}}, noSummary, nil, false, "").Sessions[0].Cards
	if j, _ := json.Marshal(cards[0].Slack); string(j) != `{"place":"#billing","channel":"C0123ABCD","research":"","muted":false}` {
		t.Errorf("card.slack = %s", j)
	}

	// A task that isn't from Slack has no slack.
	if cards[1].Slack != nil {
		t.Errorf("plain card.slack = %+v", cards[1].Slack)
	}
	if j, _ := json.Marshal(cards[1]); strings.Contains(string(j), `"slack"`) {
		t.Errorf("plain card = %s", j)
	}
}

// slackLineOf is the Slack line on a board s builds.
func slackLineOf(s *Server) *SlackLine {
	var b Board
	s.addSlack(&b)
	return b.Slack
}

// filesIn is what's in dir and below it, by path from dir.
func filesIn(t *testing.T, dir string) []string {
	t.Helper()
	var got []string
	err := filepath.WalkDir(dir, func(path string, _ os.DirEntry, err error) error {
		if err == nil && path != dir {
			rel, _ := filepath.Rel(dir, path)
			got = append(got, filepath.ToSlash(rel))
		}
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	return got
}

// TestSlackLineShowsWhileFetchable: the header has the Slack line while the
// watcher is on, or while it's off and offers a fetch. Otherwise there's
// none. Reading the watcher's state for it makes no file: there's none
// before the watcher saves it, and the board adds none after.
func TestSlackLineShowsWhileFetchable(t *testing.T) {
	dir := t.TempDir()
	status := slack.Status{Text: "Slack: off", Fetch: true}
	s := &Server{cfg: Config{StateDir: dir, Now: func() time.Time { return now }, Slack: func() slack.Status { return status }}}
	check := func(name string, want *SlackLine, made []string) {
		t.Helper()
		if got := slackLineOf(s); !reflect.DeepEqual(got, want) {
			t.Errorf("%s: %+v, want %+v", name, got, want)
		}
		if got := filesIn(t, dir); !reflect.DeepEqual(got, made) {
			t.Errorf("%s: the state folder has %q, want %q", name, got, made)
		}
	}
	check("off with a fetch", &SlackLine{Text: "Slack: off", Fetch: true}, nil)
	status = slack.Status{On: true, Fetch: true, Text: "Slack: last check 08:55"}
	check("on", &SlackLine{Text: "Slack: last check 08:55", Fetch: true}, nil)
	status = slack.Status{Text: "Slack: off"}
	check("off with no fetch", nil, nil)

	// Once the watcher has saved its state, as it has when it offers a
	// fetch, the line reads it and the folder has only what it saved.
	if err := slack.Update(dir, func(s *slack.State) error { s.UserID = "U0ME00001"; return nil }); err != nil {
		t.Fatal(err)
	}
	saved := filesIn(t, dir)
	if want := []string{"slack", "slack/state.json", "slack/state.lock"}; !reflect.DeepEqual(saved, want) {
		t.Fatalf("the watcher saved %q, want %q", saved, want)
	}
	status = slack.Status{Text: "Slack: off", Fetch: true}
	check("off with a fetch and a state", &SlackLine{Text: "Slack: off", Fetch: true}, saved)
}

// skippedAsk is an ask posted at, in #billing, or in a DM with Rui when dm.
func skippedAsk(at time.Time, dm bool, text string) slack.Ask {
	ts := fmt.Sprintf("%d.000100", at.Unix())
	m := slack.Message{Channel: "C0123ABCD", Place: "#billing", AuthorID: "U0DANA001", Author: "Dana Reed", TS: ts, Text: text}
	kind, key := slack.KindMention, "C0123ABCD/"+ts
	if dm {
		m.Channel, m.Place, m.AuthorID, m.Author = "D0RUI0001", "DM", "U0RUI0001", "Rui Costa"
		kind, key = slack.KindDM, "D0RUI0001"
	}
	m.Permalink = "https://acme.slack.com/archives/" + m.Channel + "/p" + strings.Replace(ts, ".", "", 1)
	return slack.Ask{Message: m, Kind: kind, Key: key, Gist: "lunch", At: at}
}

// TestSlackLineListsSkipped: the Slack line lists the skipped asks, newest
// first, as `slack skipped` does, each with why, without those over 7 days
// old. When the state can't be read, it lists none.
func TestSlackLineListsSkipped(t *testing.T) {
	dir := t.TempDir()
	dana := skippedAsk(time.Date(2026, 10, 28, 12, 59, 0, 0, time.UTC), false, "<@U0ME00001> lunch on Friday, the usual place before the invoice review?")
	rui := skippedAsk(time.Date(2026, 10, 30, 14, 30, 0, 0, time.UTC), true, "book club tonight?")
	refund := skippedAsk(time.Date(2026, 10, 29, 9, 0, 0, 0, time.UTC), false, "<@U0ME00001> the refund?")
	err := slack.Update(dir, func(s *slack.State) error {
		s.UserID = "U0ME00001"
		s.Skip(dana, slack.Skip{Why: "lunch\nplans"}, now.Add(-time.Hour))
		s.Skip(refund, slack.Skip{Answered: true}, now.Add(-30*time.Minute))
		s.Skip(rui, slack.Skip{Why: "a book club"}, now.Add(-10*time.Minute))
		s.Skip(skippedAsk(time.Date(2026, 10, 20, 9, 0, 0, 0, time.UTC), false, "old news"), slack.Skip{Why: "old news"}, now.Add(-8*24*time.Hour))
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	s := &Server{cfg: Config{StateDir: dir, Now: func() time.Time { return now }, Slack: func() slack.Status {
		return slack.Status{Text: "Slack: off", Fetch: true}
	}}}
	line := slackLineOf(s)
	want := []SkippedAsk{
		{ID: rui.ID(), Label: "Oct 30 14:30 DM · Rui Costa: book club tonight?", Why: "off-topic: a book club", Link: rui.Permalink},
		{ID: refund.ID(), Label: "Oct 29 09:00 #billing · Dana Reed: the refund?", Why: "may be answered already", Link: refund.Permalink},
		{ID: dana.ID(), Label: "Oct 28 12:59 #billing · Dana Reed: lunch on Friday, the usual place before…", Why: "off-topic: lunch plans", Link: dana.Permalink},
	}
	if line == nil || !reflect.DeepEqual(line.Skipped, want) {
		t.Fatalf("line = %+v, want skipped %+v", line, want)
	}
	if want := "D0RUI0001/1793370600.000100"; rui.ID() != want {
		t.Errorf("rui's ID = %s, want %s", rui.ID(), want)
	}
	j, err := json.Marshal(line)
	if err != nil {
		t.Fatal(err)
	}
	var v map[string]any
	if err := json.Unmarshal(j, &v); err != nil {
		t.Fatal(err)
	}
	if got := keys(t, v); got != "fetch problem skipped text" {
		t.Errorf("line keys = %s", got)
	}
	if got := keys(t, v["skipped"].([]any)[0]); got != "id label link why" {
		t.Errorf("skipped ask keys = %s", got)
	}
	// The state on disk is as it was: the old one is the watcher's to drop.
	if st, err := slack.Load(dir); err != nil || len(st.Skipped) != 4 {
		t.Errorf("the state was changed: %v, %d skipped", err, len(st.Skipped))
	}

	// None skipped: none listed, and no list in the board's JSON.
	if err := slack.Update(dir, func(s *slack.State) error { s.Skipped = nil; return nil }); err != nil {
		t.Fatal(err)
	}
	line = slackLineOf(s)
	if j, _ := json.Marshal(line); line == nil || line.Skipped != nil || strings.Contains(string(j), "skipped") {
		t.Errorf("line = %s, want no skipped asks", j)
	}

	// A state that can't be read lists none, and the line shows all the same.
	if err := os.WriteFile(filepath.Join(dir, "slack", "state.json"), []byte("{"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got, want := slackLineOf(s), (&SlackLine{Text: "Slack: off", Fetch: true}); !reflect.DeepEqual(got, want) {
		t.Errorf("with the state unreadable: %+v, want %+v", got, want)
	}
}

// twoSessions is a board of the sessions work and side, in that name order.
func twoSessions() Board {
	state := &store.State{Tasks: map[string]*task.Task{
		"w1:t1": {TabID: "w1:t1", Title: "Login page", Stage: task.Active, Since: now},
		"w1:t2": {TabID: "w1:t2", Title: "Billing", Stage: task.Paused, Since: now},
	}}
	live := &herdr.Live{
		Workspaces: []herdr.Workspace{{ID: "w1", Label: "acme"}},
		Tabs:       []herdr.Tab{{ID: "w1:t1", WorkspaceID: "w1"}, {ID: "w1:t2", WorkspaceID: "w1"}},
	}
	return Build([]Snapshot{
		{Name: "work", State: state, Live: live},
		{Name: "side", Live: live, State: &store.State{Tasks: map[string]*task.Task{}}, Stale: true},
	}, noSummary, nil, false, "0.11.0")
}

func TestBoardForKeepsOneSession(t *testing.T) {
	b := twoSessions()
	one, ok := b.For("work")
	if !ok || len(one.Sessions) != 1 || one.Sessions[0].Name != "work" || one.Version != "0.11.0" {
		t.Fatalf("For(work) = %+v, %v", one, ok)
	}
	if len(b.Sessions) != 2 {
		t.Errorf("For changed the board: %d sessions", len(b.Sessions))
	}
	if _, ok := b.For("gone"); ok {
		t.Error("For(gone) is ok")
	}
}

func TestBoardForSlackLineOnlyOnSlackSession(t *testing.T) {
	b := twoSessions()
	b.SlackSession, b.Slack = "work", &SlackLine{Text: "Slack: last check 14:32"}
	if side, _ := b.For("side"); side.Slack != nil {
		t.Errorf("side shows the Slack line: %+v", side.Slack)
	}
	if work, _ := b.For("work"); work.Slack == nil || work.Slack.Text != "Slack: last check 14:32" {
		t.Errorf("work's Slack line = %+v", work.Slack)
	}
}

func TestBoardForSaysSlackSessionIsntRunning(t *testing.T) {
	b := twoSessions()
	b.SlackSession, b.Slack = "default", &SlackLine{Text: "Slack: off"}
	work, _ := b.For("work")
	if want := "Slack asks go to herdr session default, which isn't running. Set slack_session in config.toml to use another."; work.SlackWaiting != want {
		t.Errorf("SlackWaiting = %q, want %q", work.SlackWaiting, want)
	}
	b.Sessions = append(b.Sessions, Session{Name: "default"})
	if work, _ = b.For("work"); work.SlackWaiting != "" {
		t.Errorf("with default running, SlackWaiting = %q", work.SlackWaiting)
	}
	b.Sessions = b.Sessions[:2]
	b.Slack = nil
	if work, _ = b.For("work"); work.SlackWaiting != "" {
		t.Errorf("without the Slack line, SlackWaiting = %q", work.SlackWaiting)
	}
}

func TestBoardListCountsCards(t *testing.T) {
	b := twoSessions()
	b.SlackSession, b.Slack = "work", &SlackLine{Text: "Slack: last check 14:32"}
	got := b.List()
	want := SessionList{Version: "0.11.0", Slack: "work", Sessions: []SessionEntry{{Name: "side", Cards: 0, Stale: true}, {Name: "work", Cards: 2}}}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("List = %+v, want %+v", got, want)
	}
	j, _ := json.Marshal(got)
	if w := `{"version":"0.11.0","slack":"work","sessions":[{"name":"side","cards":0,"stale":true},{"name":"work","cards":2,"stale":false}]}`; string(j) != w {
		t.Errorf("json = %s\nwant %s", j, w)
	}

	// Without the Slack line, as with Slack off, no session gets asks.
	b.Slack = nil
	j, _ = json.Marshal(b.List())
	if w := `{"version":"0.11.0","sessions":[{"name":"side","cards":0,"stale":true},{"name":"work","cards":2,"stale":false}]}`; string(j) != w {
		t.Errorf("without the Slack line, json = %s\nwant %s", j, w)
	}
}

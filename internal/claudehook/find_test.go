package claudehook

import (
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"testing"
)

func load(t *testing.T, name string) Payload {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatal(err)
	}
	var p Payload
	if err := json.Unmarshal(b, &p); err != nil {
		t.Fatalf("%s: %v", name, err)
	}
	return p
}

func TestFind(t *testing.T) {
	cases := []struct {
		file  string
		want  Issue
		found bool
	}{
		{"get_issue.json", Issue{"ABC-12", "https://linear.app/acme/issue/ABC-12/fix-login"}, true},
		{"get_issue_no_url.json", Issue{"ABC-12", ""}, true},
		{"save_issue_create.json", Issue{"ABC-13", "https://linear.app/acme/issue/ABC-13/retry-token-refresh"}, true},
		{"save_issue_team_change.json", Issue{"XYZ-9", "https://linear.app/acme/issue/XYZ-9/move-to-xylo"}, true},
		{"save_comment.json", Issue{"ABC-12", ""}, true},
		{"save_comment_edit.json", Issue{}, false},
		{"create_attachment_from_upload.json", Issue{"ABC-12", ""}, true},
		{"list_issues.json", Issue{}, false},
		{"bash.json", Issue{}, false},
		{"plugin_server_get_issue.json", Issue{"ABC-12", "https://linear.app/acme/issue/ABC-12/fix-login"}, true},
	}
	for _, c := range cases {
		got, found := Find(load(t, c.file))
		if got != c.want || found != c.found {
			t.Errorf("%s: Find = %+v, %v, want %+v, %v", c.file, got, found, c.want, c.found)
		}
	}
}

func payload(tool, input, response string) Payload {
	return Payload{ToolName: tool, ToolInput: json.RawMessage(input), ToolResponse: json.RawMessage(response)}
}

func TestFindFallsBackToTheInputWhenTheResultIsntJSON(t *testing.T) {
	got, found := Find(payload("mcp__linear__get_issue", `{"id":"abc-12"}`, `[{"type":"text","text":"(saved to file)"}]`))
	if !found || got != (Issue{ID: "ABC-12"}) {
		t.Errorf("Find = %+v, %v, want ABC-12 without a URL", got, found)
	}
}

func TestFindNeverUsesParentID(t *testing.T) {
	// A create whose result didn't come through: its parent is another issue.
	p := payload("mcp__linear__save_issue", `{"team":"Platform","title":"Retry","parentId":"ABC-12"}`, `[{"type":"text","text":"(saved to file)"}]`)
	if got, found := Find(p); found {
		t.Errorf("Find = %+v, want nothing", got)
	}
}

func TestFindDropsAURLForAnotherIssue(t *testing.T) {
	p := payload("mcp__linear__get_issue", `{"id":"ABC-12"}`,
		`[{"type":"text","text":"{\"id\":\"ABC-12\",\"url\":\"https://linear.app/acme/issue/ABC-7/other\"}"}]`)
	if got, found := Find(p); !found || got != (Issue{ID: "ABC-12"}) {
		t.Errorf("Find = %+v, %v, want ABC-12 without a URL", got, found)
	}
}

func TestFindDropsCredentialsFromTheURL(t *testing.T) {
	p := payload("mcp__linear__get_issue", `{"id":"ABC-12"}`,
		`[{"type":"text","text":"{\"id\":\"ABC-12\",\"url\":\"https://u:p@linear.app/acme/issue/ABC-12/fix-login?token=x#y\"}"}]`)
	if got, found := Find(p); !found || got != (Issue{"ABC-12", "https://linear.app/acme/issue/ABC-12/fix-login"}) {
		t.Errorf("Find = %+v, %v, want ABC-12 with its plain URL", got, found)
	}
}

func TestFindIgnoresInputsThatArentIssueIDs(t *testing.T) {
	for _, p := range []Payload{
		payload("mcp__linear__get_issue", `{"id":"9a8b7c6d-0000-4000-8000-000000000012"}`, `[]`),
		payload("mcp__linear__save_comment", `{"issueId":"https://linear.app/acme/issue/ABC-12"}`, `[]`),
		payload("mcp__linear__create_attachment", `{"issue":12}`, `[]`),
		payload("mcp__linear__get_issue", `not json`, `not json`),
		payload("mcp__linear__get_issue", ``, ``),
	} {
		if got, found := Find(p); found {
			t.Errorf("Find(%s) = %+v, want nothing", p.ToolInput, got)
		}
	}
}

func TestLinearTools(t *testing.T) {
	m := regexp.MustCompile(LinearTools)
	for name, want := range map[string]bool{
		"mcp__linear__get_issue":                               true,
		"mcp__claude_ai_Linear__save_issue":                    true,
		"mcp__linear__save_comment":                            true,
		"mcp__linear__create_attachment":                       true,
		"mcp__claude_ai_Linear__create_attachment_from_upload": true,
		"mcp__plugin_linear_linear__get_issue":                 true,
		"mcp__linear__list_issues":                             false,
		"mcp__claude_ai_Linear__get_issue_status":              false,
		"mcp__github__get_issue":                               false,
		"Bash":                                                 false,
	} {
		if m.MatchString(name) != want {
			t.Errorf("LinearTools on %s = %v, want %v", name, !want, want)
		}
	}
}

func TestMatcher(t *testing.T) {
	m := regexp.MustCompile(Matcher)
	for name, want := range map[string]bool{
		"mcp__claude_ai_Linear__get_issue": true,
		"mcp__linear__save_comment":        true,
		"Artifact":                         true,
		"Write":                            true,
		"Edit":                             true,
		"MultiEdit":                        true,
		"mcp__plugin_slack_slack__slack_send_message":    true,
		"mcp__claude_ai_Slack__slack_send_message":       true,
		"mcp__claude-in-chrome__navigate":                true,
		"mcp__playwright__browser_navigate":              true,
		"mcp__linear__list_issues":                       false,
		"mcp__plugin_slack_slack__slack_read_thread":     false,
		"mcp__claude_ai_Slack__slack_send_message_draft": false,
		"mcp__playwright__browser_navigate_back":         false,
		"ArtifactComments":                               false,
		"Read":                                           false,
		"NotebookEdit":                                   false,
		"Bash":                                           false,
	} {
		if m.MatchString(name) != want {
			t.Errorf("Matcher on %s = %v, want %v", name, !want, want)
		}
	}
}

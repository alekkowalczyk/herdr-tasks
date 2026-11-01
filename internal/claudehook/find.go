// Package claudehook finds the Linear issues, Slack links and pages a Claude
// session works with, from the input Claude Code gives its hooks.
package claudehook

import (
	"encoding/json"
	"regexp"
	"strings"

	"github.com/alekkowalczyk/herdr-tasks/internal/task"
)

// Payload is the part of Claude Code's hook input this uses: PostToolUse,
// UserPromptSubmit and Stop.
type Payload struct {
	HookEventName        string          `json:"hook_event_name"`
	Cwd                  string          `json:"cwd"`
	AgentID              string          `json:"agent_id"` // set only for a subagent's tool calls
	ToolName             string          `json:"tool_name"`
	ToolInput            json.RawMessage `json:"tool_input"`
	ToolResponse         json.RawMessage `json:"tool_response"`
	Prompt               string          `json:"prompt"`
	Source               string          `json:"source"` // UserPromptSubmit: user, sdk, system, loop_wakeup, …
	LastAssistantMessage string          `json:"last_assistant_message"`
}

// Issue is a Linear issue a tool call worked on. URL is "" when only the ID is known.
type Issue struct{ ID, URL string }

// LinearTools picks the Linear tools that work on one issue, from any Linear
// MCP server. Searches and lists don't count.
const LinearTools = `^mcp__.*[Ll]inear.*__(get_issue|save_issue|save_comment|create_attachment|create_attachment_from_upload)$`

// Matcher picks the tool calls the PostToolUse hook runs for: the Linear
// tools, publishing an artifact, writing or editing a file, sending a Slack
// message, searching Slack's messages, and opening a page in a browser.
const Matcher = `^(Artifact|Write|Edit|MultiEdit|mcp__.*[Ll]inear.*__(get_issue|save_issue|save_comment|create_attachment|create_attachment_from_upload)|mcp__.*[Ss]lack.*__slack_send_message|mcp__.*[Ss]lack.*__slack_search_(public|public_and_private)|mcp__.*__(browser_)?navigate)$`

var linearTool = regexp.MustCompile(LinearTools)

// issueID is a Linear issue ID such as "ABC-12", in any case.
var issueID = regexp.MustCompile(`(?i)^[a-z][a-z0-9]*-\d+$`)

// Find returns the issue a tool call worked on. It reads only the fields that
// name the issue, never URLs in other text: results also mention related
// issues, pull requests and documents. Other events give nothing.
func Find(p Payload) (Issue, bool) {
	if p.HookEventName != "" && p.HookEventName != "PostToolUse" {
		return Issue{}, false
	}
	m := linearTool.FindStringSubmatch(p.ToolName)
	if m == nil {
		return Issue{}, false
	}
	// Fields that don't decode stay empty.
	var in struct {
		ID      string `json:"id"`
		IssueID string `json:"issueId"`
		Issue   string `json:"issue"`
	}
	_ = json.Unmarshal(p.ToolInput, &in)
	switch m[1] {
	case "get_issue", "save_issue":
		// Prefer the result: moving an issue to another team changes its ID.
		if is, ok := fromResult(p.ToolResponse); ok {
			return is, true
		}
		// A save_issue without an id is a create, and the input has nothing.
		return byID(in.ID)
	case "save_comment":
		// Edits have the comment's id and no issueId.
		return byID(in.IssueID)
	default:
		return byID(in.Issue)
	}
}

// fromResult reads the issue's id and url from an MCP result, which is a list
// of content items whose first text is the issue as JSON.
func fromResult(raw json.RawMessage) (Issue, bool) {
	var content []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if json.Unmarshal(raw, &content) != nil || len(content) == 0 {
		return Issue{}, false
	}
	var res struct {
		ID  string `json:"id"`
		URL string `json:"url"`
	}
	if json.Unmarshal([]byte(content[0].Text), &res) != nil {
		return Issue{}, false
	}
	is, ok := byID(res.ID)
	if !ok {
		return Issue{}, false
	}
	if id, ok := task.LinearID(res.URL); ok && id == is.ID {
		is.URL = linearURL(res.URL)
	}
	return is, true
}

func byID(id string) (Issue, bool) {
	if !issueID.MatchString(id) {
		return Issue{}, false
	}
	return Issue{ID: strings.ToUpper(id)}, true
}

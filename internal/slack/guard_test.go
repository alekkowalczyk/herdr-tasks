package slack

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// harness makes an acme-harness folder with Docs/a.md, a link "out" that
// leaves it, a link "in" to its own Docs, and a link "gone" to nothing.
func harness(t *testing.T) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "acme-harness")
	if err := os.MkdirAll(filepath.Join(dir, "Docs"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "Docs", "a.md"), []byte("# a\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	for name, target := range map[string]string{
		"out":  os.TempDir(),
		"in":   filepath.Join(dir, "Docs"),
		"gone": filepath.Join(t.TempDir(), "nothing-here"),
	} {
		if err := os.Symlink(target, filepath.Join(dir, name)); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

func locked(tool string) string {
	return "Locked: this research session can only read. Choose Continue with my tools (c in the strip) to use " + tool + "."
}

func TestDecideResearch(t *testing.T) {
	dir := harness(t)
	real, err := filepath.EvalSymlinks(dir)
	if err != nil {
		t.Fatal(err)
	}
	read := func(p any) map[string]any { return map[string]any{"file_path": p} }
	slack := func(tool string) string { return "mcp__plugin_slack_slack__" + tool }
	tests := []struct {
		name, tool string
		input      map[string]any
		want       string
	}{
		{"read a relative path", "Read", read("Docs/a.md"), "allow"},
		{"read an absolute path", "Read", read(filepath.Join(dir, "Docs", "a.md")), "allow"},
		{"read the folder's real path", "Read", read(filepath.Join(real, "Docs", "a.md")), "allow"},
		{"read a file not there yet", "Read", read(filepath.Join(dir, "Docs", "new", "b.md")), "allow"},
		{"read through a link that stays inside", "Read", read("in/a.md"), "allow"},
		{"read the folder's parent", "Read", read(dir + "/../x"), "deny"},
		{"read with .. that stays inside", "Read", read("Docs/../Docs/a.md"), "deny"},
		{"read outside", "Read", read("/etc/passwd"), "deny"},
		{"read through a link that leaves", "Read", read("out/x"), "deny"},
		{"read a link to nothing", "Read", read("gone"), "deny"},
		{"read under a link to nothing", "Read", read("gone/x"), "deny"},
		{"read home", "Read", read("~/.ssh/id_ed25519"), "deny"},
		{"read with no path", "Read", map[string]any{}, "deny"},
		{"read a path that isn't text", "Read", read(42), "deny"},
		{"read through a file", "Read", read("Docs/a.md/x"), "deny"},

		{"grep the folder", "Grep", map[string]any{"pattern": "invoice", "path": ""}, "allow"},
		{"grep with no path", "Grep", map[string]any{"pattern": "invoice"}, "allow"},
		{"grep a subfolder", "Grep", map[string]any{"pattern": "invoice", "path": "Docs"}, "allow"},
		{"grep outside", "Grep", map[string]any{"pattern": "root", "path": "/etc"}, "deny"},
		{"grep through a link that leaves", "Grep", map[string]any{"pattern": "x", "path": "out"}, "deny"},
		{"grep a path that isn't text", "Grep", map[string]any{"pattern": "x", "path": nil}, "deny"},

		{"glob in the folder", "Glob", map[string]any{"pattern": "**/*.md"}, "allow"},
		{"glob an absolute pattern inside", "Glob", map[string]any{"pattern": filepath.Join(dir, "Docs", "*.md")}, "allow"},
		{"glob an absolute pattern outside", "Glob", map[string]any{"pattern": "/etc/*"}, "deny"},
		{"glob a sibling folder by braces", "Glob", map[string]any{"pattern": dir + "{-other,}/*"}, "deny"},
		{"glob up from the folder", "Glob", map[string]any{"pattern": "../*"}, "deny"},
		{"glob up through braces", "Glob", map[string]any{"pattern": "{..,Docs}/*"}, "deny"},
		{"glob home", "Glob", map[string]any{"pattern": "~/*"}, "deny"},
		{"glob with no pattern", "Glob", map[string]any{}, "deny"},
		{"glob outside", "Glob", map[string]any{"pattern": "*", "path": "/etc"}, "deny"},
		{"glob with ordinary braces", "Glob", map[string]any{"pattern": "**/*.{md,ts}"}, "allow"},
		{"glob with braces in a folder's name", "Glob", map[string]any{"pattern": "Docs/{a,b}/*.md"}, "deny"},
		{"glob root by a brace alternative", "Glob", map[string]any{"pattern": "{Docs,/etc}/*"}, "deny"},
		{"glob root by an empty part", "Glob", map[string]any{"pattern": "{,/}etc/*"}, "deny"},
		{"glob root by nested braces", "Glob", map[string]any{"pattern": "{a,{b,/etc}}/*"}, "deny"},
		{"glob root by a last empty alternative", "Glob", map[string]any{"pattern": "{a,}/etc/*"}, "deny"},
		{"glob root by a first empty alternative", "Glob", map[string]any{"pattern": "{,a}/etc/*"}, "deny"},
		{"glob root after a negation", "Glob", map[string]any{"pattern": "!{,a}/etc/*"}, "deny"},
		{"glob with an escape", "Glob", map[string]any{"pattern": `Docs\/x`}, "deny"},
		{"glob home by a brace alternative", "Glob", map[string]any{"pattern": "{Docs,~/x}"}, "deny"},
		{"glob with a later alternative starting with /", "Glob", map[string]any{"pattern": "Docs/{a,/etc}/*"}, "deny"},
		{"glob with a later alternative starting with ~", "Glob", map[string]any{"pattern": "Docs/{a,~/x}"}, "deny"},
		{"glob root after a comment mark", "Glob", map[string]any{"pattern": "#{,a}/etc/*"}, "deny"},

		{"slack search", slack("slack_search_public"), nil, "allow"},
		{"slack read thread", slack("slack_read_thread"), nil, "allow"},
		{"slack list members", slack("slack_list_channel_members"), nil, "allow"},
		{"slack reactions", slack("slack_get_reactions"), nil, "allow"},
		{"slack send", slack("slack_send_message"), nil, "deny"},
		{"slack draft", slack("slack_send_message_draft"), nil, "deny"},
		{"slack upload url", slack("slack_get_file_upload_url"), nil, "deny"},
		{"slack react", slack("slack_add_reaction"), nil, "deny"},
		{"slack schedule", slack("slack_schedule_message"), nil, "deny"},
		{"slack canvas", slack("slack_create_canvas"), nil, "deny"},
		{"slack name on another server", "mcp__notion__slack_read_page", nil, "deny"},
		{"slack name split two ways", "mcp__slack__extra__slack_read_thread", nil, "deny"},

		{"linear get", "mcp__linear__get_issue", nil, "allow"},
		{"claude.ai Linear list", "mcp__claude_ai_Linear__list_issues", nil, "allow"},
		{"claude.ai Linear search", "mcp__claude_ai_Linear__search_documentation", nil, "allow"},
		{"claude.ai Linear save", "mcp__claude_ai_Linear__save_issue", nil, "deny"},
		{"linear name on the Slack server", slack("get_issue"), nil, "deny"},

		{"notion", "mcp__notion__notion-search", nil, "deny"},
		{"sqlcl", "mcp__sqlcl__run-sql", nil, "deny"},
		{"bash", "Bash", map[string]any{"command": "ls"}, "deny"},
		{"edit", "Edit", map[string]any{"file_path": "Docs/a.md"}, "deny"},
		{"write", "Write", map[string]any{"file_path": "Docs/a.md"}, "deny"},
		{"web fetch", "WebFetch", nil, "deny"},
		{"agent", "Agent", nil, "deny"},
		{"structured output", "StructuredOutput", nil, "deny"},
		{"tool search", "ToolSearch", map[string]any{"query": "select:mcp__plugin_slack_slack__slack_read_thread"}, "allow"},

		{"exit plan mode", "ExitPlanMode", nil, ""},
		{"ask a question", "AskUserQuestion", nil, ""},
		{"todos", "TodoWrite", nil, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, reason := Decide(ResearchProfile, dir, Call{Tool: tt.tool, Input: tt.input})
			if got != tt.want {
				t.Fatalf("Decide(%s, %v) = %q (%s), want %q", tt.tool, tt.input, got, reason, tt.want)
			}
			if (got == "deny") != (reason != "") {
				t.Errorf("decision %q has reason %q; only a deny has one", got, reason)
			}
		})
	}

	// Tools are named in the reason, and a path outside names the folder.
	if _, reason := Decide(ResearchProfile, dir, Call{Tool: "Bash"}); reason != locked("Bash") {
		t.Errorf("Bash reason = %q", reason)
	}
	if _, reason := Decide(ResearchProfile, dir, Call{Tool: "Read", Input: read("/etc/passwd")}); !strings.Contains(reason, "only read files in "+dir) {
		t.Errorf("Read reason = %q, want it to name %s", reason, dir)
	}
}

// TestDecideResearchStartsFromTheSessionFolder: Claude Code resolves relative
// paths, and Grep's and Glob's default path, from the session's cwd, which
// must itself be inside the folder.
func TestDecideResearchStartsFromTheSessionFolder(t *testing.T) {
	dir := harness(t)
	docs := filepath.Join(dir, "Docs")
	outside := t.TempDir()
	tests := []struct {
		name, cwd, tool string
		input           map[string]any
		want            string
	}{
		{"relative read from a subfolder", docs, "Read", map[string]any{"file_path": "a.md"}, "allow"},
		{"grep from a subfolder", docs, "Grep", map[string]any{"pattern": "x"}, "allow"},
		{"no cwd starts from the folder", "", "Read", map[string]any{"file_path": "Docs/a.md"}, "allow"},
		{"grep with no path from outside", outside, "Grep", map[string]any{"pattern": "x", "path": ""}, "deny"},
		{"glob with no path from outside", outside, "Glob", map[string]any{"pattern": "*.md"}, "deny"},
		{"relative read from outside", outside, "Read", map[string]any{"file_path": "a.md"}, "deny"},
		{"absolute read from outside", outside, "Read", map[string]any{"file_path": filepath.Join(docs, "a.md")}, "deny"},
		{"cwd through a link that leaves", filepath.Join(dir, "out"), "Read", map[string]any{"file_path": "x"}, "deny"},
		{"relative cwd", "acme-harness", "Read", map[string]any{"file_path": "x"}, "deny"},
		{"slack from outside", outside, "mcp__plugin_slack_slack__slack_read_thread", nil, "allow"},
	}
	for _, tt := range tests {
		got, reason := Decide(ResearchProfile, dir, Call{Tool: tt.tool, Input: tt.input, Cwd: tt.cwd})
		if got != tt.want {
			t.Errorf("%s: Decide = %q (%s), want %q", tt.name, got, reason, tt.want)
		}
		if got == "deny" && !strings.Contains(reason, "only read files in "+dir) {
			t.Errorf("%s: reason = %q, want it to name %s", tt.name, reason, dir)
		}
	}
}

func TestDecideResearchNeedsAFolder(t *testing.T) {
	for _, dir := range []string{"", "acme-harness"} {
		if got, _ := Decide(ResearchProfile, dir, Call{Tool: "Read", Input: map[string]any{"file_path": "/home/u/acme-harness/a.md"}}); got != "deny" {
			t.Errorf("dir %q: Read = %q, want deny", dir, got)
		}
		if got, _ := Decide(ResearchProfile, dir, Call{Tool: "Grep", Input: map[string]any{"pattern": "x"}}); got != "deny" {
			t.Errorf("dir %q: Grep = %q, want deny", dir, got)
		}
	}
}

func TestDecideWatch(t *testing.T) {
	tests := []struct {
		tool string
		want string
	}{
		{"mcp__plugin_slack_slack__slack_search_public_and_private", "allow"},
		{"mcp__plugin_slack_slack__slack_read_user_profile", "allow"},
		{"StructuredOutput", "allow"},
		{"ToolSearch", "allow"},
		{"mcp__plugin_slack_slack__slack_read_thread", "deny"},
		{"mcp__plugin_slack_slack__slack_search_public", "deny"},
		{"mcp__plugin_slack_slack__slack_send_message", "deny"},
		{"mcp__notion__slack_search_public_and_private", "deny"},
		{"mcp__linear__get_issue", "deny"},
		{"Read", "deny"},
		{"Bash", "deny"},
		{"ExitPlanMode", "deny"},
	}
	for _, tt := range tests {
		got, reason := Decide(WatchProfile, "", Call{Tool: tt.tool, Input: map[string]any{"file_path": "/home/u/acme-harness/a.md"}})
		if got != tt.want {
			t.Errorf("Decide(watch, %s) = %q, want %q", tt.tool, got, tt.want)
		}
		if got == "deny" && reason == "" {
			t.Errorf("Decide(watch, %s) denies with no reason", tt.tool)
		}
	}
}

func TestGuardJudgeProfile(t *testing.T) {
	const reason = "Locked: a topic check can only give its answer."
	for tool, want := range map[string]string{
		"StructuredOutput": "allow",
		"ToolSearch":       "deny",
		"Read":             "deny",
		"Bash":             "deny",
		"mcp__claude_ai_Slack__slack_search_public_and_private": "deny",
		"mcp__plugin_slack_slack__slack_read_user_profile":      "deny",
	} {
		got, why := Decide(JudgeProfile, "", Call{Tool: tool, Input: map[string]any{"file_path": "/home/u/acme-harness/a.md"}})
		if got != want || (got == "deny" && why != reason) {
			t.Errorf("Decide(judge, %s) = %q, %q; want %q", tool, got, why, want)
		}
	}

	// The command takes the profile, with no folder.
	var out bytes.Buffer
	if err := Guard([]string{"judge"}, strings.NewReader(`{"tool_name":"ToolSearch","tool_input":{"query":"slack"}}`), &out); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), `"permissionDecision":"deny"`) || !strings.Contains(out.String(), reason) {
		t.Errorf("Guard(judge, ToolSearch) wrote %s, want a deny with the judge's reason", out.String())
	}
	out.Reset()
	if err := Guard([]string{"judge"}, strings.NewReader(`{"tool_name":"StructuredOutput","tool_input":{"relevant":true,"why":"billing"}}`), &out); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), `"permissionDecision":"allow"`) {
		t.Errorf("Guard(judge, StructuredOutput) wrote %s, want allow", out.String())
	}
	if err := Guard(nil, strings.NewReader(""), &out); err == nil || !strings.Contains(err.Error(), "watch|research|judge") {
		t.Errorf("usage = %v, want it to name the judge profile", err)
	}
	if got := settingsCommand(t, GuardSettings("/home/u/bin/herdr-tasks", JudgeProfile, "")); got != "'/home/u/bin/herdr-tasks' slack-guard judge || exit 2" {
		t.Errorf("settings command = %q", got)
	}
}

func TestDecideUnknownProfileDenies(t *testing.T) {
	if got, _ := Decide("other", "/home/u/acme-harness", Call{Tool: "mcp__plugin_slack_slack__slack_search_public"}); got != "deny" {
		t.Errorf("Decide = %q, want deny", got)
	}
}

func TestGuardWritesTheDecision(t *testing.T) {
	dir := harness(t)
	call := func(tool, input string) string {
		t.Helper()
		var out bytes.Buffer
		in := `{"session_id":"s1","cwd":"` + dir + `","hook_event_name":"PreToolUse","tool_name":"` + tool + `","tool_input":` + input + `}`
		if err := Guard([]string{"research", dir}, strings.NewReader(in), &out); err != nil {
			t.Fatalf("Guard(%s) = %v", tool, err)
		}
		return out.String()
	}
	var got struct {
		HookSpecificOutput struct {
			HookEventName, PermissionDecision, PermissionDecisionReason string
		} `json:"hookSpecificOutput"`
	}
	out := call("Bash", `{"command":"ls"}`)
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("output %q isn't JSON: %v", out, err)
	}
	h := got.HookSpecificOutput
	if h.HookEventName != "PreToolUse" || h.PermissionDecision != "deny" || h.PermissionDecisionReason != locked("Bash") {
		t.Errorf("Bash: got %+v from %s", h, out)
	}

	if out := call("Read", `{"file_path":"Docs/a.md"}`); !strings.Contains(out, `"permissionDecision":"allow"`) {
		t.Errorf("Read: got %s, want allow", out)
	}
	if out := call("Read", `{"file_path":"~/x"}`); !strings.Contains(out, `"permissionDecision":"deny"`) {
		t.Errorf("Read ~/x: got %s, want deny", out)
	}
	if out := call("ExitPlanMode", `{"plan":"x"}`); out != "" {
		t.Errorf("ExitPlanMode: got %q, want no output", out)
	}

	// The payload's cwd is where relative paths start, and the folder is
	// cleaned.
	var c bytes.Buffer
	in := `{"cwd":"` + filepath.Join(dir, "Docs") + `","tool_name":"Read","tool_input":{"file_path":"a.md"}}`
	if err := Guard([]string{"research", dir + "/./"}, strings.NewReader(in), &c); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(c.String(), `"permissionDecision":"allow"`) {
		t.Errorf("Read a.md from Docs: got %s, want allow", c.String())
	}
	c.Reset()
	in = `{"cwd":"` + t.TempDir() + `","tool_name":"Grep","tool_input":{"pattern":"x"}}`
	if err := Guard([]string{"research", dir + "/./"}, strings.NewReader(in), &c); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(c.String(), `"permissionDecision":"deny"`) || !strings.Contains(c.String(), "files in "+dir+".") {
		t.Errorf("Grep from outside: got %s, want a deny naming %s", c.String(), dir)
	}

	// The watch profile takes no folder.
	var w bytes.Buffer
	if err := Guard([]string{"watch"}, strings.NewReader(`{"tool_name":"StructuredOutput","tool_input":{"gists":[]}}`), &w); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(w.String(), `"permissionDecision":"allow"`) {
		t.Errorf("watch StructuredOutput: got %s, want allow", w.String())
	}
}

func TestGuardFailsClosed(t *testing.T) {
	const bash = `{"tool_name":"Bash","tool_input":{"command":"ls"}}`
	tests := []struct {
		name string
		args []string
		in   string
	}{
		{"not JSON", []string{"research", "/home/u/acme-harness"}, "not json"},
		{"no tool name", []string{"research", "/home/u/acme-harness"}, `{"tool_input":{"command":"ls"}}`},
		{"input not an object", []string{"research", "/home/u/acme-harness"}, `{"tool_name":"Read","tool_input":"/etc/passwd"}`},
		{"cwd not text", []string{"research", "/home/u/acme-harness"}, `{"cwd":1,"tool_name":"Read","tool_input":{"file_path":"a.md"}}`},
		{"more after the call", []string{"research", "/home/u/acme-harness"}, bash + `{"tool_name":"Read"}`},
		{"nothing", []string{"research", "/home/u/acme-harness"}, ""},
		{"no profile", nil, bash},
		{"unknown profile", []string{"other"}, bash},
		{"research with no folder", []string{"research"}, bash},
		{"research with a relative folder", []string{"research", "acme-harness"}, bash},
		{"too many arguments", []string{"research", "/home/u/acme-harness", "x"}, bash},
	}
	for _, tt := range tests {
		var out bytes.Buffer
		if err := Guard(tt.args, strings.NewReader(tt.in), &out); err == nil {
			t.Errorf("%s: Guard = nil, want an error", tt.name)
		}
		if out.Len() > 0 {
			t.Errorf("%s: wrote %q, want nothing", tt.name, out.String())
		}
	}

	if err := Guard([]string{"watch"}, strings.NewReader(bash), failingWriter{}); err == nil {
		t.Error("Guard with a failing writer = nil, want an error")
	}
}

type failingWriter struct{}

func (failingWriter) Write([]byte) (int, error) { return 0, errors.New("closed") }

// settingsCommand is the one hook command in a settings file.
func settingsCommand(t *testing.T, b []byte) string {
	t.Helper()
	var s struct {
		Hooks struct {
			PreToolUse []struct {
				Matcher string
				Hooks   []struct{ Type, Command string }
			}
		} `json:"hooks"`
	}
	if err := json.Unmarshal(b, &s); err != nil {
		t.Fatalf("settings %s aren't JSON: %v", b, err)
	}
	pre := s.Hooks.PreToolUse
	if len(pre) != 1 || pre[0].Matcher != "*" || len(pre[0].Hooks) != 1 || pre[0].Hooks[0].Type != "command" {
		t.Fatalf("settings = %s, want one command hook for every tool", b)
	}
	return pre[0].Hooks[0].Command
}

func TestGuardSettingsQuotes(t *testing.T) {
	got := settingsCommand(t, GuardSettings("/home/u/it's here/herdr-tasks", ResearchProfile, "/home/u/acme-harness"))
	want := `'/home/u/it'\''s here/herdr-tasks' slack-guard research '/home/u/acme-harness' || exit 2`
	if got != want {
		t.Errorf("command = %s\nwant      %s", got, want)
	}
	got = settingsCommand(t, GuardSettings("/home/u/bin/herdr-tasks", WatchProfile, ""))
	if want := `'/home/u/bin/herdr-tasks' slack-guard watch || exit 2`; got != want {
		t.Errorf("watch command = %s, want %s", got, want)
	}
}

// TestGuardSettingsCommandBlocksOnFailure runs the hook command in a shell,
// with a fake herdr-tasks that records its arguments and fails.
func TestGuardSettingsCommandBlocksOnFailure(t *testing.T) {
	bin := filepath.Join(t.TempDir(), "it's here")
	if err := os.Mkdir(bin, 0o755); err != nil {
		t.Fatal(err)
	}
	args := filepath.Join(t.TempDir(), "args")
	script := "#!/bin/sh\nfor a in \"$@\"; do echo \"$a\"; done > '" + args + "'\nexit 1\n"
	if err := os.WriteFile(filepath.Join(bin, "herdr-tasks"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	dir := "/home/u/acme 'harness'"
	cmd := exec.Command("/bin/sh", "-c", settingsCommand(t, GuardSettings(filepath.Join(bin, "herdr-tasks"), ResearchProfile, dir)))
	err := cmd.Run()
	var exit *exec.ExitError
	if !errors.As(err, &exit) || exit.ExitCode() != 2 {
		t.Fatalf("hook command = %v, want exit status 2", err)
	}
	b, err := os.ReadFile(args)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := string(b), "slack-guard\nresearch\n"+dir+"\n"; got != want {
		t.Errorf("args = %q, want %q", got, want)
	}

	// A guard that isn't there blocks too.
	cmd = exec.Command("/bin/sh", "-c", settingsCommand(t, GuardSettings(filepath.Join(bin, "missing"), WatchProfile, "")))
	if err := cmd.Run(); !errors.As(err, &exit) || exit.ExitCode() != 2 {
		t.Errorf("missing guard = %v, want exit status 2", err)
	}
}

// TestDecideResearchKeepsGlobsInTheFolder: braces only in a pattern's last
// part, with plain alternatives; the part of a pattern before its first
// wildcard must be inside the folder, symlinks resolved; and paths with
// spaces around them are refused.
func TestDecideResearchKeepsGlobsInTheFolder(t *testing.T) {
	dir := harness(t)
	glob := func(pattern string) map[string]any { return map[string]any{"pattern": pattern} }
	tests := []struct {
		name, tool string
		input      map[string]any
		want       string
	}{
		{"braces that spell . and ..", "Glob", glob(".{.,x}/outside/*"), "deny"},
		{"braces in the first part", "Glob", glob("{a,b}/x"), "deny"},
		{"braces in a folder's name", "Glob", glob("a{b,c}/*"), "deny"},
		{"braces in a middle part", "Glob", glob("Docs/{a,b}/x/*.md"), "deny"},
		{"an alternative that's ..", "Glob", glob("*.{..,md}"), "deny"},
		{"an alternative that's .", "Glob", glob("x{.,y}"), "deny"},
		{"an empty alternative", "Glob", glob("*.{md,}"), "deny"},
		{"an alternative with a wildcard", "Glob", glob("*.{m*,ts}"), "deny"},
		{"nested braces", "Glob", glob("*.{a,{b,c}}"), "deny"},
		{"a brace left open", "Glob", glob("*.{md"), "deny"},
		{"a brace closed alone", "Glob", glob("*.md}"), "deny"},
		{"through a link that leaves", "Glob", glob("out/*"), "deny"},
		{"through a link that leaves, deeper", "Glob", glob("out/x/**/*.md"), "deny"},
		{"no wildcard, through a link that leaves", "Glob", glob("out/x"), "deny"},
		{"through a link that leaves, from a path", "Glob", map[string]any{"pattern": "out/*", "path": "Docs/.."}, "deny"},
		{"through a link to nothing", "Glob", glob("gone/*"), "deny"},
		{"through a link that stays", "Glob", glob("in/*.md"), "allow"},
		{"root alone", "Glob", glob("/*"), "deny"},
		{"a pattern with a space before it", "Glob", glob(" *.md"), "deny"},
		{"a pattern with a newline after it", "Glob", glob("*.md\n"), "deny"},
		{"a path with a space after it", "Grep", map[string]any{"pattern": "x", "path": "Docs "}, "deny"},
		{"a file with a space before it", "Read", map[string]any{"file_path": " /etc/passwd"}, "deny"},
		{"a file with a tab before it", "Read", map[string]any{"file_path": "\tDocs/a.md"}, "deny"},

		{"extensions in braces", "Glob", glob("*.{ts,tsx}"), "allow"},
		{"extensions in braces, deeper", "Glob", glob("src/**/*.{go,md}"), "allow"},
		{"names in braces", "Glob", glob("Docs/{a,b_c-d.e}.md"), "allow"},
		{"a plain folder", "Glob", glob("docs/*.md"), "allow"},
		{"a folder with a space inside", "Read", map[string]any{"file_path": "Docs/my notes.md"}, "allow"},
	}
	for _, tt := range tests {
		got, reason := Decide(ResearchProfile, dir, Call{Tool: tt.tool, Input: tt.input})
		if got != tt.want {
			t.Errorf("%s: Decide(%s, %q) = %q (%s), want %q", tt.name, tt.tool, tt.input, got, reason, tt.want)
		}
	}
}

// TestDecideResearchKeepsSecretsOut: the research session doesn't read
// files that commonly hold keys and credentials, however it names them.
func TestDecideResearchKeepsSecretsOut(t *testing.T) {
	dir := harness(t)
	for _, f := range []string{".env", ".ssh/config", ".config/gcloud/credentials.db", ".config/gh/hosts.yml", "certs/server.pem", "Docs/env.md"} {
		path := filepath.Join(dir, f)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte("x\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Symlink(filepath.Join(dir, ".ssh"), filepath.Join(dir, "keys")); err != nil {
		t.Fatal(err)
	}
	read := func(p string) map[string]any { return map[string]any{"file_path": p} }
	tests := []struct {
		name, tool string
		input      map[string]any
		shown      string // the path the reason names; "" for an allow
	}{
		{".env", "Read", read(".env"), ".env"},
		{".env.local", "Read", read(".env.local"), ".env.local"},
		{".env by its absolute path", "Read", read(filepath.Join(dir, ".env")), filepath.Join(dir, ".env")},
		{".ENV in capitals", "Read", read(".ENV"), ".ENV"},
		{"in .ssh", "Read", read(".ssh/config"), ".ssh/config"},
		{"in .ssh through a link", "Read", read("keys/config"), "keys/config"},
		{"in .aws", "Read", read("x/.aws/credentials"), "x/.aws/credentials"},
		{"in .gnupg", "Read", read(".gnupg/pubring.kbx"), ".gnupg/pubring.kbx"},
		{"in .kube", "Read", read(".kube/config"), ".kube/config"},
		{"in .docker", "Read", read(".docker/config.json"), ".docker/config.json"},
		{"in .config/gcloud", "Read", read(".config/gcloud/credentials.db"), ".config/gcloud/credentials.db"},
		{".netrc", "Read", read(".netrc"), ".netrc"},
		{".npmrc", "Read", read("web/.npmrc"), "web/.npmrc"},
		{".pypirc", "Read", read(".pypirc"), ".pypirc"},
		{".secrets", "Read", read(".secrets"), ".secrets"},
		{"in .secrets", "Read", read(".secrets/linear.key"), ".secrets/linear.key"},
		{".secrets.json", "Read", read("config/.secrets.json"), "config/.secrets.json"},
		{"grep in .secrets", "Grep", map[string]any{"pattern": "token", "path": ".secrets"}, ".secrets"},
		{"grep .secrets files", "Grep", map[string]any{"pattern": "token", "glob": ".secret*"}, ".secret*"},
		{"glob in .secrets", "Glob", map[string]any{"pattern": ".secrets/*"}, ".secrets/*"},
		{"a .pem", "Read", read("certs/server.pem"), "certs/server.pem"},
		{"a .key", "Read", read("certs/server.key"), "certs/server.key"},
		{"id_rsa", "Read", read("id_rsa"), "id_rsa"},
		{"id_rsa.pub", "Read", read("id_rsa.pub"), "id_rsa.pub"},
		{"id_ed25519", "Read", read("backup/id_ed25519"), "backup/id_ed25519"},
		{"id_ecdsa", "Read", read("id_ecdsa_sk"), "id_ecdsa_sk"},
		{"grep in .ssh", "Grep", map[string]any{"pattern": "Host", "path": ".ssh"}, ".ssh"},
		{"grep .env files", "Grep", map[string]any{"pattern": "KEY", "glob": ".env*"}, ".env*"},
		{"grep dotfiles", "Grep", map[string]any{"pattern": "KEY", "glob": ".*"}, ".*"},
		{"grep keys by braces", "Grep", map[string]any{"pattern": "KEY", "glob": "**/*.{go,pem}"}, "**/*.{go,pem}"},
		{"grep keys by include", "Grep", map[string]any{"pattern": "KEY", "include": "*.key"}, "*.key"},
		{"grep under .config/gcloud", "Grep", map[string]any{"pattern": "x", "glob": ".config/gcloud/*"}, ".config/gcloud/*"},
		{"glob in .ssh", "Glob", map[string]any{"pattern": ".ssh/*"}, ".ssh/*"},
		{"glob in .aws by its path", "Glob", map[string]any{"pattern": "*", "path": ".aws"}, ".aws"},
		{"glob in .ssh through a link", "Glob", map[string]any{"pattern": "keys/**/*"}, "keys/**/*"},

		{"a note about env", "Read", read("Docs/env.md"), ""},
		{".environment isn't .env", "Read", read(".environment"), ""},
		{"another .config folder", "Read", read(".config/gh/hosts.yml"), ""},
		{"a key in a name", "Read", read("Docs/keys.md"), ""},
		{"a note about secrets", "Read", read("Docs/secrets.md"), ""},
		// A glob whitelists the hidden files it matches, so one that matches
		// every file greps .env and .ssh too.
		{"grep every file", "Grep", map[string]any{"pattern": "x", "glob": "*"}, "*"},
		{"grep every file, at any depth", "Grep", map[string]any{"pattern": "x", "glob": "**"}, "**"},
		{"grep every file under every folder", "Grep", map[string]any{"pattern": "x", "glob": "**/*"}, "**/*"},
		{"grep every file by braces", "Grep", map[string]any{"pattern": "x", "glob": "{*,x}"}, "{*,x}"},
		{"grep every file by include", "Grep", map[string]any{"pattern": "x", "include": "*"}, "*"},
		{"grep every file in a folder", "Grep", map[string]any{"pattern": "x", "glob": "src/*"}, "src/*"},
		// rg negates a class with [! as with [^.
		{"grep all but x files", "Grep", map[string]any{"pattern": "x", "glob": "[!x]*"}, "[!x]*"},
		{"grep all but x files at any depth", "Grep", map[string]any{"pattern": "x", "glob": "**/[!x]*"}, "**/[!x]*"},
		{"grep dotfiles but .x ones", "Grep", map[string]any{"pattern": "x", "glob": ".[!x]*"}, ".[!x]*"},
		{"grep all but x files by braces", "Grep", map[string]any{"pattern": "x", "include": "{[!x]*,y}"}, "{[!x]*,y}"},
		{"grep with a ^ class too", "Grep", map[string]any{"pattern": "x", "glob": "[^x]*"}, "[^x]*"},
		{"grep a glob that doesn't read", "Grep", map[string]any{"pattern": "x", "glob": "[x"}, "[x"},
		{"grep go files not hidden", "Grep", map[string]any{"pattern": "x", "glob": "[!.]*.go"}, ""},
		{"grep go files not hidden, with ^", "Grep", map[string]any{"pattern": "x", "glob": "[^.]*.go"}, ""},

		{"grep go files", "Grep", map[string]any{"pattern": "x", "glob": "*.go"}, ""},
		{"grep go files at any depth", "Grep", map[string]any{"pattern": "x", "glob": "**/*.go"}, ""},
		{"grep notes", "Grep", map[string]any{"pattern": "x", "include": "*.md"}, ""},
		{"grep ts files in src", "Grep", map[string]any{"pattern": "x", "glob": "src/**/*.ts"}, ""},
		{"grep the folder", "Grep", map[string]any{"pattern": "x"}, ""},
		{"glob for .env files lists names only", "Glob", map[string]any{"pattern": "**/.env"}, ""},
	}
	for _, tt := range tests {
		got, reason := Decide(ResearchProfile, dir, Call{Tool: tt.tool, Input: tt.input})
		want, wantReason := "allow", ""
		if tt.shown != "" {
			want, wantReason = "deny", tt.shown+" may hold secrets; the locked session doesn't read it"
		}
		if got != want || reason != wantReason {
			t.Errorf("%s: Decide(%s, %q) = %q (%s), want %q (%s)", tt.name, tt.tool, tt.input, got, reason, want, wantReason)
		}
	}

	// A glob or include padded with white space, or not a string, isn't one
	// the guard can read as rg would.
	for name, input := range map[string]map[string]any{
		"a padded glob":      {"pattern": "x", "glob": " *.go"},
		"a padded include":   {"pattern": "x", "include": "*.go\n"},
		"a glob list":        {"pattern": "x", "glob": []any{"*.go"}},
		"a number for glob":  {"pattern": "x", "glob": 5},
		"a null for include": {"pattern": "x", "include": nil},
	} {
		got, reason := Decide(ResearchProfile, dir, Call{Tool: "Grep", Input: input})
		if want := fmt.Sprintf(outsideReason, dir); got != "deny" || reason != want {
			t.Errorf("%s: Decide(Grep, %q) = %q (%s), want deny (%s)", name, input, got, reason, want)
		}
	}
}

// TestGuardSettingsFailClosed: a guard that fails, can't start or times out
// blocks the call, as exit 2 does; Claude Code would otherwise fall back to
// its own permissions.
func TestGuardSettingsFailClosed(t *testing.T) {
	var s struct {
		Hooks struct {
			PreToolUse []struct {
				Hooks []map[string]any
			}
		} `json:"hooks"`
	}
	b := GuardSettings("/home/u/bin/herdr-tasks", ResearchProfile, "/home/u/acme-harness")
	if err := json.Unmarshal(b, &s); err != nil {
		t.Fatal(err)
	}
	h := s.Hooks.PreToolUse[0].Hooks[0]
	if h["onFailure"] != "block" || h["timeout"] != 10.0 || h["type"] != "command" {
		t.Errorf("hook = %v, want onFailure block and timeout 10", h)
	}
	if !strings.HasSuffix(h["command"].(string), " || exit 2") {
		t.Errorf("command = %q, want it to end || exit 2", h["command"])
	}
}

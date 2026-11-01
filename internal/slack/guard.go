package slack

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
)

// Profile is what a Claude session run with the guard may do.
type Profile string

const (
	// WatchProfile is a check: it reads your Slack profile, searches Slack,
	// and gives its answer.
	WatchProfile Profile = "watch"
	// ResearchProfile is a locked research session: it reads files in its
	// folder, and reads Slack and Linear.
	ResearchProfile Profile = "research"
	// JudgeProfile is a topic check: it can only give its answer.
	JudgeProfile Profile = "judge"
)

const (
	watchDenied   = "Locked: a Slack check can only read your Slack profile and search Slack."
	judgeDenied   = "Locked: a topic check can only give its answer."
	lockedReason  = "Locked: this research session can only read. Choose Continue with my tools (c in the strip) to use %s."
	outsideReason = "Locked: this research session can only read files in %s. Choose Continue with my tools (c in the strip) to read outside it."
)

const sep = string(filepath.Separator)

// Call is a tool call, as Claude Code's PreToolUse hook passes it.
type Call struct {
	Tool  string         `json:"tool_name"`
	Input map[string]any `json:"tool_input"`
	// Cwd is the session's working folder, where Claude Code starts relative
	// paths. When it's "", they start from the research session's folder.
	Cwd string `json:"cwd"`
}

// Decide says what the guard does with a tool call: "allow", "deny", or "" to
// leave it to Claude Code. A deny comes with the reason Claude is told. dir is
// the research session's folder; the watch and judge profiles have none.
func Decide(p Profile, dir string, c Call) (decision, reason string) {
	// ToolSearch only loads tool definitions. Where Claude Code defers MCP
	// tools, the Slack and Linear tools can't be called without it.
	if c.Tool == "ToolSearch" && (p == WatchProfile || p == ResearchProfile) {
		return "allow", ""
	}
	switch p {
	case WatchProfile:
		if c.Tool == "StructuredOutput" {
			return "allow", ""
		}
		if server, name, ok := mcpTool(c.Tool); ok && strings.Contains(server, "slack") &&
			(name == "slack_read_user_profile" || name == "slack_search_public_and_private") {
			return "allow", ""
		}
		return "deny", watchDenied
	case ResearchProfile:
		return decideResearch(dir, c)
	case JudgeProfile:
		if c.Tool == "StructuredOutput" {
			return "allow", ""
		}
		return "deny", judgeDenied
	}
	return "deny", fmt.Sprintf("Locked: the Slack guard has no profile %q.", p)
}

func decideResearch(dir string, c Call) (string, string) {
	switch c.Tool {
	case "ExitPlanMode", "AskUserQuestion", "TodoWrite":
		return "", ""
	case "Read", "Grep", "Glob":
		if filepath.IsAbs(dir) {
			return decideRead(dir, c)
		}
	default:
		if server, name, ok := mcpTool(c.Tool); ok && (readsSlack(server, name) || readsLinear(server, name)) {
			return "allow", ""
		}
	}
	return "deny", fmt.Sprintf(lockedReason, c.Tool)
}

// readsSlack is true for the Slack connector's read and search tools.
func readsSlack(server, tool string) bool {
	return strings.Contains(server, "slack") && (tool == "slack_get_reactions" ||
		strings.HasPrefix(tool, "slack_read_") || strings.HasPrefix(tool, "slack_search_") || strings.HasPrefix(tool, "slack_list_"))
}

// readsLinear is true for Linear's read tools, from either of its servers.
func readsLinear(server, tool string) bool {
	return strings.Contains(strings.ToLower(server), "linear") &&
		(strings.HasPrefix(tool, "get_") || strings.HasPrefix(tool, "list_") || strings.HasPrefix(tool, "search_"))
}

// mcpTool splits an MCP tool's name, mcp__<server>__<tool>. A name with
// another "__" could be split more than one way, so it isn't taken as one.
func mcpTool(name string) (server, tool string, ok bool) {
	rest, ok := strings.CutPrefix(name, "mcp__")
	if !ok {
		return "", "", false
	}
	server, tool, ok = strings.Cut(rest, "__")
	if !ok || server == "" || tool == "" || strings.Contains(tool, "__") {
		return "", "", false
	}
	return server, tool, true
}

// decideRead allows a Read, Grep or Glob call that reads only inside dir,
// and nothing that may hold secrets. The session's cwd must be inside dir
// too: relative paths start there, and Grep and Glob search it when they
// name no path. A Glob pattern must be one that stays in the folder it
// searches, and the part of it before its first wildcard must be inside dir.
// A path or pattern with spaces around it is refused, as what Claude Code
// does with them isn't known.
func decideRead(dir string, c Call) (string, string) {
	outside := fmt.Sprintf(outsideReason, dir)
	cwd := c.Cwd
	if cwd == "" {
		cwd = dir
	}
	if !filepath.IsAbs(cwd) || !inside(dir, cwd, cwd) {
		return "deny", outside
	}
	key := "path"
	if c.Tool == "Read" {
		key = "file_path"
	}
	v, set := c.Input[key]
	path, ok := v.(string)
	if set && !ok {
		return "deny", outside
	}
	shown := path
	if path == "" {
		if c.Tool == "Read" {
			return "deny", outside
		}
		path = cwd
	}
	if padded(path) || !inside(dir, cwd, path) {
		return "deny", outside
	}
	// What the call reads, and the text that names it, for the secrets check.
	reads := []struct{ path, shown string }{{path, shown}}
	switch c.Tool {
	case "Glob":
		pattern, _ := c.Input["pattern"].(string)
		if padded(pattern) || !globStays(pattern) {
			return "deny", outside
		}
		prefix := staticPrefix(pattern)
		if !filepath.IsAbs(prefix) {
			prefix = filepath.Join(path, prefix)
		}
		if !inside(dir, cwd, prefix) {
			return "deny", outside
		}
		reads = append(reads, struct{ path, shown string }{prefix, pattern})
	case "Grep":
		for _, k := range []string{"glob", "include"} {
			v, set := c.Input[k]
			g, ok := v.(string)
			switch {
			case set && !ok, padded(g):
				return "deny", outside
			case g != "" && secretGlob(g):
				return "deny", fmt.Sprintf(secretReason, g)
			}
		}
	}
	for _, r := range reads {
		if secretPath(cwd, r.path) {
			return "deny", fmt.Sprintf(secretReason, r.shown)
		}
	}
	return "allow", ""
}

// padded reports whether s starts or ends with white space.
func padded(s string) bool { return strings.TrimSpace(s) != s }

// globStays is false for a Glob pattern that could name files outside the
// folder it searches. Braces are only allowed in the pattern's last part,
// where each alternative is a plain name, such as *.{ts,tsx}: anywhere
// else, braces could spell a path, or "..". A pattern can't start with {,
// ~, ! or #: ~ may be your home folder, and some glob libraries don't take
// a leading ! or # as text. Refused anywhere: "..", and a backslash, as
// escapes make the pattern hard to tell.
func globStays(pattern string) bool {
	if pattern == "" || strings.ContainsAny(pattern[:1], "{~!#") ||
		strings.Contains(pattern, "..") || strings.Contains(pattern, `\`) {
		return false
	}
	i := strings.LastIndex(pattern, "/")
	if strings.ContainsAny(pattern[:i+1], "{}") {
		return false
	}
	last := pattern[i+1:]
	for {
		open := strings.IndexAny(last, "{}")
		if open < 0 {
			return true
		}
		if last[open] == '}' {
			return false
		}
		n := strings.IndexAny(last[open+1:], "{}")
		if n < 0 || last[open+1+n] != '}' {
			return false
		}
		for _, alt := range strings.Split(last[open+1:open+1+n], ",") {
			if !plainAlt.MatchString(alt) || alt == "." || alt == ".." {
				return false
			}
		}
		last = last[open+1+n+1:]
	}
}

// plainAlt is a brace alternative a Glob pattern may have: part of a name.
var plainAlt = regexp.MustCompile(`^[A-Za-z0-9_-][A-Za-z0-9_.-]*$`)

// staticPrefix is the part of a Glob pattern before its first part with a
// wildcard or a brace: "Docs/x" in "Docs/x/**/*.md", "" in "*.md", and "/"
// for an absolute pattern whose first part has one.
func staticPrefix(pattern string) string {
	parts := strings.Split(pattern, "/")
	for i, p := range parts {
		if strings.ContainsAny(p, "*?[{") {
			prefix := strings.Join(parts[:i], "/")
			if prefix == "" && strings.HasPrefix(pattern, "/") {
				prefix = "/"
			}
			return prefix
		}
	}
	return pattern
}

// Where keys and credentials commonly are: folders, a folder in .config,
// and files' names as filepath.Match patterns. Names are matched in any
// case, as a file system may not tell them apart.
var (
	secretDirs  = []string{".ssh", ".aws", ".gnupg", ".kube", ".docker"}
	secretNames = []string{".env", ".env.*", ".secrets", ".secrets.*", ".netrc", ".npmrc", ".pypirc", "*.pem", "*.key",
		"id_rsa*", "id_ed25519*", "id_ecdsa*"}
	// secretSamples are names of secrets, for the Grep globs that pick them.
	secretSamples = []string{".ssh", ".aws", ".gnupg", ".kube", ".docker", ".env", ".env.local", ".secrets", ".secrets.json",
		".netrc", ".npmrc", ".pypirc", "server.pem", "server.key", "id_rsa", "id_ed25519", "id_ecdsa"}
)

const secretReason = "%s may hold secrets; the locked session doesn't read it"

// secretPath reports whether path, from base when it's relative, is or is in
// a place that may hold secrets, as written or with its symlinks resolved.
func secretPath(base, path string) bool {
	if !filepath.IsAbs(path) {
		path = filepath.Join(base, path)
	}
	if secretParts(strings.Split(filepath.Clean(path), sep)) {
		return true
	}
	real, err := resolve(path)
	return err != nil || secretParts(strings.Split(real, sep))
}

// secretParts reports whether any of a path's parts names a secret.
func secretParts(parts []string) bool {
	for i, p := range parts {
		p = strings.ToLower(p)
		if slices.Contains(secretDirs, p) || (p == "gcloud" && i > 0 && strings.EqualFold(parts[i-1], ".config")) {
			return true
		}
		if LooksSecret(p) {
			return true
		}
	}
	return false
}

// LooksSecret reports whether a base name is one a secret commonly has, in
// any case: .env, server.key, id_rsa and the like.
func LooksSecret(name string) bool {
	name = strings.ToLower(name)
	for _, pat := range secretNames {
		if ok, _ := filepath.Match(pat, name); ok {
			return true
		}
	}
	return false
}

// secretGlob reports whether a Grep glob could pick files that hold
// secrets: one of its parts, or of its brace alternatives, names one, or
// matches one without matching any file at all, as .env* and .* do; or its
// last part matches every file, as * and ** do. rg greps the hidden files a
// glob matches, so * greps .env, and ** greps .ssh. A folder part that
// matches every folder, as in **/*.go, is fine: rg still skips hidden
// folders whose names the glob doesn't match.
func secretGlob(g string) bool {
	alts, ok := expandBraces(strings.ToLower(g), 64)
	if !ok {
		return true
	}
	for _, alt := range alts {
		parts := strings.Split(negated(alt), "/")
		if secretParts(parts) || anyFile(parts[len(parts)-1]) {
			return true
		}
		for _, p := range parts {
			if _, err := filepath.Match(p, ""); err != nil {
				return true // a glob it can't read as rg would
			}
			if anyFile(p) {
				continue
			}
			for _, name := range secretSamples {
				if ok, _ := filepath.Match(p, name); ok {
					return true
				}
			}
		}
	}
	return false
}

// negated is glob g with each class that [! opens opened with [^ instead:
// rg's globs negate a class with either, and filepath.Match only with ^.
func negated(g string) string {
	b := []byte(g)
	class := false
	for i := 0; i < len(b); i++ {
		switch {
		case b[i] == '\\' && !class:
			i++ // what it escapes
		case b[i] == '[' && !class:
			class = true
			if i+1 < len(b) && b[i+1] == '!' {
				b[i+1] = '^'
			}
		case b[i] == ']' && class:
			class = false
		}
	}
	return string(b)
}

// anyFile reports whether a glob part matches any file, as * does.
func anyFile(p string) bool {
	a, _ := filepath.Match(p, "main.go")
	b, _ := filepath.Match(p, "README")
	return a && b
}

// expandBraces is g with its braces expanded, {a,b}c as ac and bc, nested
// ones too. It's false when there would be more than max, or a brace isn't
// closed.
func expandBraces(g string, max int) ([]string, bool) {
	open := strings.IndexByte(g, '{')
	if open < 0 {
		return []string{g}, !strings.Contains(g, "}")
	}
	depth, start := 0, open+1
	var alts []string
	for i := open; i < len(g); i++ {
		switch g[i] {
		case '{':
			depth++
		case ',':
			if depth == 1 {
				alts, start = append(alts, g[start:i]), i+1
			}
		case '}':
			depth--
			if depth == 0 {
				alts = append(alts, g[start:i])
				var out []string
				for _, a := range alts {
					more, ok := expandBraces(g[:open]+a+g[i+1:], max)
					if !ok || len(out)+len(more) > max {
						return nil, false
					}
					out = append(out, more...)
				}
				return out, true
			}
		}
	}
	return nil, false
}

// inside is true when path is dir or under it. A relative path starts from
// base. Both have their symlinks resolved as far as they exist. A path that
// starts with ~ is never inside, as Claude Code may read it as your home
// folder. Nor is one with a ".." in it: whether it goes up from a symlink or
// from where the symlink points depends on when Claude Code resolves it.
func inside(dir, base, path string) bool {
	if !filepath.IsAbs(dir) || !filepath.IsAbs(base) || path == "" || strings.HasPrefix(path, "~") ||
		slices.Contains(strings.Split(path, sep), "..") {
		return false
	}
	if !filepath.IsAbs(path) {
		path = filepath.Join(base, path)
	}
	d, err := resolve(dir)
	if err != nil {
		return false
	}
	p, err := resolve(path)
	if err != nil {
		return false
	}
	return p == d || strings.HasPrefix(p, strings.TrimSuffix(d, sep)+sep)
}

// resolve cleans path, which is absolute, and resolves its symlinks as far as
// it exists; the part that doesn't exist yet is added back as it was. A
// symlink to nothing is an error, as is any error but a missing file.
func resolve(path string) (string, error) {
	path = filepath.Clean(path)
	var missing []string
	for {
		real, err := filepath.EvalSymlinks(path)
		if err == nil {
			return filepath.Join(append([]string{real}, missing...)...), nil
		}
		if !errors.Is(err, fs.ErrNotExist) {
			return "", err
		}
		if _, lerr := os.Lstat(path); lerr == nil {
			return "", fmt.Errorf("%s is a link to nothing", path)
		} else if !errors.Is(lerr, fs.ErrNotExist) {
			return "", lerr
		}
		parent := filepath.Dir(path)
		if parent == path {
			return "", err
		}
		missing = append([]string{filepath.Base(path)}, missing...)
		path = parent
	}
}

// hookOutput is a PreToolUse hook's decision, as Claude Code reads it.
type hookOutput struct {
	HookSpecificOutput struct {
		HookEventName            string `json:"hookEventName"`
		PermissionDecision       string `json:"permissionDecision"`
		PermissionDecisionReason string `json:"permissionDecisionReason,omitempty"`
	} `json:"hookSpecificOutput"`
}

// Guard runs `herdr-tasks slack-guard <profile> [dir]`: reads Claude Code's
// PreToolUse JSON on in, writes its decision on out. It writes nothing when it
// leaves the call to Claude Code. Any error must block the call: the command
// exits 2, which Claude Code takes as a block.
func Guard(args []string, in io.Reader, out io.Writer) error {
	if len(args) == 0 || len(args) > 2 {
		return errors.New("usage: herdr-tasks slack-guard watch|research|judge [folder]")
	}
	p, dir := Profile(args[0]), ""
	if len(args) == 2 {
		dir = filepath.Clean(args[1])
	}
	switch {
	case p != WatchProfile && p != ResearchProfile && p != JudgeProfile:
		return fmt.Errorf("slack-guard: no profile %q", p)
	case p == ResearchProfile && !filepath.IsAbs(dir):
		return fmt.Errorf("slack-guard: the research profile needs an absolute folder, got %q", dir)
	}
	b, err := io.ReadAll(in)
	if err != nil {
		return fmt.Errorf("slack-guard: reading the tool call: %w", err)
	}
	var call Call
	if err := json.Unmarshal(b, &call); err != nil {
		return fmt.Errorf("slack-guard: reading the tool call: %w", err)
	}
	if call.Tool == "" {
		return errors.New("slack-guard: the tool call has no tool_name")
	}
	decision, reason := Decide(p, dir, call)
	if decision == "" {
		return nil
	}
	var o hookOutput
	o.HookSpecificOutput.HookEventName = "PreToolUse"
	o.HookSpecificOutput.PermissionDecision = decision
	o.HookSpecificOutput.PermissionDecisionReason = reason
	b, err = json.Marshal(o)
	if err != nil {
		return err
	}
	_, err = out.Write(append(b, '\n'))
	return err
}

// GuardSettings is the --settings file that runs the guard for every tool
// call. dir is left out when it's "". `|| exit 2` blocks the call whenever
// the guard fails, even when it can't start. onFailure "block" (Claude Code
// 2.1.295 on) blocks it too when the hook times out after 10 s, or its shell
// can't start: Claude Code would otherwise go on to its own permissions.
func GuardSettings(exe string, p Profile, dir string) []byte {
	command := shellQuote(exe) + " slack-guard " + string(p)
	if dir != "" {
		command += " " + shellQuote(dir)
	}
	command += " || exit 2"
	type hook struct {
		Type      string `json:"type"`
		Command   string `json:"command"`
		OnFailure string `json:"onFailure"`
		Timeout   int    `json:"timeout"`
	}
	type matcher struct {
		Matcher string `json:"matcher"`
		Hooks   []hook `json:"hooks"`
	}
	var s struct {
		Hooks struct {
			PreToolUse []matcher `json:"PreToolUse"`
		} `json:"hooks"`
	}
	s.Hooks.PreToolUse = []matcher{{Matcher: "*", Hooks: []hook{{Type: "command", Command: command, OnFailure: "block", Timeout: 10}}}}
	b, _ := json.Marshal(s) // plain strings: it can't fail
	return b
}

// shellQuote puts s in single quotes for sh. Each quote in s ends the
// quoting, adds an escaped quote, and starts it again.
func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

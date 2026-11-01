package slack

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestCheckArgsPutThePromptFirst(t *testing.T) {
	args := CheckArgs("do it", "/s/watch-settings.json")
	if args[0] != "-p" || args[1] != "do it" {
		t.Fatalf("the prompt isn't right after -p: %q", args)
	}
	for _, pair := range [][2]string{{"--permission-mode", "dontAsk"}, {"--tools", ""}, {"--max-budget-usd", "0.05"}, {"--model", "haiku"}, {"--settings", "/s/watch-settings.json"}} {
		found := false
		for i := 0; i+1 < len(args); i++ {
			if args[i] == pair[0] && args[i+1] == pair[1] {
				found = true
			}
		}
		if !found {
			t.Errorf("missing %s %q in %q", pair[0], pair[1], args)
		}
	}
}

func TestWatchPromptHasTheSearches(t *testing.T) {
	from := time.Unix(1793200000, 0)
	p := WatchPrompt("U0ME00001", from, true)
	for _, want := range []string{`"<@U0ME00001>"`, "is:dm", "is:mpim", "from:<@U0ME00001>", "1793200000"} {
		if !strings.Contains(p, want) {
			t.Errorf("prompt lacks %q:\n%s", want, p)
		}
	}
	p = WatchPrompt("U0ME00001", from, false)
	if strings.Contains(p, "is:dm") || strings.Contains(p, "is:mpim") {
		t.Errorf("prompt has DM searches with dms off:\n%s", p)
	}
	if !strings.Contains(p, "from:<@U0ME00001>") {
		t.Errorf("prompt lacks the own-messages search:\n%s", p)
	}
}

func TestCheckWritesTheGuardAndRunsInAnEmptyFolder(t *testing.T) {
	state := t.TempDir()
	var gotDir string
	var gotArgs []string
	run := func(_ context.Context, dir string, args []string, stdin string) ([]byte, error) {
		gotDir, gotArgs = dir, args
		if stdin != "" {
			t.Errorf("a check got stdin %q, want none: its prompt is in its arguments", stdin)
		}
		entries, _ := os.ReadDir(dir)
		if len(entries) != 0 {
			t.Errorf("the run folder isn't empty: %v", entries)
		}
		return fixture(t, "check.jsonl"), nil
	}
	out, err := Check(context.Background(), run, state, "/bin/herdr-tasks", "U0ME00001", time.Unix(1793200000, 0), true)
	if err != nil {
		t.Fatal(err)
	}
	if len(out.Searches) == 0 {
		t.Errorf("no searches parsed: %+v", out)
	}
	if want := filepath.Join(state, "slack", "run"); gotDir != want {
		t.Errorf("dir = %q, want %q", gotDir, want)
	}
	settings := filepath.Join(state, "slack", "watch-settings.json")
	fi, err := os.Stat(settings)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm() != 0o600 {
		t.Errorf("settings mode = %v", fi.Mode().Perm())
	}
	b, _ := os.ReadFile(settings)
	if !strings.Contains(string(b), "slack-guard watch") {
		t.Errorf("settings don't run the guard: %s", b)
	}
	found := false
	for i, a := range gotArgs {
		if a == "--settings" && gotArgs[i+1] == settings {
			found = true
		}
	}
	if !found {
		t.Errorf("args lack the settings file: %q", gotArgs)
	}
	if !strings.Contains(gotArgs[1], "U0ME00001") {
		t.Errorf("prompt isn't the watch prompt: %q", gotArgs[1])
	}
}

func TestCheckIdentifiesWhenMeIsEmpty(t *testing.T) {
	run := func(_ context.Context, _ string, args []string, stdin string) ([]byte, error) {
		if args[1] != IdentifyPrompt {
			t.Errorf("prompt = %q", args[1])
		}
		return fixture(t, "profile.jsonl"), nil
	}
	out, err := Check(context.Background(), run, t.TempDir(), "/bin/x", "", time.Time{}, true)
	if err != nil {
		t.Fatal(err)
	}
	if out.UserID == "" {
		t.Errorf("no user ID: %+v", out)
	}
}

func TestCheckErrors(t *testing.T) {
	state := t.TempDir()
	from := time.Unix(1793200000, 0)
	cases := []struct {
		name string
		run  Claude
		want string
	}{
		{"sign in", func(context.Context, string, []string, string) ([]byte, error) {
			return fixture(t, "signin.jsonl"), nil
		}, "Run /mcp"},
		{"timeout", func(context.Context, string, []string, string) ([]byte, error) { return nil, context.DeadlineExceeded }, "over 2 minutes"},
		{"no connector", func(context.Context, string, []string, string) ([]byte, error) {
			return []byte(`{"type":"system","subtype":"init","mcp_servers":[]}` + "\n"), nil
		}, "isn't installed"},
		{"failed", func(context.Context, string, []string, string) ([]byte, error) {
			return []byte(`{"type":"system","subtype":"init","mcp_servers":[{"name":"plugin:slack:slack","status":"connected"}]}` + "\n" +
				`{"type":"result","subtype":"error_max_budget_usd","is_error":true,"result":"over budget"}` + "\n"), nil
		}, "couldn't run the check"},
		{"crashed after output", func(context.Context, string, []string, string) ([]byte, error) {
			var kept []string
			for _, l := range strings.Split(string(fixture(t, "check.jsonl")), "\n") {
				if !strings.Contains(l, `"type":"result"`) {
					kept = append(kept, l)
				}
			}
			return []byte(strings.Join(kept, "\n")), errors.New("exit status 1")
		}, "exit status 1"},
		{"other", func(context.Context, string, []string, string) ([]byte, error) {
			return nil, errors.New("exit status 1")
		}, "exit status 1"},
	}
	for _, c := range cases {
		_, err := Check(context.Background(), c.run, state, "/bin/x", "U0ME00001", from, true)
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: err = %v, want %q", c.name, err, c.want)
		}
	}
}

func fakeClaude(t *testing.T, body string) string {
	t.Helper()
	bin := filepath.Join(t.TempDir(), "claude")
	if err := os.WriteFile(bin, []byte("#!/bin/sh\n"+body+"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	return bin
}

func TestExecClaudeScrubsTheEnvironment(t *testing.T) {
	envFile := filepath.Join(t.TempDir(), "env")
	bin := fakeClaude(t, `env > "$ENVFILE"; echo '{}'`)
	t.Setenv("ENVFILE", envFile)
	t.Setenv("HERDR_PANE_ID", "w1:p1")
	t.Setenv("CLAUDECODE", "1")
	t.Setenv("CLAUDE_CODE_ENTRYPOINT", "cli")
	t.Setenv("CLAUDE_CODE_SSE_PORT", "4100")
	// What claude signs in and picks its provider with stays.
	keep := map[string]string{
		"CLAUDE_CODE_OAUTH_TOKEN": "invented-token",
		"CLAUDE_CODE_USE_BEDROCK": "1",
		"CLAUDE_CODE_USE_VERTEX":  "1",
		"ANTHROPIC_API_KEY":       "invented-key",
		"ANTHROPIC_BASE_URL":      "http://localhost:4100",
	}
	for k, v := range keep {
		t.Setenv(k, v)
	}
	out, err := ExecClaude(bin)(context.Background(), t.TempDir(), []string{"-p", "x"}, "")
	if err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(string(out)) != "{}" {
		t.Errorf("stdout = %q", out)
	}
	b, _ := os.ReadFile(envFile)
	env := "\n" + string(b)
	for _, bad := range []string{"HERDR_PANE_ID", "CLAUDECODE", "CLAUDE_CODE_ENTRYPOINT", "CLAUDE_CODE_SSE_PORT"} {
		if strings.Contains(env, bad+"=") {
			t.Errorf("%s reached claude", bad)
		}
	}
	for k, v := range keep {
		if !strings.Contains(env, "\n"+k+"="+v+"\n") {
			t.Errorf("%s didn't reach claude", k)
		}
	}
	if !strings.Contains(env, "PATH=") {
		t.Errorf("PATH didn't reach claude")
	}
}

// TestExecClaudeGivesStdin: a run's stdin is the text it's given, and
// empty when that's "".
func TestExecClaudeGivesStdin(t *testing.T) {
	file := filepath.Join(t.TempDir(), "stdin")
	bin := fakeClaude(t, `cat > "$STDINFILE"; echo '{}'`)
	t.Setenv("STDINFILE", file)
	prompt := "Decide whether the Slack message below is about the topic.\n> lunch on Friday?\n"
	if _, err := ExecClaude(bin)(context.Background(), t.TempDir(), []string{"-p"}, prompt); err != nil {
		t.Fatal(err)
	}
	if b, err := os.ReadFile(file); err != nil || string(b) != prompt {
		t.Errorf("stdin = %q, %v; want %q", b, err, prompt)
	}
	if _, err := ExecClaude(bin)(context.Background(), t.TempDir(), []string{"-p", "x"}, ""); err != nil {
		t.Fatal(err)
	}
	if b, err := os.ReadFile(file); err != nil || len(b) != 0 {
		t.Errorf("with none, stdin = %q, %v; want it empty", b, err)
	}
}

func TestExecClaudeKillsOnTimeout(t *testing.T) {
	bin := fakeClaude(t, "sleep 30")
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	start := time.Now()
	_, err := ExecClaude(bin)(ctx, t.TempDir(), nil, "")
	if err == nil {
		t.Error("no error from a killed run")
	}
	if d := time.Since(start); d > 6*time.Second {
		t.Errorf("took %v to stop", d)
	}
}

func TestFindClaude(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("PATH", t.TempDir())
	if _, err := FindClaude(); err == nil || !strings.Contains(err.Error(), "can't find the claude command") {
		t.Errorf("err = %v", err)
	}
	bin := filepath.Join(home, ".local", "bin", "claude")
	os.MkdirAll(filepath.Dir(bin), 0o755)
	os.WriteFile(bin, []byte("#!/bin/sh\n"), 0o755)
	if got, err := FindClaude(); err != nil || got != bin {
		t.Errorf("got %q, %v", got, err)
	}
	onPath := t.TempDir()
	os.WriteFile(filepath.Join(onPath, "claude"), []byte("#!/bin/sh\n"), 0o755)
	t.Setenv("PATH", onPath)
	if got, _ := FindClaude(); got != filepath.Join(onPath, "claude") {
		t.Errorf("PATH should win, got %q", got)
	}
}

const needLock = "Claude Code 2.1.295 or later is needed for the read-only lock; "

// TestCanLockReadsClaudesVersion: the guard's hooks fail closed only from
// Claude Code 2.1.295 on, so an older claude, or one whose version can't be
// read, is refused.
func TestCanLockReadsClaudesVersion(t *testing.T) {
	for _, v := range []string{"2.1.316 (Claude Code)", "2.1.295 (Claude Code)", "2.2.0", "3.0.1 (Claude Code)", "  2.1.300\n"} {
		if err := canLock(fakeClaude(t, "echo '"+v+"'")); err != nil {
			t.Errorf("%q: canLock = %v, want nil", v, err)
		}
	}
	for v, want := range map[string]string{
		"2.1.294 (Claude Code)": needLock + "this one is 2.1.294",
		"1.9.999":               needLock + "this one is 1.9.999",
		"2.0.400 (Claude Code)": needLock + "this one is 2.0.400",
	} {
		if err := canLock(fakeClaude(t, "echo '"+v+"'")); err == nil || err.Error() != want {
			t.Errorf("%q: canLock = %v, want %q", v, err, want)
		}
	}
	for name, body := range map[string]string{
		"no version":   "echo 'Claude Code'",
		"nothing":      "true",
		"fails":        "echo '2.1.316 (Claude Code)'; exit 1",
		"not a number": "echo 'v2.x.316'",
	} {
		err := canLock(fakeClaude(t, body))
		if err == nil || !strings.HasPrefix(err.Error(), needLock) {
			t.Errorf("%s: canLock = %v, want an error starting %q", name, err, needLock)
		}
	}
	if err := canLock(filepath.Join(t.TempDir(), "claude")); err == nil || !strings.HasPrefix(err.Error(), needLock) {
		t.Errorf("a claude that isn't there: canLock = %v", err)
	}
}

// TestCanLockAsksEachBinaryOnce: the version is read once per binary, and
// again when the binary changes.
func TestCanLockAsksEachBinaryOnce(t *testing.T) {
	count := filepath.Join(t.TempDir(), "count")
	bin := fakeClaude(t, "echo x >> '"+count+"'; echo '2.1.294 (Claude Code)'")
	runs := func() int {
		b, _ := os.ReadFile(count)
		return strings.Count(string(b), "x")
	}
	for range 3 {
		if err := canLock(bin); err == nil {
			t.Fatal("canLock = nil for 2.1.294")
		}
	}
	if n := runs(); n != 1 {
		t.Errorf("claude --version ran %d times, want 1", n)
	}
	// Claude Code updates itself in place.
	if err := os.WriteFile(bin, []byte("#!/bin/sh\necho x >> '"+count+"'; echo '2.1.316 (Claude Code)'\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	later := time.Now().Add(time.Minute)
	if err := os.Chtimes(bin, later, later); err != nil {
		t.Fatal(err)
	}
	if err := canLock(bin); err != nil {
		t.Errorf("after the update: canLock = %v", err)
	}
	if err := canLock(bin); err != nil {
		t.Errorf("after the update, again: canLock = %v", err)
	}
	if n := runs(); n != 2 {
		t.Errorf("claude --version ran %d times, want 2", n)
	}

	// A version that couldn't be read is asked for again.
	flaky := fakeClaude(t, "echo x >> '"+count+"'; exit 1")
	_ = canLock(flaky)
	_ = canLock(flaky)
	if n := runs(); n != 4 {
		t.Errorf("claude --version ran %d times, want 4", n)
	}
}

// TestCheckErrorsOnIncompleteOutput: a check whose output stops short, or
// ran over the cap, failed; it's never an empty check that worked.
func TestCheckErrorsOnIncompleteOutput(t *testing.T) {
	state := t.TempDir()
	from := time.Unix(1793200000, 0)
	const init = `{"type":"system","subtype":"init","mcp_servers":[{"name":"plugin:slack:slack","status":"connected"}]}` + "\n"
	var noResult []string
	for _, l := range strings.Split(string(fixture(t, "check.jsonl")), "\n") {
		if !strings.Contains(l, `"type":"result"`) {
			noResult = append(noResult, l)
		}
	}
	cases := []struct {
		name string
		out  string
		err  error
		want string
	}{
		{"no result event", strings.Join(noResult, "\n"), nil, "Claude couldn't run the check: it stopped without a result"},
		{"a result with no subtype", init + `{"type":"result","is_error":false,"result":"ok"}` + "\n", nil, "Claude couldn't run the check: "},
		{"over the cap", string(fixture(t, "check.jsonl")), errOverCap, "Claude couldn't run the check: its output was over 8 MiB"},
	}
	for _, c := range cases {
		run := func(context.Context, string, []string, string) ([]byte, error) { return []byte(c.out), c.err }
		if _, err := Check(context.Background(), run, state, "/bin/x", "U0ME00001", from, true); err == nil || !strings.HasPrefix(err.Error(), c.want) {
			t.Errorf("%s: err = %v, want %q", c.name, err, c.want)
		}
	}
}

// TestCheckErrorsSayWhatFailed: a run that failed before it started says
// why, rather than that the Slack connector isn't installed.
func TestCheckErrorsSayWhatFailed(t *testing.T) {
	state := t.TempDir()
	from := time.Unix(1793200000, 0)
	failed := []byte(`{"type":"result","subtype":"success","is_error":true,"result":"Invalid API key · Please run /login"}` + "\n")
	for name, err := range map[string]error{"exits 0": nil, "exits 1": errors.New("exit status 1")} {
		run := func(context.Context, string, []string, string) ([]byte, error) { return failed, err }
		_, got := Check(context.Background(), run, state, "/bin/x", "U0ME00001", from, true)
		if want := "Claude couldn't run the check: Invalid API key · Please run /login"; got == nil || got.Error() != want {
			t.Errorf("%s: err = %v, want %q", name, got, want)
		}
	}
	// A run that started with no Slack connector says so.
	none := []byte(`{"type":"system","subtype":"init","mcp_servers":[]}` + "\n" + `{"type":"result","subtype":"success","is_error":false,"result":"ok"}` + "\n")
	run := func(context.Context, string, []string, string) ([]byte, error) { return none, nil }
	if _, err := Check(context.Background(), run, state, "/bin/x", "U0ME00001", from, true); err == nil || !strings.Contains(err.Error(), "isn't installed") {
		t.Errorf("no connector: err = %v", err)
	}
}

// TestExecClaudeSaysWhenOutputIsOverTheCap: the output is cut at 8 MiB, and
// the run is an error.
func TestExecClaudeSaysWhenOutputIsOverTheCap(t *testing.T) {
	bin := fakeClaude(t, "head -c 9000000 /dev/zero")
	out, err := ExecClaude(bin)(context.Background(), t.TempDir(), nil, "")
	if !errors.Is(err, errOverCap) || len(out) != 8<<20 {
		t.Errorf("err = %v, %d bytes; want errOverCap and 8 MiB", err, len(out))
	}
}

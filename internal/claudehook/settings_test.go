package claudehook

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"
)

const bin = "$HOME/.local/bin/herdr-tasks"

// settings is an invented Claude Code settings file, its keys in no
// particular order.
const settings = `{
  "$schema": "https://json.schemastore.org/claude-code-settings.json",
  "env": {
    "ZED": "1",
    "ACME_MODE": "fast"
  },
  "model": "opus",
  "hooks": {
    "SessionStart": [
      {
        "matcher": "*",
        "hooks": [
          {
            "type": "command",
            "command": "bash '/opt/acme/hooks/start.sh' session",
            "timeout": 10
          }
        ]
      }
    ]
  },
  "statusLine": {
    "type": "command",
    "command": "acme-status"
  }
}
`

const noHooks = `{
  "$schema": "https://json.schemastore.org/claude-code-settings.json",
  "env": {
    "ZED": "1"
  },
  "statusLine": {
    "type": "command",
    "command": "acme-status"
  }
}
`

const otherPostToolUse = `{
  "model": "opus",
  "hooks": {
    "PostToolUse": [
      {
        "matcher": "Bash",
        "hooks": [
          {
            "type": "command",
            "command": "acme-audit"
          }
        ]
      }
    ]
  }
}
`

func apply(src []byte) ([]byte, error) { return Apply(src, Command(bin)) }

func decode(t *testing.T, b []byte) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatalf("result doesn't parse: %v\n%s", err, b)
	}
	return m
}

func postToolUse(t *testing.T, b []byte) []any {
	t.Helper()
	hooks, _ := decode(t, b)["hooks"].(map[string]any)
	groups, _ := hooks["PostToolUse"].([]any)
	return groups
}

// installed is how many handlers Apply adds: one for each of its events.
const installed = 3

// countOurs counts the herdr-tasks handlers in b, in every event.
func countOurs(t *testing.T, b []byte) int {
	t.Helper()
	n := 0
	hooks, _ := decode(t, b)["hooks"].(map[string]any)
	for _, groups := range hooks {
		for _, g := range groups.([]any) {
			for _, h := range g.(map[string]any)["hooks"].([]any) {
				if c, _ := h.(map[string]any)["command"].(string); ourCommand.MatchString(c) {
					n++
				}
			}
		}
	}
	return n
}

func keys(t *testing.T, b []byte) []string {
	t.Helper()
	fields, _, _, err := parseObject(b)
	if err != nil {
		t.Fatal(err)
	}
	var ks []string
	for _, f := range fields {
		ks = append(ks, f.key)
	}
	return ks
}

func TestApplyAddsOurHookAndKeepsTheRest(t *testing.T) {
	out, err := Apply([]byte(settings), Command(bin))
	if err != nil {
		t.Fatal(err)
	}
	groups := postToolUse(t, out)
	if len(groups) != 1 {
		t.Fatalf("want one PostToolUse group:\n%s", out)
	}
	g := groups[0].(map[string]any)
	h := g["hooks"].([]any)[0].(map[string]any)
	if g["matcher"] != Matcher || h["type"] != "command" || h["command"] != `"$HOME/.local/bin/herdr-tasks" claude-hook` || h["timeout"] != 10.0 || h["async"] != true {
		t.Fatalf("group = %v", g)
	}
	if got, want := keys(t, out), keys(t, []byte(settings)); !slices.Equal(got, want) {
		t.Fatalf("keys = %v, want %v", got, want)
	}
	// Only the hooks value changes; the bytes around it stay.
	i := strings.Index(settings, `"hooks": `) + len(`"hooks": `)
	j := strings.Index(settings, ",\n  \"statusLine\"")
	if !strings.HasPrefix(string(out), settings[:i]) || !strings.HasSuffix(string(out), settings[j:]) {
		t.Fatalf("bytes outside hooks changed:\n%s", out)
	}
	if !strings.Contains(string(out), "bash '/opt/acme/hooks/start.sh' session") {
		t.Fatalf("SessionStart hook lost:\n%s", out)
	}
}

func TestApplyToAnEmptyFile(t *testing.T) {
	out, err := Apply(nil, Command(bin))
	if err != nil {
		t.Fatal(err)
	}
	want := `{
  "hooks": {
    "PostToolUse": [
      {
        "matcher": "` + Matcher + `",
        "hooks": [
          {
            "type": "command",
            "command": "\"$HOME/.local/bin/herdr-tasks\" claude-hook",
            "timeout": 10,
            "async": true
          }
        ]
      }
    ],
    "UserPromptSubmit": [
      {
        "hooks": [
          {
            "type": "command",
            "command": "\"$HOME/.local/bin/herdr-tasks\" claude-hook",
            "timeout": 10,
            "async": true
          }
        ]
      }
    ],
    "Stop": [
      {
        "hooks": [
          {
            "type": "command",
            "command": "\"$HOME/.local/bin/herdr-tasks\" claude-hook",
            "timeout": 10,
            "async": true
          }
        ]
      }
    ]
  }
}
`
	if string(out) != want {
		t.Fatalf("got:\n%s\nwant:\n%s", out, want)
	}
	for _, src := range []string{"", "\n"} {
		if back, err := Remove([]byte(src)); err != nil || string(back) != src {
			t.Fatalf("Remove(%q) = %q, %v", src, back, err)
		}
	}
}

func TestApplyTwiceIsTheSame(t *testing.T) {
	for _, src := range []string{"", "{}\n", noHooks, settings, otherPostToolUse} {
		once, err := Apply([]byte(src), Command(bin))
		if err != nil {
			t.Fatal(err)
		}
		twice, err := Apply(once, Command(bin))
		if err != nil || string(twice) != string(once) {
			t.Fatalf("second apply changed it (err %v):\n%s\n---\n%s", err, once, twice)
		}
	}
}

func TestApplyThenRemoveRestoresBytes(t *testing.T) {
	for _, src := range []string{noHooks, settings, otherPostToolUse, "{}\n"} {
		out, err := Apply([]byte(src), Command(bin))
		if err != nil {
			t.Fatal(err)
		}
		back, err := Remove(out)
		if err != nil || string(back) != src {
			t.Fatalf("remove didn't restore (err %v):\n%q\n---\n%q", err, src, back)
		}
	}
}

func TestApplyUpdatesAnOldCommandPath(t *testing.T) {
	old, err := Apply([]byte(otherPostToolUse), Command("/old/build/herdr-tasks"))
	if err != nil {
		t.Fatal(err)
	}
	out, err := Apply(old, Command(bin))
	if err != nil {
		t.Fatal(err)
	}
	if countOurs(t, out) != installed || !strings.Contains(string(out), `"\"$HOME/.local/bin/herdr-tasks\" claude-hook"`) || strings.Contains(string(out), "/old/build") {
		t.Fatalf("old hook kept:\n%s", out)
	}
	if !strings.Contains(string(out), "acme-audit") {
		t.Fatalf("other PostToolUse hook lost:\n%s", out)
	}

	// One written by hand, with another matcher, counts as ours too.
	hand := `{"hooks":{"PostToolUse":[{"matcher":"mcp__.*","hooks":[{"type":"command","command":"'/old/herdr-tasks' claude-hook"}]}]}}`
	out, err = Apply([]byte(hand), Command(bin))
	if err != nil {
		t.Fatal(err)
	}
	groups := postToolUse(t, out)
	if countOurs(t, out) != installed || len(groups) != 1 || groups[0].(map[string]any)["matcher"] != Matcher {
		t.Fatalf("hand-written hook not replaced:\n%s", out)
	}
}

func TestApplyInstallsAllThreeEvents(t *testing.T) {
	out, err := Apply([]byte(settings), Command(bin))
	if err != nil {
		t.Fatal(err)
	}
	hooks, _ := decode(t, out)["hooks"].(map[string]any)
	want := map[string]any{"type": "command", "command": `"$HOME/.local/bin/herdr-tasks" claude-hook`, "timeout": 10.0, "async": true}
	// Only PostToolUse has a matcher: the others run for every prompt and reply.
	for event, matcher := range map[string]any{"PostToolUse": Matcher, "UserPromptSubmit": nil, "Stop": nil} {
		groups, _ := hooks[event].([]any)
		if len(groups) != 1 {
			t.Errorf("%s: want one group:\n%s", event, out)
			continue
		}
		g := groups[0].(map[string]any)
		hs, _ := g["hooks"].([]any)
		_, hasMatcher := g["matcher"]
		if g["matcher"] != matcher || hasMatcher != (matcher != nil) || len(hs) != 1 || !reflect.DeepEqual(hs[0], want) {
			t.Errorf("%s: group = %v", event, g)
		}
	}
	if _, ok := hooks["SessionStart"]; !ok || len(hooks) != 4 {
		t.Errorf("want SessionStart and our three events:\n%s", out)
	}
}

func TestApplyReplacesTheOldLinearOnlyHook(t *testing.T) {
	// What 0.6.0 installed: PostToolUse only, for the Linear tools.
	old := `{
  "hooks": {
    "PostToolUse": [
      {
        "matcher": "` + LinearTools + `",
        "hooks": [
          {
            "type": "command",
            "command": "\"$HOME/.local/bin/herdr-tasks\" claude-hook",
            "timeout": 10,
            "async": true
          }
        ]
      }
    ]
  }
}
`
	out, err := Apply([]byte(old), Command(bin))
	if err != nil {
		t.Fatal(err)
	}
	groups := postToolUse(t, out)
	if len(groups) != 1 || groups[0].(map[string]any)["matcher"] != Matcher || countOurs(t, out) != installed {
		t.Fatalf("want one PostToolUse group, with the new matcher:\n%s", out)
	}
	if again, err := Apply(out, Command(bin)); err != nil || string(again) != string(out) {
		t.Fatalf("second apply changed it (err %v):\n%s", err, again)
	}
}

func TestRemoveTakesOursOutOfEveryEvent(t *testing.T) {
	// Ours by hand in SessionStart, and another tool's Stop hook.
	src := `{
  "hooks": {
    "SessionStart": [{"hooks": [{"type": "command", "command": "herdr-tasks claude-hook"}]}],
    "Stop": [{"hooks": [{"type": "command", "command": "acme-done"}]}]
  }
}
`
	in, err := Apply([]byte(src), Command(bin))
	if err != nil {
		t.Fatal(err)
	}
	out, err := Remove(in)
	if err != nil {
		t.Fatal(err)
	}
	hooks, _ := decode(t, out)["hooks"].(map[string]any)
	stop, _ := hooks["Stop"].([]any)
	if countOurs(t, out) != 0 || len(hooks) != 1 || len(stop) != 1 || !strings.Contains(string(out), "acme-done") {
		t.Fatalf("want only the other Stop hook left:\n%s", out)
	}
}

func TestRemoveKeepsOtherHooksInAMixedGroup(t *testing.T) {
	src := `{
  "hooks": {
    "PostToolUse": [
      {
        "matcher": "mcp__.*",
        "hooks": [
          {"type": "command", "command": "herdr-tasks claude-hook"},
          {"type": "command", "command": "acme-log"}
        ]
      },
      {
        "matcher": "Write",
        "hooks": [{"type": "command", "command": "\"/opt/acme/herdr-tasks\" claude-hook", "async": true}]
      }
    ],
    "Stop": [{"hooks": [{"type": "command", "command": "acme-done"}]}]
  }
}
`
	out, err := Remove([]byte(src))
	if err != nil {
		t.Fatal(err)
	}
	groups := postToolUse(t, out)
	if countOurs(t, out) != 0 || len(groups) != 1 {
		t.Fatalf("want only the mixed group left:\n%s", out)
	}
	g := groups[0].(map[string]any)
	hs := g["hooks"].([]any)
	if g["matcher"] != "mcp__.*" || len(hs) != 1 || hs[0].(map[string]any)["command"] != "acme-log" {
		t.Fatalf("group = %v", g)
	}
	if !strings.Contains(string(out), "acme-done") {
		t.Fatalf("Stop hook lost:\n%s", out)
	}
}

func TestOurCommand(t *testing.T) {
	for c, want := range map[string]bool{
		"herdr-tasks claude-hook":                    true,
		"'/old/herdr-tasks' claude-hook":             true,
		`"/opt/acme/herdr-tasks" claude-hook`:        true,
		"/opt/acme/herdr-tasks claude-hook":          true,
		`"$HOME/.local/bin/herdr-tasks" claude-hook`: true,
		"  herdr-tasks  claude-hook  ":               true,
		"echo herdr-tasks claude-hook":               false,
		"/opt/acme/not-herdr-tasks claude-hook":      false,
		`"/x/not-herdr-tasks" claude-hook`:           false,
		"'/x/not-herdr-tasks' claude-hook":           false,
		"my-herdr-tasks claude-hook":                 false,
		"acme-sync; herdr-tasks claude-hook":         false,
		"FOO=1 herdr-tasks claude-hook":              false,
		"herdr-tasks claude-hook-v2":                 false,
		"herdr-tasks overview":                       false,
	} {
		if got := ourCommand.MatchString(c); got != want {
			t.Errorf("ourCommand on %q = %v, want %v", c, got, want)
		}
	}
	// Whatever Command writes is ours.
	for _, bin := range []string{bin, "/opt/my tools/herdr-tasks", `/opt/a"b\c/herdr-tasks`, "/opt/$release/herdr-tasks", "/tmp/$(id)/herdr-tasks", "/tmp/`id`/herdr-tasks"} {
		if c := Command(bin); !ourCommand.MatchString(c) {
			t.Errorf("ourCommand doesn't match Command(%q) = %q", bin, c)
		}
	}
}

func TestRemoveKeepsHooksThatOnlyMentionUs(t *testing.T) {
	src := `{
  "hooks": {
    "PreToolUse": [{"hooks": [{"type": "command", "command": "/opt/acme/not-herdr-tasks claude-hook"}]}],
    "Stop": [{"hooks": [
      {"type": "command", "command": "echo herdr-tasks claude-hook"},
      {"type": "command", "command": "herdr-tasks claude-hook"}
    ]}]
  }
}
`
	out, err := Remove([]byte(src))
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range []string{"/opt/acme/not-herdr-tasks claude-hook", "echo herdr-tasks claude-hook"} {
		if !strings.Contains(string(out), c) {
			t.Errorf("Remove took out %q:\n%s", c, out)
		}
	}
	if countOurs(t, out) != 0 {
		t.Errorf("our hook is still there:\n%s", out)
	}
	if out, err = Apply([]byte(src), Command(bin)); err != nil || !strings.Contains(string(out), "echo herdr-tasks claude-hook") {
		t.Errorf("Apply took out the echo hook (err %v):\n%s", err, out)
	}
}

func TestApplyRefusesJSONC(t *testing.T) {
	for _, src := range []string{
		"{\n  // Claude Code reads strict JSON\n  \"model\": \"opus\"\n}\n",
		"{\n  \"model\": \"opus\",\n}\n",
		`{"model": "opus"} and more`,
		`[]`,
		`{"hooks": []}`,
		`{"hooks": {"PostToolUse": {}}}`,
		`{"hooks": {"Stop": "acme-done"}}`,
	} {
		var pe *ParseError
		if _, err := Apply([]byte(src), Command(bin)); !errors.As(err, &pe) {
			t.Errorf("Apply(%q): err = %v, want a ParseError", src, err)
		}
		if _, err := Remove([]byte(src)); !errors.As(err, &pe) {
			t.Errorf("Remove(%q): err = %v, want a ParseError", src, err)
		}
	}
}

func TestApplyDoesntEscapeHTML(t *testing.T) {
	src := `{
  "hooks": {
    "Stop": [
      {
        "hooks": [
          {
            "type": "command",
            "command": "make lint && acme <notes >log"
          }
        ]
      }
    ]
  }
}
`
	out, err := Apply([]byte(src), Command("/opt/r&d/herdr-tasks"))
	if err != nil {
		t.Fatal(err)
	}
	s := string(out)
	if strings.Contains(s, `\u00`) || !strings.Contains(s, "make lint && acme <notes >log") || !strings.Contains(s, "/opt/r&d/herdr-tasks") {
		t.Fatalf("escaped:\n%s", out)
	}
}

func TestCommand(t *testing.T) {
	for bin, want := range map[string]string{
		"$HOME/.local/bin/herdr-tasks": `"$HOME/.local/bin/herdr-tasks" claude-hook`,
		"/opt/my tools/herdr-tasks":    `"/opt/my tools/herdr-tasks" claude-hook`,
		`/opt/a"b/herdr-tasks`:         `"/opt/a\"b/herdr-tasks" claude-hook`,
		"/opt/$release/herdr-tasks":    `"/opt/\$release/herdr-tasks" claude-hook`,
		"/tmp/$(id)/herdr-tasks":       `"/tmp/\$(id)/herdr-tasks" claude-hook`,
	} {
		if got := Command(bin); got != want || !ourCommand.MatchString(got) {
			t.Errorf("Command(%q) = %q, want %q", bin, got, want)
		}
	}
}

func TestCommandSurvivesTheShell(t *testing.T) {
	for bin, want := range map[string]string{
		"$HOME/.local/bin/herdr-tasks":  "/home/acme/.local/bin/herdr-tasks",
		"/opt/$release/herdr-tasks":     "/opt/$release/herdr-tasks",
		"/tmp/$(id)/herdr-tasks":        "/tmp/$(id)/herdr-tasks",
		"/tmp/`id`/herdr-tasks":         "/tmp/`id`/herdr-tasks",
		`/opt/a"b\c/herdr-tasks`:        `/opt/a"b\c/herdr-tasks`,
		"/opt/$HOME/herdr-tasks":        "/opt/$HOME/herdr-tasks",
		"/opt/my tools/r&d/herdr-tasks": "/opt/my tools/r&d/herdr-tasks",
	} {
		quoted := strings.TrimSuffix(Command(bin), " claude-hook")
		cmd := exec.Command("sh", "-c", "set -- "+quoted+`; printf %s "$1"`)
		cmd.Env = []string{"HOME=/home/acme", "PATH=" + os.Getenv("PATH")}
		out, err := cmd.Output()
		if err != nil || string(out) != want {
			t.Errorf("sh reads %s as %q (err %v), want %q", quoted, out, err, want)
		}
	}
}

func TestSettingsPath(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("CLAUDE_CONFIG_DIR", "")
	if got := SettingsPath(); got != filepath.Join(home, ".claude", "settings.json") {
		t.Errorf("SettingsPath = %q", got)
	}
	t.Setenv("CLAUDE_CONFIG_DIR", "/opt/acme/claude")
	if got := SettingsPath(); got != "/opt/acme/claude/settings.json" {
		t.Errorf("with CLAUDE_CONFIG_DIR, SettingsPath = %q", got)
	}
}

// files lists the names in dir.
func files(t *testing.T, dir string) []string {
	t.Helper()
	es, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, e := range es {
		names = append(names, e.Name())
	}
	return names
}

func TestWriteSettingsFollowsSymlink(t *testing.T) {
	dir := t.TempDir()
	real := filepath.Join(dir, "dotfiles", "settings.json")
	link := filepath.Join(dir, "claude", "settings.json")
	os.MkdirAll(filepath.Dir(real), 0o755)
	os.MkdirAll(filepath.Dir(link), 0o755)
	os.WriteFile(real, []byte(settings), 0o644)
	if err := os.Symlink(filepath.Join("..", "dotfiles", "settings.json"), link); err != nil {
		t.Fatal(err)
	}

	changed, err := WriteSettings(link, filepath.Join(dir, "state"), true, apply)
	if err != nil || !changed {
		t.Fatalf("changed %v, err %v", changed, err)
	}
	if fi, err := os.Lstat(link); err != nil || fi.Mode()&os.ModeSymlink == 0 {
		t.Fatalf("the link was replaced (err %v)", err)
	}
	b, _ := os.ReadFile(real)
	if countOurs(t, b) != installed {
		t.Fatalf("target not edited:\n%s", b)
	}
	if b, _ := os.ReadFile(real + ".before-herdr-tasks"); string(b) != settings {
		t.Fatalf("backup next to the target = %q", b)
	}
	if got := files(t, filepath.Dir(link)); !slices.Equal(got, []string{"settings.json"}) {
		t.Fatalf("files next to the link: %v", got)
	}
	if got := files(t, filepath.Dir(real)); !slices.Equal(got, []string{"settings.json", "settings.json.before-herdr-tasks"}) {
		t.Fatalf("files next to the target: %v", got)
	}
}

// withoutSwap runs the rest of the test as on a filesystem that can't swap
// files, where WriteSettings checks the file, then renames over it.
func withoutSwap(t *testing.T) {
	swap = func(string, string) error { return errNoSwap }
	t.Cleanup(func() { swap = swapFiles })
}

// meanwhile has fn run after WriteSettings last reads the file, just before
// it puts its own in place.
func meanwhile(t *testing.T, fn func()) {
	beforeReplace = fn
	t.Cleanup(func() { beforeReplace = func() {} })
}

func TestWriteSettingsRefusesAConcurrentChange(t *testing.T) {
	for _, canSwap := range []bool{true, false} {
		dir := t.TempDir()
		path := filepath.Join(dir, "settings.json")
		os.WriteFile(path, []byte(settings), 0o644)
		other := "{\n  \"model\": \"haiku\"\n}\n"
		meanwhile(t, func() { os.WriteFile(path, []byte(other), 0o644) })
		if !canSwap {
			withoutSwap(t)
		}

		changed, err := WriteSettings(path, filepath.Join(dir, "state"), true, apply)
		if err == nil || changed || !strings.Contains(err.Error(), "changed while herdr-tasks was editing it") {
			t.Fatalf("swap %v: want an error, got changed %v, err %v", canSwap, changed, err)
		}
		if b, _ := os.ReadFile(path); string(b) != other {
			t.Fatalf("swap %v: the other writer's change was lost:\n%s", canSwap, b)
		}
		if got := files(t, dir); !slices.Equal(got, []string{"settings.json", "settings.json.before-herdr-tasks", "state"}) {
			t.Fatalf("swap %v: files left: %v", canSwap, got)
		}
	}
}

// canSwap skips the test where the temp dir's filesystem can't swap files.
func canSwap(t *testing.T) {
	t.Helper()
	dir := t.TempDir()
	a, b := filepath.Join(dir, "a"), filepath.Join(dir, "b")
	os.WriteFile(a, nil, 0o644)
	os.WriteFile(b, nil, 0o644)
	if err := swapFiles(a, b); errors.Is(err, errNoSwap) {
		t.Skip("this filesystem can't swap files")
	} else if err != nil {
		t.Fatal(err)
	}
}

// keptIn is the file an error says a version of the settings is in.
func keptIn(t *testing.T, err error) string {
	t.Helper()
	i := strings.LastIndex(err.Error(), " is in ")
	if i < 0 {
		t.Fatalf("the error doesn't say where the other version is: %v", err)
	}
	return err.Error()[i+len(" is in "):]
}

// While WriteSettings puts back a version saved meanwhile, it's saved again:
// neither save is deleted, and the newest one ends up in place.
func TestWriteSettingsKeepsASecondConcurrentChange(t *testing.T) {
	canSwap(t)
	first := "{\n  \"model\": \"haiku\"\n}\n"
	second := "{\n  \"model\": \"sonnet\"\n}\n"
	for _, c := range []struct {
		name string
		save func(path string)
	}{
		{"by rename", func(path string) {
			os.WriteFile(path+".new", []byte(second), 0o644)
			os.Rename(path+".new", path)
		}},
		{"in place", func(path string) { os.WriteFile(path, []byte(second), 0o644) }},
	} {
		for _, swapBackFails := range []bool{false, true} {
			name := fmt.Sprintf("%s, swap back fails %v", c.name, swapBackFails)
			dir := t.TempDir()
			path := filepath.Join(dir, "settings.json")
			os.WriteFile(path, []byte(settings), 0o644)
			meanwhile(t, func() { os.WriteFile(path, []byte(first), 0o644) })
			calls := 0
			swap = func(a, b string) error {
				if calls++; calls == 2 {
					c.save(path)
					if swapBackFails {
						return errors.New("acme: swap failed")
					}
				}
				return swapFiles(a, b)
			}
			t.Cleanup(func() { swap = swapFiles })

			changed, err := WriteSettings(path, filepath.Join(dir, "state"), true, apply)
			if err == nil || changed || !strings.Contains(err.Error(), "changed twice while herdr-tasks was editing it") {
				t.Fatalf("%s: want an error, got changed %v, err %v", name, changed, err)
			}
			if b, _ := os.ReadFile(path); string(b) != second {
				t.Errorf("%s: the second save isn't in place:\n%s", name, b)
			}
			kept := keptIn(t, err)
			if real, _ := filepath.EvalSymlinks(dir); filepath.Dir(kept) != real {
				t.Fatalf("%s: the first save is kept in %s, outside %s", name, kept, real)
			}
			if b, _ := os.ReadFile(kept); string(b) != first {
				t.Errorf("%s: %s holds %q, want the first save", name, kept, b)
			}
			if got := files(t, dir); !slices.Equal(got, []string{filepath.Base(kept), "settings.json", "settings.json.before-herdr-tasks", "state"}) {
				t.Errorf("%s: files left: %v", name, got)
			}
		}
	}

	// A failed swap back with no second save puts the first one back.
	dir := t.TempDir()
	path := filepath.Join(dir, "settings.json")
	os.WriteFile(path, []byte(settings), 0o644)
	meanwhile(t, func() { os.WriteFile(path, []byte(first), 0o644) })
	calls := 0
	swap = func(a, b string) error {
		if calls++; calls == 2 {
			return errors.New("acme: swap failed")
		}
		return swapFiles(a, b)
	}
	changed, err := WriteSettings(path, filepath.Join(dir, "state"), true, apply)
	if err == nil || changed || !strings.Contains(err.Error(), "changed while herdr-tasks was editing it") {
		t.Fatalf("want an error, got changed %v, err %v", changed, err)
	}
	if b, _ := os.ReadFile(path); string(b) != first {
		t.Errorf("the first save isn't back:\n%s", b)
	}
	if got := files(t, dir); !slices.Equal(got, []string{"settings.json", "settings.json.before-herdr-tasks", "state"}) {
		t.Errorf("files left: %v", got)
	}
}

func TestWriteSettingsRefusesAFileMadeMeanwhile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "settings.json")
	other := "{\n  \"model\": \"haiku\"\n}\n"
	meanwhile(t, func() { os.WriteFile(path, []byte(other), 0o644) })

	changed, err := WriteSettings(path, filepath.Join(dir, "state"), true, apply)
	if err == nil || changed {
		t.Fatalf("want an error, got changed %v, err %v", changed, err)
	}
	if b, _ := os.ReadFile(path); string(b) != other {
		t.Fatalf("the other writer's file was replaced:\n%s", b)
	}
	if got := files(t, dir); !slices.Equal(got, []string{"settings.json", "state"}) {
		t.Fatalf("files left: %v", got)
	}
}

func TestWriteSettingsWithoutSwap(t *testing.T) {
	withoutSwap(t)
	dir := t.TempDir()
	path := filepath.Join(dir, "settings.json")
	os.WriteFile(path, []byte(settings), 0o644)
	if changed, err := WriteSettings(path, filepath.Join(dir, "state"), true, apply); !changed || err != nil {
		t.Fatalf("changed %v, err %v", changed, err)
	}
	if b, _ := os.ReadFile(path); countOurs(t, b) != installed || !strings.Contains(string(b), "acme-status") {
		t.Fatalf("not edited:\n%s", b)
	}
	if got := files(t, dir); !slices.Equal(got, []string{"settings.json", "settings.json.before-herdr-tasks", "state"}) {
		t.Fatalf("files left: %v", got)
	}
}

func TestWriteSettingsWaitsOutAnEmptyFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "settings.json")
	os.WriteFile(path, nil, 0o644)
	// An editor saving in place has emptied the file, and finishes.
	wait := settle
	settle = func() { os.WriteFile(path, []byte(noHooks), 0o644) }
	t.Cleanup(func() { settle = wait })

	if changed, err := WriteSettings(path, filepath.Join(dir, "state"), true, apply); !changed || err != nil {
		t.Fatalf("changed %v, err %v", changed, err)
	}
	b, _ := os.ReadFile(path)
	if countOurs(t, b) != installed || !strings.Contains(string(b), "acme-status") {
		t.Fatalf("the editor's save was lost:\n%s", b)
	}
	if b, _ := os.ReadFile(path + ".before-herdr-tasks"); string(b) != noHooks {
		t.Fatalf("backup = %q", b)
	}

	// A file that has been empty a while is just empty.
	settle = func() {}
	os.WriteFile(path, nil, 0o644)
	past := time.Now().Add(-time.Minute)
	os.Chtimes(path, past, past)
	if changed, err := WriteSettings(path, filepath.Join(dir, "state"), true, apply); !changed || err != nil {
		t.Fatalf("empty file: changed %v, err %v", changed, err)
	}
	if b, _ := os.ReadFile(path); countOurs(t, b) != installed {
		t.Fatalf("empty file not edited:\n%s", b)
	}
}

// A file emptied just now and still empty is probably being saved by a
// writer that's slow to finish. What the swap would displace looks like what
// was read, so the swap can't tell; it's refused.
func TestWriteSettingsRefusesAFileEmptiedJustNow(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "settings.json")
	os.WriteFile(path, nil, 0o644)
	wait := settle
	settle = func() {}
	t.Cleanup(func() { settle = wait })

	changed, err := WriteSettings(path, filepath.Join(dir, "state"), true, apply)
	if err == nil || changed || !strings.Contains(err.Error(), "changed while herdr-tasks was editing it") {
		t.Fatalf("want an error, got changed %v, err %v", changed, err)
	}
	if b, _ := os.ReadFile(path); len(b) != 0 {
		t.Errorf("the file was edited:\n%s", b)
	}
	if got := files(t, dir); !slices.Equal(got, []string{"settings.json", "state"}) {
		t.Errorf("files left: %v, want no backup", got)
	}

	// Remove has nothing to take out of an empty file, and says nothing.
	if changed, err := WriteSettings(path, filepath.Join(dir, "state"), false, Remove); changed || err != nil {
		t.Errorf("Remove: changed %v, err %v", changed, err)
	}
}

func TestWriteSettingsKeepsMode(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "settings.json")
	os.WriteFile(path, []byte(settings), 0o640)
	os.Chmod(path, 0o640)

	if _, err := WriteSettings(path, filepath.Join(dir, "state"), true, apply); err != nil {
		t.Fatal(err)
	}
	if fi, err := os.Stat(path); err != nil || fi.Mode().Perm() != 0o640 {
		t.Fatalf("mode = %v, err %v", fi.Mode().Perm(), err)
	}
}

func TestWriteSettingsDoesntCreateWhenOff(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "claude", "settings.json")
	state := filepath.Join(dir, "state")
	for _, edit := range []func([]byte) ([]byte, error){Remove, apply} {
		if changed, err := WriteSettings(path, state, false, edit); changed || err != nil {
			t.Fatalf("changed %v, err %v", changed, err)
		}
	}
	if _, err := os.Lstat(filepath.Dir(path)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("made %s", filepath.Dir(path))
	}

	// With the hook on, the file is made.
	if changed, err := WriteSettings(path, state, true, apply); !changed || err != nil {
		t.Fatalf("changed %v, err %v", changed, err)
	}
	b, _ := os.ReadFile(path)
	if countOurs(t, b) != installed {
		t.Fatalf("new file:\n%s", b)
	}
	if fi, _ := os.Stat(path); fi.Mode().Perm() != 0o644 {
		t.Fatalf("mode = %v", fi.Mode().Perm())
	}
	if got := files(t, filepath.Dir(path)); !slices.Equal(got, []string{"settings.json"}) {
		t.Fatalf("files: %v", got)
	}
}

func TestWriteSettingsKeepsTheFirstBackup(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "settings.json")
	state := filepath.Join(dir, "state")
	os.WriteFile(path, []byte(noHooks), 0o644)

	for i, step := range []struct {
		edit    func([]byte) ([]byte, error)
		changed bool
	}{{apply, true}, {apply, false}, {Remove, true}, {Remove, false}} {
		if changed, err := WriteSettings(path, state, false, step.edit); changed != step.changed || err != nil {
			t.Fatalf("step %d: changed %v, err %v", i, changed, err)
		}
	}
	if b, _ := os.ReadFile(path); string(b) != noHooks {
		t.Fatalf("not restored:\n%s", b)
	}
	if b, _ := os.ReadFile(path + ".before-herdr-tasks"); string(b) != noHooks {
		t.Fatalf("backup = %q", b)
	}
}

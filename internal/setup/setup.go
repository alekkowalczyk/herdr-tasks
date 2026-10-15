// Package setup installs what a herdr plugin can't declare in its manifest:
// the herdr-tasks command on PATH, and herdr config lines for the key
// binding, the tab bar status, and the sidebar counts. Config changes live in
// marked blocks so they can be taken out again.
package setup

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/BurntSushi/toml"
)

const (
	beginMark   = "# >>> herdr-tasks (added by the herdr-tasks plugin; `herdr-tasks remove` takes it out)"
	endMark     = "# <<< herdr-tasks"
	OverviewKey = "prefix+t"
	StripKey    = "prefix+i"
	SidebarKey  = "prefix+a"
)

// ConfigPath is herdr's config file.
func ConfigPath() string {
	if p := os.Getenv("HERDR_CONFIG_PATH"); p != "" {
		return p
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".config", "herdr", "config.toml")
}

// BinDir is where the herdr-tasks command gets linked.
func BinDir() string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".local", "bin")
}

// InstallBinary copies exe to ~/.local/bin/herdr-tasks. It copies rather
// than links because herdr builds plugins in a temporary folder and moves
// them afterwards. A marker file in stateDir records that the copy is ours,
// so an unrelated herdr-tasks file is never overwritten or removed. It
// returns the command herdr should run for the tab bar status, plus
// anything worth telling you.
func InstallBinary(exe, stateDir string) (command string, notes []string, err error) {
	dir := BinDir()
	dst := filepath.Join(dir, "herdr-tasks")
	if !ours(dst, stateDir) {
		return exe, []string{dst + " already exists and wasn't installed by herdr-tasks, so it was left alone."}, nil
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", nil, err
	}
	b, err := os.ReadFile(exe)
	if err != nil {
		return "", nil, err
	}
	tmp := dst + ".tmp"
	if err := os.WriteFile(tmp, b, 0o755); err != nil {
		return "", nil, err
	}
	if err := os.Rename(tmp, dst); err != nil {
		return "", nil, err
	}
	if err := os.MkdirAll(stateDir, 0o755); err != nil {
		return "", nil, err
	}
	if err := os.WriteFile(filepath.Join(stateDir, "installed-binary"), []byte(dst+"\n"), 0o644); err != nil {
		return "", nil, err
	}
	if !onPath(dir) {
		notes = append(notes, dir+" isn't on your PATH, so agents can't run herdr-tasks by name yet.")
	}
	return "$HOME/.local/bin/herdr-tasks", notes, nil
}

// RemoveBinary deletes ~/.local/bin/herdr-tasks if herdr-tasks installed it.
func RemoveBinary(stateDir string) error {
	dst := filepath.Join(BinDir(), "herdr-tasks")
	if _, err := os.Lstat(dst); errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if !ours(dst, stateDir) {
		return nil
	}
	_ = os.Remove(filepath.Join(stateDir, "installed-binary"))
	return os.Remove(dst)
}

// ours reports whether dst is missing, a link (from older versions), or a
// copy herdr-tasks installed.
func ours(dst, stateDir string) bool {
	fi, err := os.Lstat(dst)
	if errors.Is(err, os.ErrNotExist) || (err == nil && fi.Mode()&os.ModeSymlink != 0) {
		return true
	}
	b, err := os.ReadFile(filepath.Join(stateDir, "installed-binary"))
	return err == nil && strings.TrimSpace(string(b)) == dst
}

func onPath(dir string) bool {
	for _, p := range filepath.SplitList(os.Getenv("PATH")) {
		if filepath.Clean(p) == filepath.Clean(dir) {
			return true
		}
	}
	return false
}

// RemoveBlocks takes every herdr-tasks block out of a config.
func RemoveBlocks(src string) string {
	var out []string
	inside := false
	for _, line := range strings.SplitAfter(src, "\n") {
		trimmed := strings.TrimSpace(line)
		switch {
		case strings.HasPrefix(trimmed, "# >>> herdr-tasks"):
			inside = true
			// Drop the blank line we put before an appended block.
			if n := len(out); n > 0 && strings.TrimSpace(out[n-1]) == "" {
				out = out[:n-1]
			}
		case inside && trimmed == endMark:
			inside = false
		case !inside:
			out = append(out, line)
		}
	}
	return strings.Join(out, "")
}

// countsRow is the sidebar row with a colored count per stage. With the task
// list on, it only counts the stages the list leaves out.
const countsRow = `[{ token = "$tasks_active", fg = "#62d39d" }, { token = "$tasks_review", fg = "#b9a6ff" }, { token = "$tasks_paused", fg = "#e7b75a" }, { token = "$tasks_parked", fg = "#93a5c6" }, { token = "$tasks_done", fg = "#6f7884" }]`

// listRows are the sidebar rows listing a space's tasks: one token per kind
// of row, so priority, asking, and your-turn tasks stand out. See
// reconcile.ListTokens.
func listRows() []string {
	var rows []string
	for i := 1; i <= 7; i++ {
		rows = append(rows, fmt.Sprintf(`  [{ token = "$t%[1]d_pri", fg = "#ff5f6d" }, { token = "$t%[1]d_ask", fg = "#ff5f6d", bold = true }, { token = "$t%[1]d_you", fg = "#ff8d6e" }, "$t%[1]d"],`, i))
	}
	return rows
}

var uiHeader = regexp.MustCompile(`(?m)^[ \t]*\[ui\][ \t]*(#.*)?$`)

// Apply adds herdr-tasks config to src and returns the new config, plus a
// note for each part it left out because you already configure it yourself.
func Apply(src, statusCommand string) (string, []string, error) {
	src = RemoveBlocks(src)
	var cfg map[string]any
	if _, err := toml.Decode(src, &cfg); err != nil {
		return "", nil, fmt.Errorf("your herdr config doesn't parse, so it was left alone: %w", err)
	}
	ui, _ := cfg["ui"].(map[string]any)
	sidebar, _ := ui["sidebar"].(map[string]any)
	_, hasTabBarRight := ui["tab_bar_right"]
	_, hasSpaces := sidebar["spaces"]

	var notes []string
	status := fmt.Sprintf(`tab_bar_right = [{ type = "command", command = %q, interval_seconds = 3 }]`, statusCommand+" status")
	if hasTabBarRight {
		notes = append(notes, fmt.Sprintf("You already set ui.tab_bar_right. Add this entry to it for the task status: { type = \"command\", command = %q, interval_seconds = 3 }", statusCommand+" status"))
	}
	if hasSpaces {
		notes = append(notes, "You already set ui.sidebar.spaces. Add this row to its rows for stage counts: "+countsRow)
	}
	keyTaken := usesKey(cfg["keys"], OverviewKey)
	if keyTaken {
		notes = append(notes, "ctrl+b t is already bound in your config, so the overview has no key. Bind the plugin action herdr-tasks.overview to a free key.")
	}
	stripKeyTaken := usesKey(cfg["keys"], StripKey)
	if stripKeyTaken {
		notes = append(notes, "ctrl+b i is already bound in your config, so the strip has no key. Bind the plugin action herdr-tasks.strip to a free key.")
	}
	sidebarKeyTaken := usesKey(cfg["keys"], SidebarKey)
	if sidebarKeyTaken {
		notes = append(notes, "ctrl+b a is already bound in your config, so switching the sidebar between tasks and counts has no key. Bind the plugin action herdr-tasks.sidebar to a free key.")
	}

	out := src
	var tail []string
	if !hasTabBarRight {
		block := beginMark + "\n" + status + "\n" + endMark + "\n"
		if loc := uiHeader.FindStringIndex(out); loc != nil {
			// Inside your [ui] section, right after its header.
			at := loc[1]
			if at < len(out) && out[at] == '\n' {
				at++
			} else {
				block = "\n" + block
			}
			out = out[:at] + block + out[at:]
		} else {
			tail = append(tail, "[ui]", status, "")
		}
	}
	if !keyTaken {
		tail = append(tail,
			"# ctrl+b t opens the task overview",
			"[[keys.command]]",
			`key = "`+OverviewKey+`"`,
			`type = "plugin_action"`,
			`command = "herdr-tasks.overview"`,
			`description = "task overview"`,
			"")
	}
	if !sidebarKeyTaken {
		tail = append(tail,
			"# ctrl+b a switches the sidebar between each space's tasks and stage counts",
			"[[keys.command]]",
			`key = "`+SidebarKey+`"`,
			`type = "plugin_action"`,
			`command = "herdr-tasks.sidebar"`,
			`description = "sidebar tasks or counts"`,
			"")
	}
	if !stripKeyTaken {
		tail = append(tail,
			"# ctrl+b i shows or hides the task strip at the bottom of the tab",
			"[[keys.command]]",
			`key = "`+StripKey+`"`,
			`type = "plugin_action"`,
			`command = "herdr-tasks.strip"`,
			`description = "task strip"`,
			"")
	}
	if !hasSpaces {
		tail = append(tail,
			"# each space's tasks, most urgent first, then counts of the rest",
			"[ui.sidebar.spaces]",
			"rows = [",
			`  ["state_icon", "workspace"],`,
			`  ["branch", "git_status"],`)
		tail = append(tail, listRows()...)
		tail = append(tail, "  "+countsRow+",", "]", "")
	}
	if len(tail) > 0 {
		if out != "" && !strings.HasSuffix(out, "\n") {
			out += "\n"
		}
		out += "\n" + beginMark + "\n" + strings.Join(tail, "\n") + endMark + "\n"
	}

	var check map[string]any
	if _, err := toml.Decode(out, &check); err != nil {
		return "", nil, fmt.Errorf("adding the herdr-tasks lines would break your config, so it was left alone: %w", err)
	}
	return out, notes, nil
}

// usesKey reports whether any binding under [keys] is set to key.
func usesKey(v any, key string) bool {
	switch v := v.(type) {
	case string:
		return v == key
	case map[string]any:
		for _, x := range v {
			if usesKey(x, key) {
				return true
			}
		}
	case []any:
		for _, x := range v {
			if usesKey(x, key) {
				return true
			}
		}
	case []map[string]any:
		for _, x := range v {
			if usesKey(x, key) {
				return true
			}
		}
	}
	return false
}

// WriteConfig saves a new config, keeping a one-time backup of the original.
func WriteConfig(path, before, after string) (backup string, err error) {
	if before == after {
		return "", nil
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return "", err
	}
	mode := os.FileMode(0o644)
	if fi, err := os.Stat(path); err == nil {
		mode = fi.Mode().Perm()
		backup = path + ".before-herdr-tasks"
		if _, err := os.Stat(backup); errors.Is(err, os.ErrNotExist) {
			if err := os.WriteFile(backup, []byte(before), mode); err != nil {
				return "", err
			}
		} else {
			backup = ""
		}
	}
	tmp := path + ".herdr-tasks.tmp"
	if err := os.WriteFile(tmp, []byte(after), mode); err != nil {
		return "", err
	}
	return backup, os.Rename(tmp, path)
}

// ReadConfig returns herdr's config, or "" if there isn't one yet.
func ReadConfig(path string) (string, error) {
	b, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return "", nil
	}
	return string(b), err
}

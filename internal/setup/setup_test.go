package setup

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/BurntSushi/toml"
)

const cmd = "$HOME/.local/bin/herdr-tasks"

func decode(t *testing.T, s string) map[string]any {
	t.Helper()
	var m map[string]any
	if _, err := toml.Decode(s, &m); err != nil {
		t.Fatalf("result doesn't parse: %v\n%s", err, s)
	}
	return m
}

func TestApplyGoesInsideYourUISection(t *testing.T) {
	src := "[ui]\ntheme = \"dark\"\n\n[keys]\ncycle_pane_next = \"prefix+o\"\n"
	out, notes, err := Apply(src, cmd)
	if err != nil {
		t.Fatal(err)
	}
	if len(notes) != 0 {
		t.Fatalf("unexpected notes: %v", notes)
	}
	m := decode(t, out)
	ui := m["ui"].(map[string]any)
	if ui["theme"] != "dark" || ui["tab_bar_right"] == nil {
		t.Fatalf("ui = %v", ui)
	}
	if _, ok := ui["sidebar"].(map[string]any)["spaces"]; !ok {
		t.Fatal("sidebar rows missing")
	}
	keys := m["keys"].(map[string]any)
	if keys["cycle_pane_next"] != "prefix+o" || len(keys["command"].([]map[string]any)) != 3 {
		t.Fatalf("keys = %v", keys)
	}
	if !strings.HasPrefix(out, "[ui]\n"+beginMark) {
		t.Fatalf("status line should sit right under [ui]:\n%s", out)
	}
}

func TestApplyTwiceIsTheSame(t *testing.T) {
	src := "[ui]\ntheme = \"dark\"\n"
	once, _, _ := Apply(src, cmd)
	twice, _, err := Apply(once, cmd)
	if err != nil || once != twice {
		t.Fatalf("second apply changed the config (err %v):\n%s\n---\n%s", err, once, twice)
	}
}

func TestRemoveRestoresTheOriginal(t *testing.T) {
	for _, src := range []string{
		"[ui]\ntheme = \"dark\"\n\n[keys]\ncycle_pane_next = \"prefix+o\"\n",
		"[keys]\nnew_tab = \"prefix+c\"\n",
		"",
	} {
		out, _, err := Apply(src, cmd)
		if err != nil {
			t.Fatal(err)
		}
		if back := RemoveBlocks(out); back != src {
			t.Fatalf("remove didn't restore:\n%q\n---\n%q", src, back)
		}
	}
}

func TestApplyWithoutUISection(t *testing.T) {
	out, _, err := Apply("[keys]\nnew_tab = \"prefix+c\"\n", cmd)
	if err != nil {
		t.Fatal(err)
	}
	m := decode(t, out)
	if m["ui"].(map[string]any)["tab_bar_right"] == nil {
		t.Fatal("tab_bar_right missing")
	}
}

func TestApplyLeavesYourOwnSettingsAlone(t *testing.T) {
	src := `[ui]
tab_bar_right = [{ type = "hostname" }]

[ui.sidebar.spaces]
rows = [["workspace"]]

[keys]
toggle_sidebar = "prefix+t"
zoom = "prefix+i"
new_workspace = "prefix+a"
`
	out, notes, err := Apply(src, cmd)
	if err != nil {
		t.Fatal(err)
	}
	if out != src {
		t.Fatalf("config changed though everything was already set:\n%s", out)
	}
	if len(notes) != 5 {
		t.Fatalf("want 5 notes, got %v", notes)
	}
}

func TestApplyRefusesABrokenConfig(t *testing.T) {
	if _, _, err := Apply("[ui\nbroken", cmd); err == nil {
		t.Fatal("a config that doesn't parse should be left alone")
	}
}

func TestInstallBinaryLeavesOtherFilesAlone(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	state := filepath.Join(home, "state")
	exe := filepath.Join(home, "build", "herdr-tasks")
	os.MkdirAll(filepath.Dir(exe), 0o755)
	os.WriteFile(exe, []byte("v1"), 0o755)

	dst := filepath.Join(home, ".local", "bin", "herdr-tasks")
	if cmd, _, err := InstallBinary(exe, state); err != nil || cmd != "$HOME/.local/bin/herdr-tasks" {
		t.Fatalf("install: %q %v", cmd, err)
	}
	// The build folder goes away; the installed copy must still work.
	os.RemoveAll(filepath.Dir(exe))
	if b, _ := os.ReadFile(dst); string(b) != "v1" {
		t.Fatalf("installed copy = %q", b)
	}
	os.MkdirAll(filepath.Dir(exe), 0o755)
	os.WriteFile(exe, []byte("v2"), 0o755)
	InstallBinary(exe, state)
	if b, _ := os.ReadFile(dst); string(b) != "v2" {
		t.Fatalf("update didn't replace our copy: %q", b)
	}
	if err := RemoveBinary(state); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(dst); !os.IsNotExist(err) {
		t.Fatal("remove left our copy behind")
	}

	// Someone else's herdr-tasks is never touched.
	os.WriteFile(dst, []byte("theirs"), 0o755)
	if cmd, notes, _ := InstallBinary(exe, state); cmd != exe || len(notes) != 1 {
		t.Fatalf("overwrote a foreign file? cmd %q notes %v", cmd, notes)
	}
	RemoveBinary(state)
	if b, _ := os.ReadFile(dst); string(b) != "theirs" {
		t.Fatal("removed a foreign file")
	}
}

func TestSidebarRowsFitHerdrsLimits(t *testing.T) {
	out, _, err := Apply("", cmd)
	if err != nil {
		t.Fatal(err)
	}
	m := decode(t, out)
	rows := m["ui"].(map[string]any)["sidebar"].(map[string]any)["spaces"].(map[string]any)["rows"].([]any)
	if len(rows) > 16 {
		t.Fatalf("%d rows; herdr allows 16", len(rows))
	}
	for _, r := range rows {
		if len(r.([]any)) > 16 {
			t.Fatalf("row with %d tokens; herdr allows 16", len(r.([]any)))
		}
	}
}

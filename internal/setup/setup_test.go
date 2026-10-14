package setup

import (
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
	if keys["cycle_pane_next"] != "prefix+o" || len(keys["command"].([]map[string]any)) != 1 {
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
`
	out, notes, err := Apply(src, cmd)
	if err != nil {
		t.Fatal(err)
	}
	if out != src {
		t.Fatalf("config changed though everything was already set:\n%s", out)
	}
	if len(notes) != 3 {
		t.Fatalf("want 3 notes, got %v", notes)
	}
}

func TestApplyRefusesABrokenConfig(t *testing.T) {
	if _, _, err := Apply("[ui\nbroken", cmd); err == nil {
		t.Fatal("a config that doesn't parse should be left alone")
	}
}

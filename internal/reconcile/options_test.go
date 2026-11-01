package reconcile

import (
	"os"
	"path/filepath"
	"testing"
)

func TestNewSettingsAreOffByDefault(t *testing.T) {
	t.Setenv("HERDR_PLUGIN_CONFIG_DIR", t.TempDir())
	opt, err := LoadOptions()
	if err != nil {
		t.Fatal(err)
	}
	if opt.Board || opt.ClaudeLinks {
		t.Fatalf("board and claude_links should be off: %+v", opt)
	}
	if opt.Port() != 7391 {
		t.Fatalf("port = %d, want 7391", opt.Port())
	}
	if !opt.Markers || !opt.SortTabs || !opt.Strip {
		t.Fatalf("markers, sort_tabs and strip should stay on: %+v", opt)
	}
}

func TestSettingsTurnOn(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("HERDR_PLUGIN_CONFIG_DIR", dir)
	conf := "board = true\nclaude_links = true\nboard_port = 7777\n"
	if err := os.WriteFile(filepath.Join(dir, "config.toml"), []byte(conf), 0o644); err != nil {
		t.Fatal(err)
	}
	opt, err := LoadOptions()
	if err != nil {
		t.Fatal(err)
	}
	if !opt.Board || !opt.ClaudeLinks {
		t.Fatalf("board and claude_links should be on: %+v", opt)
	}
	if opt.Port() != 7777 {
		t.Fatalf("port = %d, want 7777", opt.Port())
	}
}

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
	if opt.Board || opt.ClaudeLinks || opt.BoardTailnet {
		t.Fatalf("board, claude_links and board_tailnet should be off: %+v", opt)
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
	conf := "board = true\nclaude_links = true\nboard_port = 7777\nboard_tailnet = true\n"
	if err := os.WriteFile(filepath.Join(dir, "config.toml"), []byte(conf), 0o644); err != nil {
		t.Fatal(err)
	}
	opt, err := LoadOptions()
	if err != nil {
		t.Fatal(err)
	}
	if !opt.Board || !opt.ClaudeLinks || !opt.BoardTailnet {
		t.Fatalf("board, claude_links and board_tailnet should be on: %+v", opt)
	}
	if opt.Port() != 7777 {
		t.Fatalf("port = %d, want 7777", opt.Port())
	}
}

func TestSlackDefaults(t *testing.T) {
	t.Setenv("HERDR_PLUGIN_CONFIG_DIR", t.TempDir())
	opt, err := LoadOptions()
	if err != nil {
		t.Fatal(err)
	}
	if opt.SlackWatch || opt.SlackWorkspace != "Slack inbox" || !opt.SlackDMs ||
		opt.SlackEvery != "5m" || opt.SlackHours != "Mon-Fri 08:00-18:00" || opt.SlackMaxNew != 4 || opt.SlackTopic != "" {
		t.Fatalf("slack defaults wrong: %+v", opt)
	}
}

func TestSlackOverrides(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("HERDR_PLUGIN_CONFIG_DIR", dir)
	conf := "slack_watch = true\nslack_dms = false\nslack_skip_channels = [\"random\", \"C0123ABCD\"]\nslack_max_new = 0\n" +
		"slack_topic = \"the acme billing service\"\n"
	if err := os.WriteFile(filepath.Join(dir, "config.toml"), []byte(conf), 0o644); err != nil {
		t.Fatal(err)
	}
	opt, err := LoadOptions()
	if err != nil {
		t.Fatal(err)
	}
	if !opt.SlackWatch || opt.SlackDMs || opt.SlackMaxNew != 0 ||
		len(opt.SlackSkipChannels) != 2 || opt.SlackSkipChannels[1] != "C0123ABCD" || opt.SlackTopic != "the acme billing service" {
		t.Fatalf("slack overrides not read: %+v", opt)
	}
}

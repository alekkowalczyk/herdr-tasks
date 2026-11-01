package main

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	"github.com/alekkowalczyk/herdr-tasks/internal/claudehook"
	"github.com/alekkowalczyk/herdr-tasks/internal/setup"
)

// installed is what setup leaves for remove to take away, in a temp HOME.
type installed struct {
	home, state, bin, config, settings string
	// calls is where the fake herdr writes each command it's given.
	calls string
}

// install sets up herdr-tasks in a temp HOME: a copy on PATH, the
// herdr-tasks lines in herdr's config, the Claude links hook in Claude's
// settings, and the board's token. herdr is a script that only records its
// calls, and no herdr session is running.
func install(t *testing.T) installed {
	t.Helper()
	home := t.TempDir()
	in := installed{
		home:     home,
		state:    filepath.Join(home, "state"),
		bin:      filepath.Join(home, ".local", "bin", "herdr-tasks"),
		config:   filepath.Join(home, "herdr", "config.toml"),
		settings: filepath.Join(home, "claude", "settings.json"),
		calls:    filepath.Join(home, "herdr-calls"),
	}
	t.Setenv("HOME", home)
	t.Setenv("HERDR_PLUGIN_STATE_DIR", in.state)
	t.Setenv("HERDR_CONFIG_PATH", in.config)
	t.Setenv("CLAUDE_CONFIG_DIR", filepath.Dir(in.settings))
	t.Setenv("HERDR_SOCKET_PATH", "")
	herdrBin := filepath.Join(home, "fake-herdr")
	write(t, herdrBin, "#!/bin/sh\necho \"$@\" >> '"+in.calls+"'\n", 0o755)
	t.Setenv("HERDR_BIN_PATH", herdrBin)

	exe := filepath.Join(home, "build", "herdr-tasks")
	write(t, exe, "v1", 0o755)
	if _, _, err := setup.InstallBinary(exe, in.state); err != nil {
		t.Fatal(err)
	}
	cfg, _, err := setup.Apply("", "$HOME/.local/bin/herdr-tasks")
	if err != nil {
		t.Fatal(err)
	}
	write(t, in.config, cfg, 0o644)
	settings, err := claudehook.Apply(nil, claudehook.Command("$HOME/.local/bin/herdr-tasks"))
	if err != nil {
		t.Fatal(err)
	}
	write(t, in.settings, string(settings), 0o644)
	write(t, filepath.Join(in.state, "board-token"), "0123abcd\n", 0o600)
	return in
}

func write(t *testing.T, path, data string, mode os.FileMode) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(data), mode); err != nil {
		t.Fatal(err)
	}
}

func read(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// untouched fails the test unless everything install left is still there.
func (in installed) untouched(t *testing.T, config, settings string) {
	t.Helper()
	if _, err := os.Stat(in.bin); err != nil {
		t.Errorf("the copy on PATH is gone: %v", err)
	}
	if got := setup.InstalledBinary(in.state); got != in.bin {
		t.Errorf("InstalledBinary = %q, want %q", got, in.bin)
	}
	if _, err := os.Stat(filepath.Join(in.state, "board-token")); err != nil {
		t.Errorf("the board's token is gone: %v", err)
	}
	if got := read(t, in.config); got != config {
		t.Errorf("herdr config changed:\n%s", got)
	}
	if got := read(t, in.settings); got != settings {
		t.Errorf("Claude settings changed:\n%s", got)
	}
	if b, err := os.ReadFile(in.calls); err == nil {
		t.Errorf("herdr was run: %q", b)
	}
}

func TestRemoveStopsWhenTheBoardDoesnt(t *testing.T) {
	in := install(t)
	config, settings := read(t, in.config), read(t, in.settings)
	// A board server that holds its lock and doesn't answer.
	lock := filepath.Join(in.state, "board", "board.lock")
	write(t, lock, "", 0o600)
	f, err := os.OpenFile(lock, os.O_RDWR, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX); err != nil {
		t.Fatal(err)
	}

	err = (&app{}).remove(false)
	if err == nil || !strings.Contains(err.Error(), "nothing was removed") {
		t.Fatalf("remove = %v, want an error saying nothing was removed", err)
	}
	in.untouched(t, config, settings)
}

func TestRemoveStopsWhenTheHookCantComeOut(t *testing.T) {
	in := install(t)
	config, settings := read(t, in.config), read(t, in.settings)
	conflict := errors.New(in.settings + " changed while herdr-tasks was editing it, so the Claude links hook wasn't changed; try again")
	writeSettings = func(string, string, bool, func([]byte) ([]byte, error)) (bool, error) { return false, conflict }
	t.Cleanup(func() { writeSettings = claudehook.WriteSettings })

	err := (&app{}).remove(false)
	if !errors.Is(err, conflict) {
		t.Fatalf("remove = %v, want the conflict", err)
	}
	// So "try again" works: herdr-tasks is still there to run.
	in.untouched(t, config, settings)
}

func TestRemoveGoesOnWhenClaudeSettingsDontParse(t *testing.T) {
	in := install(t)
	write(t, in.settings, "{ not json", 0o644)

	if err := (&app{}).remove(false); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(in.bin); !os.IsNotExist(err) {
		t.Errorf("the copy on PATH is still there: %v", err)
	}
	if got := read(t, in.calls); !strings.Contains(got, "plugin uninstall herdr-tasks") {
		t.Errorf("herdr calls = %q, want the plugin uninstalled", got)
	}
	if got := read(t, in.config); strings.Contains(got, "herdr-tasks") {
		t.Errorf("herdr config still has herdr-tasks lines:\n%s", got)
	}
	if got := read(t, in.settings); got != "{ not json" {
		t.Errorf("Claude settings = %q, want them left alone", got)
	}
}

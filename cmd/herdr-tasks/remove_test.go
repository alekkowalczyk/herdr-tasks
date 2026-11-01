package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"testing"

	"github.com/alekkowalczyk/herdr-tasks/internal/claudehook"
	"github.com/alekkowalczyk/herdr-tasks/internal/setup"
	"github.com/alekkowalczyk/herdr-tasks/internal/tailnet"
)

// installed is what setup leaves for remove to take away, in a temp HOME.
type installed struct {
	home, state, bin, config, settings string
	// calls is where the fake herdr writes each command it's given.
	calls string
	// tailscale is the tailscale command remove runs.
	tailscale *fakeTailscale
}

// fakeTailscale is the tailscale command in memory: what it serves, and the
// calls that change that, as "serve <port> <target>" and "off <port>". Off
// fails with offErr when it's set.
type fakeTailscale struct {
	served map[int]string
	calls  []string
	offErr error
}

func (f *fakeTailscale) Self(context.Context) (tailnet.Self, error) {
	return tailnet.Self{DNSName: "devbox.tail0000.ts.net", Owner: "dana@acme.test"}, nil
}

func (f *fakeTailscale) Served(context.Context) (map[int]string, error) {
	return maps.Clone(f.served), nil
}

func (f *fakeTailscale) Serve(_ context.Context, port int, target string) error {
	f.calls = append(f.calls, fmt.Sprintf("serve %d %s", port, target))
	f.served[port] = target
	return nil
}

func (f *fakeTailscale) Off(_ context.Context, port int) error {
	f.calls = append(f.calls, fmt.Sprintf("off %d", port))
	if f.offErr != nil {
		return fmt.Errorf("tailscale serve --https=%d off: %w", port, f.offErr)
	}
	delete(f.served, port)
	return nil
}

// install sets up herdr-tasks in a temp HOME: a copy on PATH, the
// herdr-tasks lines in herdr's config, the Claude links hook in Claude's
// settings, and the board's token. herdr is a script that only records its
// calls, and no herdr session is running. tailscale is a fake that serves
// nothing.
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
	in.tailscale = &fakeTailscale{served: map[int]string{}}
	was := tailscaleCLI
	tailscaleCLI = in.tailscale
	t.Cleanup(func() { tailscaleCLI = was })
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

// remove takes out the tailscale serve entries the board made, and only
// those still pointing where it recorded.
func TestRemoveUnsharesOnlyOwnEntries(t *testing.T) {
	in := install(t)
	sockets := filepath.Join(in.state, "board", "sockets")
	ours := "unix:" + filepath.Join(sockets, "board.sock")
	record := filepath.Join(in.state, "board", "tailnet.json")
	write(t, record, fmt.Sprintf(`{"board":{"port":7391,"target":%q,"at":"2026-11-01T09:00:00Z"},`+
		`"shares":[{"port":4100,"target":%q,"at":"2026-11-01T09:05:00Z"}]}`, ours, "unix:"+filepath.Join(sockets, "share-4100.sock")), 0o600)
	in.tailscale.served = map[int]string{7391: ours, 4100: "http://127.0.0.1:4100"}

	if err := (&app{}).remove(false); err != nil {
		t.Fatal(err)
	}
	if want := []string{"off 7391"}; !slices.Equal(in.tailscale.calls, want) {
		t.Errorf("tailscale was asked %q, want %q", in.tailscale.calls, want)
	}
	if _, err := os.Stat(record); !os.IsNotExist(err) {
		t.Errorf("the record is still there: %v", err)
	}
}

// When Tailscale doesn't take its entries out, remove says how to, with
// each port. With --purge the record goes, so that's the only trace left.
// The note is the same without --purge, which is what runs here: --purge
// deletes the Linear key from the real keychain on macOS.
func TestRemoveSaysHowToUnshareWhenTailscaleFails(t *testing.T) {
	in := install(t)
	sockets := filepath.Join(in.state, "board", "sockets")
	ours := "unix:" + filepath.Join(sockets, "board.sock")
	share := "unix:" + filepath.Join(sockets, "share-4100.sock")
	write(t, filepath.Join(in.state, "board", "tailnet.json"), fmt.Sprintf(`{"board":{"port":7391,"target":%q,"at":"2026-11-01T09:00:00Z"},`+
		`"shares":[{"port":4100,"target":%q,"at":"2026-11-01T09:05:00Z"}]}`, ours, share), 0o600)
	in.tailscale.served = map[int]string{7391: ours, 4100: share}
	in.tailscale.offErr = errors.New("timed out after 10s")

	var err error
	out := stdout(t, func() { err = (&app{}).remove(false) })
	if err != nil {
		t.Fatal(err)
	}
	want := "herdr-tasks: note: couldn't take the board off your tailnet: " +
		"tailscale serve --https=7391 off: timed out after 10s; tailscale serve --https=4100 off: timed out after 10s. " +
		"Check tailscale serve status, and take out the entries herdr-tasks made with: " +
		"tailscale serve --https=7391 off; tailscale serve --https=4100 off\n"
	if !strings.Contains(out, want) {
		t.Errorf("printed:\n%s\nwant a line:\n%s", out, want)
	}
}

// stdout is what f prints on stdout.
func stdout(t *testing.T, f func()) string {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	was := os.Stdout
	os.Stdout = w
	got := make(chan string)
	go func() {
		b, _ := io.ReadAll(r)
		got <- string(b)
	}()
	defer func() {
		os.Stdout = was
		w.Close()
	}()
	f()
	os.Stdout = was
	w.Close()
	return <-got
}

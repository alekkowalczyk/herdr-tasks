package main

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/alekkowalczyk/herdr-tasks/internal/board"
	"github.com/alekkowalczyk/herdr-tasks/internal/linear"
	"github.com/alekkowalczyk/herdr-tasks/internal/reconcile"
)

const (
	testConfigDir = "/config/herdr-tasks"
	testURL       = "http://127.0.0.1:7391/?code=0123abcd"
)

// fakeBoard is the board command with a fake server and browser. calls lists
// what it asked them to do, and shown what it told you.
type fakeBoard struct {
	boardCmd
	calls []string
	shown []string
	out   bytes.Buffer
}

func newFakeBoard(t *testing.T, opts reconcile.Options) *fakeBoard {
	t.Helper()
	t.Setenv("HERDR_PLUGIN_CONFIG_DIR", testConfigDir)
	f := &fakeBoard{}
	call := func(format string, args ...any) { f.calls = append(f.calls, fmt.Sprintf(format, args...)) }
	f.boardCmd = boardCmd{
		opts: opts,
		dir:  "/state",
		exe:  "/plugin/bin/herdr-tasks",
		ensure: func(dir string, port int, exe, v string) error {
			call("ensure %s %d %s %s", dir, port, exe, v)
			return nil
		},
		rotate: func(dir string, port int, exe, v string) error {
			call("rotate %s %d %s %s", dir, port, exe, v)
			return nil
		},
		loginURL: func(dir, session string) (board.Login, error) {
			call("login url %s %q", dir, session)
			return board.Login{Local: testURL}, nil
		},
		show: func(msg string) { f.shown = append(f.shown, msg) },
		open: func(url string) error {
			call("open %s", url)
			return nil
		},
		sleep: func(d time.Duration) { call("sleep %v", d) },
		out:   &f.out,
	}
	return f
}

func TestBoardOffShowsHowToTurnItOn(t *testing.T) {
	for _, flags := range [][2]bool{{false, false}, {true, true}} {
		f := newFakeBoard(t, reconcile.Options{})
		if err := f.run(flags[0], flags[1], ""); err != nil {
			t.Fatal(err)
		}
		want := "The task board is off. Add board = true to " + filepath.Join(testConfigDir, "config.toml") + "."
		if !slices.Equal(f.shown, []string{want}) {
			t.Errorf("showed %q, want %q", f.shown, want)
		}
		if len(f.calls) > 0 || f.out.Len() > 0 {
			t.Errorf("with the board off: %q, printed %q; want nothing done", f.calls, f.out.String())
		}
	}
}

func TestBoardPortTakenMessage(t *testing.T) {
	f := newFakeBoard(t, reconcile.Options{Board: true, BoardPort: 7400})
	f.ensure = func(string, int, string, string) error {
		return fmt.Errorf("%w: something answers at 127.0.0.1:7400", board.ErrPortTaken)
	}
	err := f.run(false, false, "")
	if !errors.Is(err, errShown) {
		t.Errorf("run = %v, want errShown, so it exits 1", err)
	}
	want := "Port 7400 is in use by another program. Set board_port in " + filepath.Join(testConfigDir, "config.toml") + "."
	if !slices.Equal(f.shown, []string{want}) {
		t.Errorf("showed %q, want %q", f.shown, want)
	}
	if len(f.calls) > 0 {
		t.Errorf("after the port was taken: %q", f.calls)
	}

	// Other errors are shown as they are.
	f = newFakeBoard(t, reconcile.Options{Board: true})
	stuck := fmt.Errorf("%w (pid 12, version 0.6.0); see /state/board/board.log", board.ErrNotStopped)
	f.ensure = func(string, int, string, string) error { return stuck }
	if err := f.run(false, false, ""); !errors.Is(err, errShown) {
		t.Errorf("run = %v, want errShown", err)
	}
	if !slices.Equal(f.shown, []string{stuck.Error()}) {
		t.Errorf("showed %q, want %q", f.shown, stuck.Error())
	}
}

func TestBoardPrintsAURLWithACode(t *testing.T) {
	f := newFakeBoard(t, reconcile.Options{Board: true})
	if err := f.run(true, false, ""); err != nil {
		t.Fatal(err)
	}
	want := []string{"ensure /state 7391 /plugin/bin/herdr-tasks " + version, `login url /state ""`}
	if !slices.Equal(f.calls, want) {
		t.Errorf("calls = %q, want %q", f.calls, want)
	}
	if got := f.out.String(); got != testURL+"\n" {
		t.Errorf("printed %q, want the URL", got)
	}
	if len(f.shown) > 0 {
		t.Errorf("showed %q", f.shown)
	}

	// Without --print it opens the board...
	f = newFakeBoard(t, reconcile.Options{Board: true, BoardPort: 7400})
	if err := f.run(false, false, ""); err != nil {
		t.Fatal(err)
	}
	want = []string{"ensure /state 7400 /plugin/bin/herdr-tasks " + version, `login url /state ""`, "open " + testURL}
	if !slices.Equal(f.calls, want) || f.out.Len() > 0 || len(f.shown) > 0 {
		t.Errorf("calls = %q, printed %q, showed %q; want %q", f.calls, f.out.String(), f.shown, want)
	}

	// ...and shows the URL when no browser opens.
	f = newFakeBoard(t, reconcile.Options{Board: true})
	f.open = func(string) error { return errors.New("no display to open a browser on") }
	if err := f.run(false, false, ""); err != nil {
		t.Fatal(err)
	}
	if want := "Open this in your browser: " + testURL + " (the code works for a minute)"; !slices.Equal(f.shown, []string{want}) {
		t.Errorf("showed %q, want %q", f.shown, want)
	}
}

// With the board on the tailnet, a machine that can't open a browser, as
// one without a display, shows the tailnet address, and --print prints it
// on a second line.
func TestBoardShowsTailnetWhenNoBrowser(t *testing.T) {
	const tailnetURL = "https://devbox.tail0000.ts.net:7391/work/?code=0123abcd"
	login := func(string, string) (board.Login, error) {
		return board.Login{Local: testURL, Tailnet: tailnetURL}, nil
	}
	f := newFakeBoard(t, reconcile.Options{Board: true, BoardTailnet: true})
	f.loginURL = login
	var opened []string
	f.open = func(url string) error {
		opened = append(opened, url)
		return errors.New("no display to open a browser on")
	}
	if err := f.run(false, false, "work"); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(opened, []string{testURL}) {
		t.Errorf("opened %q, want the local address", opened)
	}
	if want := "Open this in your browser: " + tailnetURL + " (the code works for a minute)"; !slices.Equal(f.shown, []string{want}) {
		t.Errorf("showed %q, want %q", f.shown, want)
	}

	f = newFakeBoard(t, reconcile.Options{Board: true, BoardTailnet: true})
	f.loginURL = login
	if err := f.run(true, false, "work"); err != nil {
		t.Fatal(err)
	}
	if got, want := f.out.String(), testURL+"\n"+tailnetURL+"\n"; got != want {
		t.Errorf("printed %q, want %q", got, want)
	}
}

// With a tab, as from the overview, the board opens at that task's view, on
// the tailnet too.
func TestBoardOpensATask(t *testing.T) {
	const tailnetURL = "https://devbox.tail0000.ts.net:7391/work/?code=0123abcd"
	f := newFakeBoard(t, reconcile.Options{Board: true, BoardTailnet: true})
	f.loginURL = func(string, string) (board.Login, error) {
		return board.Login{Local: testURL, Tailnet: tailnetURL}, nil
	}
	var opened []string
	f.open = func(url string) error {
		opened = append(opened, url)
		return errors.New("no display to open a browser on")
	}
	f.tab = "w1:t3"
	if err := f.run(false, false, "work"); err != nil {
		t.Fatal(err)
	}
	if want := []string{testURL + "#tab=w1%3At3"}; !slices.Equal(opened, want) {
		t.Errorf("opened %q, want %q", opened, want)
	}
	if want := "Open this in your browser: " + tailnetURL + "#tab=w1%3At3 (the code works for a minute)"; !slices.Equal(f.shown, []string{want}) {
		t.Errorf("showed %q, want %q", f.shown, want)
	}
}

// A server that just started hasn't read Tailscale yet, so the board
// command asks again, every half second, until it says where the board is
// on the tailnet or why it isn't there, for up to 12 s.
func TestBoardWaitsForTheTailnetAddress(t *testing.T) {
	const tailnetURL = "https://devbox.tail0000.ts.net:7391/work/?code=0123abcd"
	f := newFakeBoard(t, reconcile.Options{Board: true, BoardTailnet: true})
	logins := 0
	f.loginURL = func(string, string) (board.Login, error) {
		if logins++; logins < 4 {
			return board.Login{Local: testURL}, nil
		}
		return board.Login{Local: testURL, Tailnet: tailnetURL}, nil
	}
	f.open = func(string) error { return errors.New("no display to open a browser on") }
	if err := f.run(false, false, "work"); err != nil {
		t.Fatal(err)
	}
	if want := slices.Repeat([]string{"sleep 500ms"}, 3); logins != 4 || !slices.Equal(f.calls[1:], want) {
		t.Errorf("asked %d times, calls %q; want 4 and %q", logins, f.calls, want)
	}
	if want := "Open this in your browser: " + tailnetURL + " (the code works for a minute)"; !slices.Equal(f.shown, []string{want}) {
		t.Errorf("showed %q, want %q", f.shown, want)
	}

	// A problem ends the wait too.
	f = newFakeBoard(t, reconcile.Options{Board: true, BoardTailnet: true})
	logins = 0
	f.loginURL = func(string, string) (board.Login, error) {
		if logins++; logins < 2 {
			return board.Login{Local: testURL}, nil
		}
		return board.Login{Local: testURL, Problem: "Port 7391 is already shared with your tailnet for something else."}, nil
	}
	if err := f.run(true, false, "work"); err != nil {
		t.Fatal(err)
	}
	if logins != 2 || !slices.Equal(f.shown, []string{"Port 7391 is already shared with your tailnet for something else."}) {
		t.Errorf("asked %d times, showed %q", logins, f.shown)
	}

	// After 12 s it goes on with the local address.
	f = newFakeBoard(t, reconcile.Options{Board: true, BoardTailnet: true})
	f.open = func(string) error { return errors.New("no display to open a browser on") }
	if err := f.run(false, false, "work"); err != nil {
		t.Fatal(err)
	}
	sleeps := 0
	for _, c := range f.calls {
		if strings.HasPrefix(c, "sleep ") {
			sleeps++
		}
	}
	if sleeps != 24 {
		t.Errorf("waited %d times, want 24 (12 s)", sleeps)
	}
	if want := "Open this in your browser: " + testURL + " (the code works for a minute)"; !slices.Equal(f.shown, []string{want}) {
		t.Errorf("showed %q, want %q", f.shown, want)
	}

	// With board_tailnet off, it doesn't wait.
	f = newFakeBoard(t, reconcile.Options{Board: true})
	if err := f.run(true, false, "work"); err != nil {
		t.Fatal(err)
	}
	if slices.ContainsFunc(f.calls, func(c string) bool { return strings.HasPrefix(c, "sleep ") }) {
		t.Errorf("with board_tailnet off: %q", f.calls)
	}
}

// When the board can't be shared with the tailnet, the board command says
// why, on the message line, and --print still prints only addresses.
func TestBoardShowsTailnetProblem(t *testing.T) {
	const problem = "Port 7391 is already shared with your tailnet for something else."
	login := func(string, string) (board.Login, error) {
		return board.Login{Local: testURL, Problem: problem}, nil
	}
	for _, c := range []struct {
		print    bool
		openErr  error
		out      string
		shown    string
		describe string
	}{
		{true, nil, testURL + "\n", problem, "--print"},
		{false, nil, "", problem, "opening the board"},
		{false, errors.New("no display to open a browser on"), "",
			"Open this in your browser: " + testURL + " (the code works for a minute) " + problem, "no browser"},
	} {
		f := newFakeBoard(t, reconcile.Options{Board: true, BoardTailnet: true})
		f.loginURL = login
		f.open = func(string) error { return c.openErr }
		if err := f.run(c.print, false, "work"); err != nil {
			t.Fatal(err)
		}
		if f.out.String() != c.out || !slices.Equal(f.shown, []string{c.shown}) {
			t.Errorf("%s: printed %q, showed %q; want %q and %q", c.describe, f.out.String(), f.shown, c.out, c.shown)
		}
	}
}

// --new-token replaces the token and restarts the server in one step, so no
// server starts with the old token in between.
func TestBoardNewTokenRotates(t *testing.T) {
	f := newFakeBoard(t, reconcile.Options{Board: true, BoardPort: 7400})
	if err := f.run(false, true, ""); err != nil {
		t.Fatal(err)
	}
	want := []string{"rotate /state 7400 /plugin/bin/herdr-tasks " + version, `login url /state ""`, "open " + testURL}
	if !slices.Equal(f.calls, want) {
		t.Errorf("calls = %q, want %q", f.calls, want)
	}

	// A server that doesn't stop only knows the old token, so the token stays.
	f = newFakeBoard(t, reconcile.Options{Board: true})
	stuck := fmt.Errorf("%w (pid 12, version 0.6.0); see /state/board/board.log", board.ErrNotStopped)
	f.rotate = func(string, int, string, string) error { return stuck }
	if err := f.run(false, true, ""); !errors.Is(err, errShown) {
		t.Errorf("run = %v, want errShown", err)
	}
	if len(f.calls) > 0 || !slices.Equal(f.shown, []string{stuck.Error()}) {
		t.Errorf("calls = %q, showed %q; want only %q shown", f.calls, f.shown, stuck.Error())
	}
}

func TestPathCopy(t *testing.T) {
	dir := t.TempDir()
	state := filepath.Join(dir, "state")
	plugin, copied := filepath.Join(dir, "plugin", "herdr-tasks"), filepath.Join(dir, "bin", "herdr-tasks")
	for _, p := range []string{plugin, copied} {
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte("herdr-tasks"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	// As executable() gives it: the temp dir may be behind a symlink.
	resolved := func(p string) string {
		r, err := filepath.EvalSymlinks(p)
		if err != nil {
			t.Fatal(err)
		}
		return r
	}
	if pathCopy(resolved(copied), state) {
		t.Error("with nothing installed: a PATH copy")
	}
	if err := os.MkdirAll(state, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(state, "installed-binary"), []byte(copied+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if !pathCopy(resolved(copied), state) {
		t.Error("the installed copy isn't the PATH copy")
	}
	if pathCopy(resolved(plugin), state) {
		t.Error("the plugin's binary is the PATH copy")
	}
}

func TestBrowserCommand(t *testing.T) {
	env := func(vars map[string]string) func(string) string {
		return func(k string) string { return vars[k] }
	}
	cmd, wait, err := browserCommand("darwin", env(nil), testURL)
	if err != nil || !wait || !slices.Equal(cmd.Args, []string{"/usr/bin/open", testURL}) {
		t.Errorf("darwin: %v, wait %v, %v", cmd, wait, err)
	}

	if _, _, err := browserCommand("linux", env(nil), testURL); err == nil {
		t.Error("linux without a display: no error")
	}
	cmd, wait, err = browserCommand("linux", env(map[string]string{"WAYLAND_DISPLAY": "wayland-0"}), testURL)
	if err != nil || wait || !slices.Equal(cmd.Args, []string{"xdg-open", testURL}) {
		t.Fatalf("linux: %v, wait %v, %v", cmd, wait, err)
	}
	// xdg-open can run as long as the browser it starts, so it must not
	// share the session or hold the pipes of the herdr action.
	if cmd.SysProcAttr == nil || !cmd.SysProcAttr.Setsid || cmd.Stdin != nil || cmd.Stdout != nil || cmd.Stderr != nil {
		t.Errorf("xdg-open: %+v, stdio %v %v %v; want its own session and no stdio", cmd.SysProcAttr, cmd.Stdin, cmd.Stdout, cmd.Stderr)
	}
}

func TestLinearKeyReadsStdinWhenPiped(t *testing.T) {
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	if _, err := w.WriteString("  lin_api_abc123  \nsomething else\n"); err != nil {
		t.Fatal(err)
	}
	w.Close()
	var saved []string
	var out bytes.Buffer
	save := func(k string) error { saved = append(saved, k); return nil }
	del := func() error { t.Error("deleted the key"); return nil }
	dir := t.TempDir()
	if err := linearKey(nil, r, &out, save, del, dir); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(saved, []string{"lin_api_abc123"}) {
		t.Errorf("saved %q, want the first line", saved)
	}
	if got := out.String(); got != "herdr-tasks: saved the Linear key\n" {
		t.Errorf("printed %q", got)
	}
	if readMarker(filepath.Join(dir, keyChanged)) == "" {
		t.Error("the board server isn't told the key changed")
	}

	// A key that isn't saved changes nothing for the board server.
	dir = t.TempDir()
	r, w, err = os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	w.WriteString("lin_api_abc123\n")
	w.Close()
	failed := func(string) error { return linear.ErrKeychain }
	if err := linearKey(nil, r, &out, failed, del, dir); !errors.Is(err, linear.ErrKeychain) {
		t.Errorf("linearKey with a failing keychain = %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, keyChanged)); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("the board server is told of a key that wasn't saved: %v", err)
	}
}

func TestLinearKeyDelete(t *testing.T) {
	deleted := false
	var out bytes.Buffer
	save := func(string) error { t.Error("saved a key"); return nil }
	dir := t.TempDir()
	marker := filepath.Join(dir, keyChanged)
	if err := os.WriteFile(marker, []byte("1\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := linearKey([]string{"--delete"}, nil, &out, save, func() error { deleted = true; return nil }, dir); err != nil {
		t.Fatal(err)
	}
	if !deleted || out.String() != "herdr-tasks: deleted the Linear key\n" {
		t.Errorf("deleted %v, printed %q", deleted, out.String())
	}
	if m := readMarker(marker); m == "" || m == "1\n" {
		t.Errorf("the board server isn't told the key changed: %q", m)
	}
}

// keyTime is when the linearClient tests start.
var keyTime = time.Date(2026, 10, 31, 9, 0, 0, 0, time.UTC)

// fakeKeychain is a Linear key store for linearClient, with a clock.
type fakeKeychain struct {
	key   string
	err   error
	reads int
	now   time.Time
}

func (k *fakeKeychain) load() (string, error) {
	k.reads++
	return k.key, k.err
}

func (k *fakeKeychain) clock() time.Time { return k.now }

// The board server asks for a Linear client every minute. Reading the key
// runs /usr/bin/security on macOS, which may show an unlock dialog, so it
// reads the key once, and again only once herdr-tasks linear-key changed it.
func TestLinearClientReadsTheKeyOnce(t *testing.T) {
	dir := t.TempDir()
	k := &fakeKeychain{key: "lin_api_abc123", now: keyTime}
	client := linearClient(k.load, filepath.Join(dir, keyChanged), k.clock)
	for range 5 {
		c, ok := client()
		if !ok || c == nil || c.Key != "lin_api_abc123" {
			t.Fatalf("client = %v, %v; want one with the key", c, ok)
		}
		k.now = k.now.Add(time.Hour)
	}
	if k.reads != 1 {
		t.Errorf("read the key %d times, want once", k.reads)
	}

	k.key = "lin_api_def456"
	if err := noteKeyChanged(dir); err != nil {
		t.Fatal(err)
	}
	if c, ok := client(); !ok || c.Key != "lin_api_def456" || k.reads != 2 {
		t.Errorf("after linear-key: %v, %v, %d reads; want the new key, read again", c, ok, k.reads)
	}
	k.key, k.err = "", linear.ErrNoKey
	if err := noteKeyChanged(dir); err != nil {
		t.Fatal(err)
	}
	if c, ok := client(); ok || c != nil {
		t.Errorf("after linear-key --delete: %v, %v; want no client", c, ok)
	}
}

// A keychain that fails, as a locked one does, isn't asked again for a
// while, and less often each time it fails again.
func TestLinearClientWaitsAfterTheKeychainFails(t *testing.T) {
	dir := t.TempDir()
	k := &fakeKeychain{err: linear.ErrKeychain, now: keyTime}
	client := linearClient(k.load, filepath.Join(dir, keyChanged), k.clock)
	// ask asks every minute for d, and returns how many reads that made.
	ask := func(d time.Duration) int {
		t.Helper()
		before := k.reads
		for end := k.now.Add(d); k.now.Before(end); k.now = k.now.Add(time.Minute) {
			if c, ok := client(); ok || c != nil {
				t.Fatalf("client with a failing keychain: %v, %v", c, ok)
			}
		}
		return k.reads - before
	}
	if n := ask(5 * time.Minute); n != 1 {
		t.Errorf("first 5 minutes: %d reads, want 1", n)
	}
	if n := ask(10 * time.Minute); n != 1 {
		t.Errorf("the next 10 minutes: %d reads, want 1", n)
	}
	if n := ask(20 * time.Minute); n != 1 {
		t.Errorf("the next 20 minutes: %d reads, want 1", n)
	}
	if n := ask(3 * time.Hour); n != 4 {
		t.Errorf("3 hours later on: %d reads, want 4 (40 minutes, then hourly)", n)
	}

	// Saving the key with linear-key reads it at once.
	k.key, k.err = "lin_api_abc123", nil
	if err := noteKeyChanged(dir); err != nil {
		t.Fatal(err)
	}
	if c, ok := client(); !ok || c.Key != "lin_api_abc123" {
		t.Errorf("after linear-key: %v, %v; want the key", c, ok)
	}
}

// With no key saved, it looks again every 15 minutes, as well as when
// linear-key saves one.
func TestLinearClientWithNoKey(t *testing.T) {
	dir := t.TempDir()
	k := &fakeKeychain{err: linear.ErrNoKey, now: keyTime}
	client := linearClient(k.load, filepath.Join(dir, keyChanged), k.clock)
	for range 30 {
		if _, ok := client(); ok {
			t.Fatal("a client with no key")
		}
		k.now = k.now.Add(time.Minute)
	}
	if k.reads != 2 {
		t.Errorf("read %d times in 30 minutes, want 2", k.reads)
	}
	k.key, k.err = "lin_api_abc123", nil
	if err := noteKeyChanged(dir); err != nil {
		t.Fatal(err)
	}
	if c, ok := client(); !ok || c.Key != "lin_api_abc123" {
		t.Errorf("after linear-key: %v, %v; want the key", c, ok)
	}
}

func TestBoardSessionFlag(t *testing.T) {
	print, newToken, session, err := parseBoardArgs([]string{"--print", "--session", "side"})
	if err != nil || !print || newToken || session != "side" {
		t.Fatalf("parseBoardArgs = %v, %v, %q, %v", print, newToken, session, err)
	}
	f := newFakeBoard(t, reconcile.Options{Board: true})
	if err := f.run(false, false, session); err != nil {
		t.Fatal(err)
	}
	want := []string{"ensure /state 7391 /plugin/bin/herdr-tasks " + version, `login url /state "side"`, "open " + testURL}
	if !slices.Equal(f.calls, want) {
		t.Errorf("calls = %q, want %q", f.calls, want)
	}

	for _, args := range [][]string{{"--bogus"}, {"--session"}, {"--session", "--print"}} {
		_, _, _, err := parseBoardArgs(args)
		if err == nil {
			t.Fatalf("%q: no error", args)
		}
		for _, flag := range []string{"--print", "--new-token", "--session"} {
			if !strings.Contains(err.Error(), flag) {
				t.Errorf("%q: error %q doesn't name %s", args, err, flag)
			}
		}
	}
	if err := (&app{}).openBoard([]string{"--bogus"}); err == nil {
		t.Error("openBoard --bogus: no error")
	}
}

func TestCurrentSession(t *testing.T) {
	env := func(m map[string]string) func(string) string { return func(k string) string { return m[k] } }
	for name, c := range map[string]struct {
		env  map[string]string
		want string
	}{
		"a named session":    {map[string]string{"HERDR_SESSION": "work"}, "work"},
		"the default socket": {map[string]string{"HERDR_SOCKET_PATH": "/tmp/h/.config/herdr/herdr.sock"}, "default"},
		"a named socket":     {map[string]string{"HERDR_SOCKET_PATH": "/tmp/h/.config/herdr/sessions/side/herdr.sock"}, "side"},
		"the socket wins":    {map[string]string{"HERDR_SOCKET_PATH": "/tmp/h/.config/herdr/herdr.sock", "HERDR_SESSION": "work"}, "default"},
		"outside herdr":      {nil, ""},
		"a nested name":      {map[string]string{"HERDR_SESSION": "a/b"}, "default"}, // as herdr's own SessionKey reads that socket
	} {
		if got := currentSession(env(c.env)); got != c.want {
			t.Errorf("%s: currentSession = %q, want %q", name, got, c.want)
		}
	}
}

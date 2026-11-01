package main

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/charmbracelet/x/term"

	"github.com/alekkowalczyk/herdr-tasks/internal/board"
	"github.com/alekkowalczyk/herdr-tasks/internal/claudelog"
	"github.com/alekkowalczyk/herdr-tasks/internal/herdr"
	"github.com/alekkowalczyk/herdr-tasks/internal/linear"
	"github.com/alekkowalczyk/herdr-tasks/internal/reconcile"
	"github.com/alekkowalczyk/herdr-tasks/internal/setup"
	"github.com/alekkowalczyk/herdr-tasks/internal/slack"
	"github.com/alekkowalczyk/herdr-tasks/internal/store"
	"github.com/alekkowalczyk/herdr-tasks/internal/tailnet"
)

// errShown is returned once you've been told what went wrong, so the command
// exits 1 without saying it again.
var errShown = errors.New("already shown")

// boardCmd is `herdr-tasks board`. The server and the browser are funcs, so
// tests can fake them.
type boardCmd struct {
	opts reconcile.Options
	dir  string // the state dir
	exe  string // the binary the server runs

	ensure   func(dir string, port int, exe, version string) error
	rotate   func(dir string, port int, exe, version string) error
	loginURL func(dir, session string) (board.Login, error)

	show func(msg string)       // tells you something
	open func(url string) error // opens url in the browser
	out  io.Writer              // where --print prints the URL
}

// openBoard runs `herdr-tasks board [--print] [--new-token] [--session NAME]`.
func (a *app) openBoard(args []string) error {
	print, newToken, session, err := parseBoardArgs(args)
	if err != nil {
		return err
	}
	if session == "" {
		session = currentSession(os.Getenv)
	}
	exe, err := executable()
	if err != nil {
		return err
	}
	ensure := board.Ensure
	rotate := func(dir string, port int, exe, _ string) error { return board.Rotate(dir, port, exe) }
	if pathCopy(exe, store.Dir()) {
		ensure, rotate = board.EnsureNotOlder, board.RotateNotOlder
	}
	b := boardCmd{
		opts: a.opts, dir: store.Dir(), exe: exe,
		ensure: ensure, rotate: rotate, loginURL: board.LoginURL,
		show: a.showMessage, open: openBrowser, out: os.Stdout,
	}
	return b.run(print, newToken, session)
}

const boardUsage = "use --print, --new-token or --session NAME"

// parseBoardArgs reads the options of `herdr-tasks board`.
func parseBoardArgs(args []string) (print, newToken bool, session string, err error) {
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--print":
			print = true
		case "--new-token":
			newToken = true
		case "--session":
			if i+1 >= len(args) || args[i+1] == "" || strings.HasPrefix(args[i+1], "-") {
				return false, false, "", fmt.Errorf("--session needs a session name; %s", boardUsage)
			}
			i++
			session = args[i]
		default:
			return false, false, "", fmt.Errorf("unknown board option %q; %s", args[i], boardUsage)
		}
	}
	return print, newToken, session, nil
}

// currentSession is the herdr session this command runs in, from the
// variables herdr sets in its panes, or "" when it runs outside herdr.
func currentSession(getenv func(string) string) string {
	if getenv("HERDR_SOCKET_PATH") == "" && getenv("HERDR_SESSION") == "" {
		return ""
	}
	return herdr.SessionKey(herdr.SocketPathFor(getenv))
}

// run starts the board server if it isn't running, and signs the browser in
// to it with a one-time code, at the board of session, or at the list of
// sessions when it's "". With print it only prints the address, and the
// address on the tailnet while the board is shared there. When no browser
// opens, as on a machine without a display, it shows the address on the
// tailnet, or else the local one. It also shows why the board isn't on the
// tailnet, when that goes wrong. With newToken it first replaces the token,
// which signs every browser out.
func (b boardCmd) run(print, newToken bool, session string) error {
	if !b.opts.Board {
		b.show("The task board is off. Add board = true to " + configFile() + ".")
		return nil
	}
	port := b.opts.Port()
	var err error
	if newToken {
		// The server only knows the token it started with, so it's
		// stopped with that one and started again with the new one.
		err = b.rotate(b.dir, port, b.exe, version)
	} else {
		err = b.ensure(b.dir, port, b.exe, version)
	}
	if err != nil {
		return b.fail(boardTrouble(err, port))
	}
	login, err := b.loginURL(b.dir, session)
	if err != nil {
		return b.fail(err.Error())
	}
	// Said once, as each message opens the overview when herdr runs this.
	var say []string
	if print {
		fmt.Fprintln(b.out, login.Local)
		if login.Tailnet != "" {
			fmt.Fprintln(b.out, login.Tailnet)
		}
	} else if err := b.open(login.Local); err != nil {
		url := login.Local
		if login.Tailnet != "" {
			url = login.Tailnet
		}
		say = append(say, "Open this in your browser: "+url+" (the code works for a minute)")
	}
	if login.Problem != "" {
		say = append(say, login.Problem)
	}
	if len(say) > 0 {
		b.show(strings.Join(say, " "))
	}
	return nil
}

func (b boardCmd) fail(msg string) error {
	b.show(msg)
	return errShown
}

// boardTrouble says why the board server didn't start.
func boardTrouble(err error, port int) string {
	if errors.Is(err, board.ErrPortTaken) {
		return fmt.Sprintf("Port %d is in use by another program. Set board_port in %s.", port, configFile())
	}
	return err.Error()
}

// pathCopy reports whether exe is the copy of herdr-tasks that setup put on
// PATH. Only setup refreshes it, so after `make build` it's older than the
// plugin's own binary, which the startup hook and herdr's action run.
func pathCopy(exe, stateDir string) bool {
	p := setup.InstalledBinary(stateDir)
	if p == "" {
		return false
	}
	if r, err := filepath.EvalSymlinks(p); err == nil {
		p = r
	}
	return p == exe
}

// configFile is the plugin's settings file.
func configFile() string { return filepath.Join(reconcile.ConfigDir(), "config.toml") }

// executable is this binary, for the board server to run.
func executable() (string, error) {
	exe, err := os.Executable()
	if err != nil {
		return "", err
	}
	return filepath.EvalSymlinks(exe)
}

// pluginID is the plugin's ID in herdr.
func pluginID() string {
	if id := os.Getenv("HERDR_PLUGIN_ID"); id != "" {
		return id
	}
	return store.PluginID
}

// showMessage tells you msg on stderr, which herdr keeps in `herdr plugin
// log list`. Nothing a herdr action prints is shown, so when herdr runs this
// (stderr isn't a terminal) it also opens the overview with msg in its
// message line.
func (a *app) showMessage(msg string) {
	if os.Getenv("HERDR_ENV") == "1" && !term.IsTerminal(os.Stderr.Fd()) {
		_ = a.client.OpenPluginPaneEnv(pluginID(), "overview", map[string]string{"HERDR_TASKS_MESSAGE": msg})
	}
	fmt.Fprintln(os.Stderr, "herdr-tasks: "+msg)
}

// openBrowser opens url in the default browser.
func openBrowser(url string) error {
	cmd, wait, err := browserCommand(runtime.GOOS, os.Getenv, url)
	if err != nil {
		return err
	}
	if wait {
		return cmd.Run()
	}
	return cmd.Start()
}

// browserCommand is the command that opens url on goos, as herdr opens
// links. Its stdio is /dev/null, so it never holds a herdr action's pipes
// open. xdg-open may run as long as the browser it starts, so it gets a
// session of its own and isn't waited for.
func browserCommand(goos string, getenv func(string) string, url string) (cmd *exec.Cmd, wait bool, err error) {
	switch goos {
	case "darwin":
		return exec.Command("/usr/bin/open", url), true, nil
	case "linux":
		if getenv("DISPLAY") == "" && getenv("WAYLAND_DISPLAY") == "" {
			return nil, false, errors.New("no display to open a browser on")
		}
		cmd := exec.Command("xdg-open", url)
		cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
		return cmd, false, nil
	}
	return nil, false, fmt.Errorf("can't open a browser on %s", goos)
}

// serve runs the board server, for every running herdr session, and the
// Slack watcher, which logs to slack.log. With board_tailnet on, it shares
// the boards with the tailnet too. The startup hook and `herdr-tasks board`
// start it, detached. It stops on SIGTERM, when the board and the Slack
// watcher are turned off, when the board is moved to another port, or when
// no herdr session has run for a while. It logs to stderr, which is
// board.log.
func (a *app) serve() error {
	exe, err := executable()
	if err != nil {
		return err
	}
	dir := store.Dir()
	lock, ln, err := board.Listener(dir, a.opts.Port())
	if err != nil {
		return err
	}
	token, err := board.EnsureToken(dir)
	if err != nil {
		return err
	}
	// The listener Ensure passed may be on another port than the settings
	// say now; the server notices that and stops.
	port := ln.Addr().(*net.TCPAddr).Port

	// The terminal or herdr session it came from may go away.
	signal.Ignore(syscall.SIGHUP)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	sigs := make(chan os.Signal, 1)
	signal.Notify(sigs, syscall.SIGTERM, os.Interrupt)
	go func() {
		log.Printf("shutting down: %v", <-sigs)
		cancel()
	}()

	log.Printf("serving on 127.0.0.1:%d: version %s, pid %d", port, version, os.Getpid())
	w := &slack.Watcher{Dir: dir, Exe: exe, Options: reconcile.LoadOptions, Log: slack.OpenLog(dir)}
	s := board.NewServer(board.Config{
		StateDir: dir,
		Port:     port,
		Version:  version,
		Build:    board.BuildID(exe),
		Exe:      exe,
		Sessions: board.HerdrSessions(),
		Linear:   linearClient(linear.LoadKey, filepath.Join(dir, keyChanged), time.Now),
		Options:  reconcile.LoadOptions,
		Claude:   claudelog.NewCache(claudelog.ConfigDir()),
		Slack:    w.Status,
		SlackFetch: func(ctx context.Context) (string, error) {
			got, err := w.Fetch(ctx)
			if err != nil {
				return "", err
			}
			return got.Summary(time.Now()), nil
		},
		SlackTake: func(ctx context.Context, id string) (string, error) {
			d, err := w.Take(ctx, id)
			if err != nil {
				return "", err
			}
			return d.TookLine(), nil
		},
		Tailscale: tailnet.CLI{},
	}, board.NewAuth(token), lock)
	w.Changed = s.Refresh
	watching := make(chan struct{})
	go func() {
		defer close(watching)
		w.Run(ctx)
	}()
	err = s.Run(ctx, ln)
	// A check under way ends with ctx, which kills its claude. The wait is
	// short, as a new server waits for this one to exit.
	cancel()
	select {
	case <-watching:
	case <-time.After(watcherGrace):
	}
	return err
}

// watcherGrace is how long the server waits for the Slack watcher to stop
// as it shuts down.
const watcherGrace = 500 * time.Millisecond

// keyChanged is the file in the state dir that `herdr-tasks linear-key`
// writes after it saves or deletes the Linear key, so the board server reads
// the key again.
const keyChanged = "linear-key.changed"

// How long the board server waits to read the Linear key again after a read
// that gave none.
const (
	keyRetry    = 5 * time.Minute  // after the keychain failed; it doubles while it fails
	keyRetryMax = time.Hour        // the most keyRetry doubles to
	noKeyRetry  = 15 * time.Minute // when no key is saved
)

// linearClient gives the board a Linear client with the saved key, read
// with load. On macOS a read runs /usr/bin/security, which can show an
// unlock dialog while the keychain is locked, so the key is read once and
// kept. It's read again when the content of the marker file changes, as
// herdr-tasks linear-key changes it, and, after a read that gave no key,
// once a while has passed: keyRetry, doubling up to keyRetryMax, while the
// keychain fails, and noKeyRetry while no key is saved. A failure is logged
// once.
func linearClient(load func() (string, error), marker string, now func() time.Time) func() (*linear.Client, bool) {
	var mu sync.Mutex
	var (
		read  bool          // whether the key was read yet
		key   string        // the key, once read
		mark  string        // the marker's content when it was read
		next  time.Time     // when to read again, while there's no key
		retry time.Duration // the wait after the keychain's last failure
		said  string
	)
	return func() (*linear.Client, bool) {
		mu.Lock()
		defer mu.Unlock()
		m := readMarker(marker)
		if read && m == mark && (key != "" || now().Before(next)) {
			if key == "" {
				return nil, false
			}
			return &linear.Client{Key: key}, true
		}
		if m != mark {
			retry = 0
		}
		read, mark = true, m
		var err error
		key, err = load()
		switch {
		case err == nil:
			retry, said = 0, ""
			return &linear.Client{Key: key}, true
		case errors.Is(err, linear.ErrNoKey):
			retry = 0
			next = now().Add(noKeyRetry)
		default:
			retry = min(max(2*retry, keyRetry), keyRetryMax)
			next = now().Add(retry)
			if msg := err.Error(); msg != said {
				log.Printf("reading the Linear key: %s", msg)
				said = msg
			}
		}
		key = ""
		return nil, false
	}
}

// readMarker is the content of the marker file, or "" when there's none.
func readMarker(path string) string {
	b, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	return string(b)
}

// noteKeyChanged writes something new to the keyChanged file in dir, so a
// running board server reads the Linear key again.
func noteKeyChanged(dir string) error {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(dir, keyChanged), []byte(strconv.FormatInt(time.Now().UnixNano(), 10)+"\n"), 0o600)
}

// startBoard starts the board server, which also runs the Slack watcher,
// when herdr starts, unless it runs. A problem goes to stderr and doesn't
// stop the hook.
func (a *app) startBoard() {
	exe, err := executable()
	if err == nil {
		err = board.Ensure(store.Dir(), a.opts.Port(), exe, version)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "herdr-tasks: "+boardTrouble(err, a.opts.Port()))
	}
}

// linearKey runs `herdr-tasks linear-key [--delete]`: it saves the Linear
// API key the board asks for issue status with, read from in, or removes it.
// Then it tells a running board server, through the keyChanged file in the
// state dir.
func linearKey(args []string, in *os.File, out io.Writer, save func(string) error, remove func() error, stateDir string) error {
	if slices.Contains(args, "--delete") {
		if err := remove(); err != nil {
			return err
		}
		fmt.Fprintln(out, "herdr-tasks: deleted the Linear key")
	} else {
		key, err := readKey(in)
		if err != nil {
			return err
		}
		if err := save(key); err != nil {
			return err
		}
		fmt.Fprintln(out, "herdr-tasks: saved the Linear key")
	}
	if err := noteKeyChanged(stateDir); err != nil {
		fmt.Fprintf(out, "herdr-tasks: note: the task board sees the change once its server restarts (%v)\n", err)
	}
	return nil
}

// readKey reads the key from in: its first line when it's piped, or typed
// at a prompt without echo when it's a terminal.
func readKey(in *os.File) (string, error) {
	if !term.IsTerminal(in.Fd()) {
		line, err := bufio.NewReader(io.LimitReader(in, 4096)).ReadString('\n')
		if err != nil && !errors.Is(err, io.EOF) {
			return "", err
		}
		return strings.TrimSpace(line), nil
	}
	fmt.Fprint(os.Stderr, "Linear API key (input hidden): ")
	b, err := term.ReadPassword(in.Fd())
	fmt.Fprintln(os.Stderr)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(b)), nil
}

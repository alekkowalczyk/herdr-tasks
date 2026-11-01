package board

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// The server's files, in serverDir.
const (
	lockFile = "board.lock" // held by the server for its whole life
	metaFile = "board.json" // Meta, while a server runs
	logFile  = "board.log"  // the server's stdout and stderr
)

// serverDir is the folder in the state dir that holds the server's files.
// They don't go in the state dir itself, where each herdr session's tasks
// are <session>.json and <session>.lock, as a session may be called board.
// No session's file is named plain board.
func serverDir(stateDir string) string { return filepath.Join(stateDir, "board") }

// serverFile is the path of one of the server's files.
func serverFile(stateDir, name string) string { return filepath.Join(serverDir(stateDir), name) }

const (
	// fdsEnv tells `serve` that Ensure passed it the lock and the listener.
	fdsEnv = "HERDR_TASKS_BOARD_FDS"
	// How long Ensure and Stop wait for a server to answer or to exit.
	patience = 3 * time.Second
	// How often they look again meanwhile.
	poll = 50 * time.Millisecond
	// board.log is emptied when a server starts and it's bigger than this.
	maxLog = 1 << 20
)

var (
	ErrPortTaken = errors.New("port taken by another program")
	// ErrNotStopped comes wrapped with the server's pid and version, and
	// where its log is.
	ErrNotStopped = errors.New("the running board server didn't stop")

	errLocked     = errors.New("board.lock is taken")
	errNotRunning = errors.New("the board server isn't running")
)

// sessionVars tie a process to one herdr session or one plugin event. The
// server serves every session, so it doesn't get them.
var sessionVars = []string{
	"HERDR_SOCKET_PATH", "HERDR_SESSION",
	"HERDR_PANE_ID", "HERDR_TAB_ID", "HERDR_WORKSPACE_ID",
	"HERDR_PLUGIN_EVENT", "HERDR_PLUGIN_EVENT_JSON", "HERDR_PLUGIN_CONTEXT_JSON", "HERDR_PLUGIN_ACTION_ID",
	"HERDR_PLUGIN_CLICKED_URL", "HERDR_PLUGIN_LINK_HANDLER_ID",
}

// Meta is what the running server records in board.json and answers on
// /api/health.
type Meta struct {
	PID     int    `json:"pid"`
	Port    int    `json:"port"`
	Version string `json:"version"`
	Build   string `json:"build"` // BuildID of its binary
	Exe     string `json:"exe"`   // its binary, symlinks resolved; "" from a server that doesn't say
}

// ReadMeta reads board.json. The server writes it before it serves and
// removes it as it starts to shut down. The next process to take the lock
// removes one that a killed server left.
func ReadMeta(stateDir string) (Meta, error) {
	var m Meta
	b, err := os.ReadFile(serverFile(stateDir, metaFile))
	if err != nil {
		return m, err
	}
	err = json.Unmarshal(b, &m)
	return m, err
}

func writeMeta(stateDir string, m Meta) error {
	b, err := json.Marshal(m)
	if err != nil {
		return err
	}
	return writePrivate(serverFile(stateDir, metaFile), string(b)+"\n", false)
}

// BuildID tells builds of herdr-tasks apart, so a rebuild with an unchanged
// version still replaces the running server. It hashes the file rather than
// using its size and time, so the copy that setup puts on PATH counts as the
// same build as the plugin's own binary and the two don't keep replacing
// each other's server. It's "" when exe can't be read.
func BuildID(exe string) string {
	f, err := os.Open(exe)
	if err != nil {
		return ""
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return ""
	}
	return hex.EncodeToString(h.Sum(nil)[:8])
}

// Ensure makes sure a server of this version and build is running on port.
// It starts `exe serve` when none is, and replaces one that differs. It
// returns once the server answers, or with an error: ErrPortTaken, or
// ErrNotStopped when a server holds the lock but doesn't answer or stop.
func Ensure(stateDir string, port int, exe, version string) error {
	return ensure(stateDir, port, exe, version, false)
}

// EnsureNotOlder is Ensure for a copy of herdr-tasks that may be older than
// the plugin's own binary, such as the one setup puts on PATH, which `make
// build` doesn't refresh. It keeps a server of any build on port, unless the
// server's version is older than version, so the plugin's server and the
// copy's don't keep taking each other's place.
func EnsureNotOlder(stateDir string, port int, exe, version string) error {
	return ensure(stateDir, port, exe, version, true)
}

func ensure(stateDir string, port int, exe, version string, anyBuild bool) error {
	if _, err := EnsureToken(stateDir); err != nil {
		return err
	}
	exe, err := filepath.Abs(exe)
	if err != nil {
		return err
	}
	build := BuildID(exe)
	for range 3 {
		lock, m, err := claim(stateDir)
		if err != nil {
			return err
		}
		if lock != nil {
			// Read again with the lock held, as the server will: Rotate
			// only replaces it while it holds the lock.
			token, err := EnsureToken(stateDir)
			if err != nil {
				lock.Close()
				return err
			}
			return start(stateDir, port, exe, token, lock)
		}
		same := m.Version == version && m.Build == build
		if m.Port == port && (same || anyBuild && !older(m.Version, version)) {
			return nil
		}
		if err := Stop(stateDir); err != nil {
			return err
		}
	}
	return errors.New("another board server keeps starting; try again")
}

// older reports whether version a comes before b. Both are dotted numbers,
// as 0.7.0; it's false when either isn't.
func older(a, b string) bool {
	parse := func(v string) []int {
		var n []int
		for _, part := range strings.Split(v, ".") {
			i, err := strconv.Atoi(part)
			if err != nil || i < 0 {
				return nil
			}
			n = append(n, i)
		}
		return n
	}
	x, y := parse(a), parse(b)
	if x == nil || y == nil {
		return false
	}
	for i := range max(len(x), len(y)) {
		var p, q int
		if i < len(x) {
			p = x[i]
		}
		if i < len(y) {
			q = y[i]
		}
		if p != q {
			return p < q
		}
	}
	return false
}

// claim takes board.lock. While another process holds it, claim waits for
// that server to answer instead, for up to 3 s, as one may be starting.
func claim(stateDir string) (*os.File, Meta, error) {
	deadline := time.Now().Add(patience)
	for {
		lock, err := tryLock(stateDir)
		if err == nil {
			return lock, Meta{}, nil
		}
		if !errors.Is(err, errLocked) {
			return nil, Meta{}, err
		}
		// Read each time: the server may be one that Rotate started with a
		// new token.
		token, err := readToken(filepath.Join(stateDir, tokenFile))
		if err != nil {
			return nil, Meta{}, err
		}
		m, err := ask(stateDir, token, within(deadline))
		if err == nil {
			return nil, m, nil
		}
		if time.Now().After(deadline) {
			return nil, Meta{}, notStopped(stateDir, fmt.Errorf("it isn't answering: %w", err))
		}
		time.Sleep(poll)
	}
}

// start starts the server with lock, and waits until it answers.
func start(stateDir string, port int, exe, token string, lock *os.File) error {
	cmd, err := spawn(stateDir, port, exe, lock)
	if err != nil {
		return err
	}
	// Waiting also reaps the server, should this process outlive it.
	exited := make(chan error, 1)
	go func() { exited <- cmd.Wait() }()
	logPath := serverFile(stateDir, logFile)
	deadline := time.Now().Add(patience)
	for {
		_, err := health(port, token, within(deadline))
		if err == nil {
			return nil
		}
		select {
		case werr := <-exited:
			return fmt.Errorf("the board server stopped as it started (%v)%s", werr, logTail(logPath))
		case <-time.After(poll):
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("the board server didn't answer: %v%s", err, logTail(logPath))
		}
	}
}

// spawn starts `exe serve` in its own session, handing it lock and a
// listener on port as fds 3 and 4. This process's copies are closed when it
// returns, so if the server dies, the lock and the port are free again.
func spawn(stateDir string, port int, exe string, lock *os.File) (*exec.Cmd, error) {
	defer lock.Close()
	ln, err := listen(port)
	if err != nil {
		return nil, err
	}
	lnf, err := ln.(*net.TCPListener).File()
	ln.Close()
	if err != nil {
		return nil, err
	}
	defer lnf.Close()
	logPath := serverFile(stateDir, logFile)
	if fi, err := os.Stat(logPath); err == nil && fi.Size() > maxLog {
		_ = os.Truncate(logPath, 0)
	}
	logf, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return nil, err
	}
	defer logf.Close()

	cmd := exec.Command(exe, "serve")
	cmd.Dir = stateDir
	cmd.Env = serverEnv(os.Environ())
	// Never the caller's stdout and stderr: herdr waits for a hook's pipes
	// to close before it counts the hook as done. stdin is /dev/null.
	cmd.Stdout, cmd.Stderr = logf, logf
	cmd.ExtraFiles = []*os.File{lock, lnf}
	// A session of its own, so it outlives the hook that started it and
	// signals sent to that hook's process group.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	return cmd, nil
}

// serverEnv is env without sessionVars, and with fdsEnv set.
func serverEnv(env []string) []string {
	out := make([]string, 0, len(env)+1)
	for _, kv := range env {
		k, _, _ := strings.Cut(kv, "=")
		if k != fdsEnv && !slices.Contains(sessionVars, k) {
			out = append(out, kv)
		}
	}
	return append(out, fdsEnv+"=1")
}

// Listener gives `herdr-tasks serve` its lock and listener. When Ensure
// started it, they're the ones Ensure passed as fds 3 and 4. Run by hand, it
// takes the lock itself, failing if a server is running, and listens on
// 127.0.0.1:port. Keep lock open for as long as the server runs: closing it
// lets a second server start.
func Listener(stateDir string, port int) (lock *os.File, ln net.Listener, err error) {
	if os.Getenv(fdsEnv) == "1" {
		os.Unsetenv(fdsEnv) // not for anything the server starts
		lock = os.NewFile(3, serverFile(stateDir, lockFile))
		if _, err := lock.Stat(); err != nil {
			return nil, nil, fmt.Errorf("%s is set, but fd 3 isn't open: %w", fdsEnv, err)
		}
		// Not for anything the server starts either, such as the
		// /usr/bin/security that reads the Linear key: one still running
		// after the server exits would keep the lock.
		syscall.CloseOnExec(3)
		lnf := os.NewFile(4, "board listener")
		ln, err = net.FileListener(lnf)
		lnf.Close()
		if err != nil {
			lock.Close()
			return nil, nil, fmt.Errorf("%s is set, but fd 4 isn't a listener: %w", fdsEnv, err)
		}
		return lock, ln, nil
	}
	lock, err = tryLock(stateDir)
	if errors.Is(err, errLocked) {
		return nil, nil, errors.New("the board server is already running")
	}
	if err != nil {
		return nil, nil, err
	}
	if ln, err = listen(port); err != nil {
		lock.Close()
		return nil, nil, err
	}
	return lock, ln, nil
}

// listen listens on 127.0.0.1:port, unless something answers there already.
// On macOS, a program listening on every address doesn't stop us binding
// 127.0.0.1, and we'd quietly take its local traffic; connecting first
// catches that.
func listen(port int) (net.Listener, error) {
	addr := net.JoinHostPort("127.0.0.1", strconv.Itoa(port))
	if c, err := net.DialTimeout("tcp", addr, 300*time.Millisecond); err == nil {
		c.Close()
		return nil, fmt.Errorf("%w: something answers at %s", ErrPortTaken, addr)
	}
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrPortTaken, err)
	}
	return ln, nil
}

// tryLock takes board.lock without waiting, or returns errLocked. The lock
// is free, so no server runs, and any board.json is a killed server's: it's
// removed at once, so that nobody sends the token to the port it records
// while this process holds the lock.
func tryLock(stateDir string) (*os.File, error) {
	if err := os.MkdirAll(serverDir(stateDir), 0o700); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(serverFile(stateDir, lockFile), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		f.Close()
		if errors.Is(err, syscall.EWOULDBLOCK) {
			return nil, errLocked
		}
		return nil, err
	}
	os.Remove(serverFile(stateDir, metaFile))
	return f, nil
}

// Stop asks the running server to shut down and waits up to 3 s for it to
// exit. It's nil when no server is running, and ErrNotStopped when one
// still is.
func Stop(stateDir string) error {
	lock, err := stopAndHold(stateDir)
	if err != nil {
		return err
	}
	lock.Close()
	return nil
}

// Rotate replaces the token, which signs every browser out, and starts the
// server again with the new one. It writes the token while it holds
// board.lock, and then hands the lock to the server, so no server starts
// with the old token in between.
func Rotate(stateDir string, port int, exe string) error {
	return rotate(stateDir, port, exe, "", false)
}

// RotateNotOlder is Rotate for a copy of herdr-tasks that may be older than
// the plugin's own binary, as EnsureNotOlder is Ensure. When the server
// running on port isn't older than version, it starts that server's own
// binary again, rather than exe, so a new token doesn't cost the plugin's
// server its place.
func RotateNotOlder(stateDir string, port int, exe, version string) error {
	return rotate(stateDir, port, exe, version, true)
}

func rotate(stateDir string, port int, exe, version string, anyBuild bool) error {
	exe, err := filepath.Abs(exe)
	if err != nil {
		return err
	}
	if anyBuild {
		// Asked before it stops, as board.json goes with it.
		if m, err := Health(stateDir); err == nil && m.Port == port && !older(m.Version, version) && isProgram(m.Exe) {
			exe = m.Exe
		}
	}
	lock, err := stopAndHold(stateDir)
	if err != nil {
		return err
	}
	token, err := NewToken(stateDir)
	if err != nil {
		lock.Close()
		return err
	}
	return start(stateDir, port, exe, token, lock)
}

// isProgram reports whether path is an absolute path to a regular file that
// can be run.
func isProgram(path string) bool {
	if !filepath.IsAbs(path) {
		return false
	}
	fi, err := os.Stat(path)
	return err == nil && fi.Mode().IsRegular() && fi.Mode().Perm()&0o111 != 0
}

// stopAndHold stops the running server as Stop does, but keeps board.lock
// rather than let go of it, so no other server starts.
func stopAndHold(stateDir string) (*os.File, error) {
	deadline := time.Now().Add(patience)
	asked := 0 // the pid of the server that agreed to shut down
	var cause error
	for {
		lock, err := tryLock(stateDir)
		if err == nil {
			return lock, nil
		}
		if !errors.Is(err, errLocked) {
			return nil, err
		}
		// A server that's starting may not have written board.json yet, and
		// another Ensure may start one as this one exits: each is asked.
		if m, err := ReadMeta(stateDir); err != nil {
			cause = err
		} else if m.PID != asked {
			if cause = shutdown(stateDir, m, within(deadline)); cause == nil {
				asked = m.PID
			}
		}
		if time.Now().After(deadline) {
			return nil, notStopped(stateDir, cause)
		}
		time.Sleep(poll)
	}
}

// shutdown asks the server board.json records to shut down.
func shutdown(stateDir string, m Meta, timeout time.Duration) error {
	token, err := readToken(filepath.Join(stateDir, tokenFile))
	if err != nil {
		return err
	}
	resp, err := call("POST", m.Port, "/api/shutdown", token, timeout)
	if err != nil {
		return err
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusAccepted {
		return fmt.Errorf("it answered %s", resp.Status)
	}
	return nil
}

// Health asks the running server how it is, at the port board.json records.
func Health(stateDir string) (Meta, error) {
	token, err := adminToken(stateDir)
	if err != nil {
		return Meta{}, err
	}
	return ask(stateDir, token, time.Second)
}

// Login is the address that signs a browser in: Local on this machine.
// Tailnet is the address on the tailnet, or "" when the board isn't there.
type Login struct {
	Local, Tailnet string
}

// LoginURL asks the running server for a one-time login code, and returns
// the addresses that sign a browser in with it, at the board of session, or
// at the list of sessions when session is "": on this machine, and on the
// tailnet while the board is shared there. The code works once, for a
// minute.
func LoginURL(stateDir, session string) (Login, error) {
	token, err := adminToken(stateDir)
	if err != nil {
		return Login{}, err
	}
	m, err := ReadMeta(stateDir)
	if err != nil {
		return Login{}, err
	}
	resp, err := call("POST", m.Port, "/api/login-code", token, patience)
	if err != nil {
		return Login{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return Login{}, fmt.Errorf("port %d answered %s", m.Port, resp.Status)
	}
	var r struct {
		Code    string `json:"code"`
		Tailnet string `json:"tailnet"` // the board's address on the tailnet, while it's shared
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<16)).Decode(&r); err != nil {
		return Login{}, fmt.Errorf("port %d: %w", m.Port, err)
	}
	if r.Code == "" {
		return Login{}, fmt.Errorf("port %d gave no login code", m.Port)
	}
	at := "/"
	if session != "" {
		at = "/" + url.PathEscape(session) + "/"
	}
	code := url.QueryEscape(r.Code)
	login := Login{Local: fmt.Sprintf("http://127.0.0.1:%d%s?code=%s", m.Port, at, code)}
	if r.Tailnet != "" {
		login.Tailnet = r.Tailnet + at + "?code=" + code
	}
	return login, nil
}

// adminToken is the token to ask the running server with, once it's sure
// one holds the lock. That way the token only goes to the port board.json
// records while it's the running server's (see ask).
func adminToken(stateDir string) (string, error) {
	lock, err := tryLock(stateDir)
	if err == nil {
		lock.Close()
		return "", errNotRunning
	}
	if !errors.Is(err, errLocked) {
		return "", err
	}
	return readToken(filepath.Join(stateDir, tokenFile))
}

// ask asks the server at the port board.json records how it is. Only ask
// while the lock is taken, as Health and claim do: board.json is then the
// running server's, or missing while one starts or shuts down, and the
// token doesn't go to a port that a server left to some other program.
func ask(stateDir, token string, timeout time.Duration) (Meta, error) {
	m, err := ReadMeta(stateDir)
	if err != nil {
		return Meta{}, err
	}
	return health(m.Port, token, timeout)
}

// healthReply is /api/health's answer.
type healthReply struct {
	App string `json:"app"` // "herdr-tasks"
	Meta
}

// health asks the server on port how it is.
func health(port int, token string, timeout time.Duration) (Meta, error) {
	resp, err := call("GET", port, "/api/health", token, timeout)
	if err != nil {
		return Meta{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return Meta{}, fmt.Errorf("port %d answered %s", port, resp.Status)
	}
	var h healthReply
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<16)).Decode(&h); err != nil {
		return Meta{}, fmt.Errorf("port %d: %w", port, err)
	}
	if h.App != "herdr-tasks" {
		return Meta{}, fmt.Errorf("port %d isn't the board", port)
	}
	return h.Meta, nil
}

// direct goes straight to 127.0.0.1, never through a proxy.
var direct = &http.Transport{DisableKeepAlives: true}

// call sends a request with the admin token to the server on port.
func call(method string, port int, path, token string, timeout time.Duration) (*http.Response, error) {
	req, err := http.NewRequest(method, fmt.Sprintf("http://127.0.0.1:%d%s", port, path), nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	return (&http.Client{Transport: direct, Timeout: timeout}).Do(req)
}

// within is how long one request may take: a second at most, and not much
// past deadline.
func within(deadline time.Time) time.Duration {
	return min(max(time.Until(deadline), 100*time.Millisecond), time.Second)
}

func notStopped(stateDir string, cause error) error {
	who, why := "", ""
	if m, err := ReadMeta(stateDir); err == nil {
		who = fmt.Sprintf(" (pid %d, version %s)", m.PID, m.Version)
	}
	if cause != nil {
		why = ": " + cause.Error()
	}
	return fmt.Errorf("%w%s%s; see %s", ErrNotStopped, who, why, serverFile(stateDir, logFile))
}

// logTail is the end of board.log, to show with an error.
func logTail(path string) string {
	f, err := os.Open(path)
	if err != nil {
		return ""
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		return ""
	}
	off := max(fi.Size()-4096, 0)
	b := make([]byte, fi.Size()-off)
	n, _ := f.ReadAt(b, off)
	lines := strings.Split(strings.TrimRight(string(b[:n]), "\n"), "\n")
	if off > 0 {
		lines = lines[1:] // the first is likely cut
	}
	lines = lines[max(len(lines)-5, 0):]
	text := strings.TrimSpace(strings.Join(lines, "\n"))
	if text == "" {
		return ""
	}
	return "; the end of " + path + ":\n" + text
}

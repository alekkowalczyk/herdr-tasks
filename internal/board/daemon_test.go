package board

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/rand/v2"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/alekkowalczyk/herdr-tasks/internal/store"
	"github.com/alekkowalczyk/herdr-tasks/internal/task"
)

const testVersion = "test"

// TestMain doubles as `herdr-tasks serve` when Ensure starts this test
// binary as the server.
func TestMain(m *testing.M) {
	if os.Getenv("GO_BOARD_HELPER") == "1" {
		os.Exit(serveHelper())
	}
	os.Exit(m.Run())
}

// serveHelper is the server the lifecycle tests start: Listener, NewServer
// and Run, as `herdr-tasks serve` does, with no herdr session. It runs in the
// state dir. GO_BOARD_HELPER_BUILD and GO_BOARD_HELPER_VERSION make it
// report another build or version, and so do the files <binary>.build and
// <binary>.version, for a copy of it. GO_BOARD_HELPER_FAIL makes it fail to
// start, and GO_BOARD_HELPER_CHILD makes it start a child that outlives it,
// whose pid goes in child.pid.
func serveHelper() int {
	if msg := os.Getenv("GO_BOARD_HELPER_FAIL"); msg != "" {
		fmt.Fprintln(os.Stderr, msg)
		return 1
	}
	dir, err := os.Getwd()
	if err != nil {
		fmt.Fprintln(os.Stderr, "helper:", err)
		return 1
	}
	lock, ln, err := Listener(dir, 0)
	if err != nil {
		fmt.Fprintln(os.Stderr, "helper:", err)
		return 1
	}
	if os.Getenv("GO_BOARD_HELPER_CHILD") == "1" {
		// As the server runs /usr/bin/security for the Linear key, which
		// may wait on a locked keychain after the server exits.
		child := exec.Command("/bin/sleep", "30")
		if err := child.Start(); err != nil {
			fmt.Fprintln(os.Stderr, "helper:", err)
			return 1
		}
		if err := os.WriteFile(filepath.Join(dir, "child.pid"), []byte(strconv.Itoa(child.Process.Pid)), 0o600); err != nil {
			fmt.Fprintln(os.Stderr, "helper:", err)
			return 1
		}
	}
	token, err := EnsureToken(dir)
	if err != nil {
		fmt.Fprintln(os.Stderr, "helper:", err)
		return 1
	}
	port := ln.Addr().(*net.TCPAddr).Port
	exe, _ := os.Executable()
	build := BuildID(exe)
	if b := os.Getenv("GO_BOARD_HELPER_BUILD"); b != "" {
		build = b
	}
	if b, err := os.ReadFile(exe + ".build"); err == nil {
		build = strings.TrimSpace(string(b))
	}
	version := testVersion
	if v := os.Getenv("GO_BOARD_HELPER_VERSION"); v != "" {
		version = v
	}
	if v, err := os.ReadFile(exe + ".version"); err == nil {
		version = strings.TrimSpace(string(v))
	}
	if resolved, err := filepath.EvalSymlinks(exe); err == nil {
		exe = resolved
	}
	fmt.Printf("helper serving: pid %d, port %d\n", os.Getpid(), port)
	// A helper that a failed test left running doesn't outlive it by much.
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	s := NewServer(Config{StateDir: dir, Port: port, Version: version, Build: build, Exe: exe, Sessions: running()}, NewAuth(token), lock)
	if err := s.Run(ctx, ln); err != nil {
		fmt.Fprintln(os.Stderr, "helper:", err)
		return 1
	}
	return 0
}

// helperExe makes servers that Ensure starts run serveHelper, and returns
// the binary to start.
func helperExe(t *testing.T) string {
	t.Helper()
	t.Setenv("GO_BOARD_HELPER", "1")
	// Under -race, a process waits a second as it exits, which would hide
	// a slow Stop.
	t.Setenv("GORACE", strings.TrimSpace(os.Getenv("GORACE")+" atexit_sleep_ms=0"))
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	return exe
}

// stateDir is an empty state dir. Whatever server is still running in it at
// the end of the test is stopped.
func stateDir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	t.Cleanup(func() {
		if err := Stop(dir); err != nil {
			t.Errorf("stopping the test server: %v", err)
			if m, err := ReadMeta(dir); err == nil && m.PID > 0 {
				_ = syscall.Kill(m.PID, syscall.SIGKILL)
			}
		}
	})
	return dir
}

// freePort is a port nothing listens on right now.
func freePort(t *testing.T) int {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	return ln.Addr().(*net.TCPAddr).Port
}

// request sends a request to the server on port, with secret as its Bearer
// token.
func request(t *testing.T, method string, port int, path, secret string) *http.Response {
	t.Helper()
	req, err := http.NewRequest(method, fmt.Sprintf("http://127.0.0.1:%d%s", port, path), nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+secret)
	resp, err := (&http.Client{Transport: &http.Transport{DisableKeepAlives: true}}).Do(req)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { resp.Body.Close() })
	return resp
}

func status(t *testing.T, port int, path, secret string) int {
	t.Helper()
	resp := request(t, "GET", port, path, secret)
	io.Copy(io.Discard, resp.Body)
	return resp.StatusCode
}

// listening reports whether anything answers on port.
func listening(port int) bool {
	c, err := net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", port), time.Second)
	if err != nil {
		return false
	}
	c.Close()
	return true
}

func starts(t *testing.T, dir string) int {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(dir, "board", "board.log"))
	if err != nil {
		t.Fatal(err)
	}
	return strings.Count(string(b), "helper serving")
}

func TestEnsureStartsOneServer(t *testing.T) {
	dir, port, exe := stateDir(t), freePort(t), helperExe(t)
	errs := make([]error, 5)
	var wg sync.WaitGroup
	for i := range errs {
		wg.Add(1)
		go func() {
			defer wg.Done()
			errs[i] = Ensure(dir, port, exe, testVersion)
		}()
	}
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			t.Fatalf("Ensure %d: %v", i, err)
		}
	}
	m, err := Health(dir)
	if err != nil {
		t.Fatal(err)
	}
	if m.Port != port || m.Version != testVersion || m.Build != BuildID(exe) || m.PID <= 0 {
		t.Errorf("health = %+v, want port %d, version %s, build %s", m, port, testVersion, BuildID(exe))
	}
	if n := starts(t, dir); n != 1 {
		t.Errorf("%d servers started, want 1", n)
	}
	if fi, err := os.Stat(filepath.Join(dir, "board", "board.json")); err != nil || fi.Mode().Perm() != 0o600 {
		t.Errorf("board.json: %v, %v; want mode 600", fi, err)
	}
	if fi, err := os.Stat(filepath.Join(dir, "board")); err != nil || fi.Mode().Perm() != 0o700 {
		t.Errorf("the board folder: %v, %v; want mode 700", fi, err)
	}

	// Once it runs, Ensure leaves it alone.
	if err := Ensure(dir, port, exe, testVersion); err != nil {
		t.Fatal(err)
	}
	if again, err := Health(dir); err != nil || again.PID != m.PID {
		t.Errorf("after another Ensure: %+v, %v; want pid %d", again, err, m.PID)
	}
	if n := starts(t, dir); n != 1 {
		t.Errorf("%d servers started, want 1", n)
	}
}

func TestEnsureReportsPortTaken(t *testing.T) {
	dir := stateDir(t)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	port := ln.Addr().(*net.TCPAddr).Port
	if err := Ensure(dir, port, helperExe(t), testVersion); !errors.Is(err, ErrPortTaken) {
		t.Fatalf("Ensure on a taken port = %v, want ErrPortTaken", err)
	}
	if _, err := Health(dir); err == nil {
		t.Error("a server is running after ErrPortTaken")
	}
}

func TestEnsureReplacesAnOtherBuild(t *testing.T) {
	dir, port, exe := stateDir(t), freePort(t), helperExe(t)
	t.Setenv("GO_BOARD_HELPER_BUILD", "another-build")
	if err := Ensure(dir, port, exe, testVersion); err != nil {
		t.Fatal(err)
	}
	old, err := Health(dir)
	if err != nil || old.Build != "another-build" {
		t.Fatalf("first server: %+v, %v", old, err)
	}

	t.Setenv("GO_BOARD_HELPER_BUILD", "")
	start := time.Now()
	if err := Ensure(dir, port, exe, testVersion); err != nil {
		t.Fatal(err)
	}
	if d := time.Since(start); d > 4*time.Second {
		t.Errorf("replacing the server took %v", d)
	}
	m, err := Health(dir)
	if err != nil {
		t.Fatal(err)
	}
	if m.Build != BuildID(exe) || m.PID == old.PID {
		t.Errorf("after Ensure: %+v; want build %s from a new process", m, BuildID(exe))
	}
	if n := starts(t, dir); n != 2 {
		t.Errorf("%d servers started, want 2", n)
	}
}

// The copy of herdr-tasks on PATH may be older than the plugin's binary. It
// leaves the plugin's server running whatever its build, unless that
// server's version is older or it's on another port.
func TestEnsureNotOlderKeepsTheRunningServer(t *testing.T) {
	dir, port, exe := stateDir(t), freePort(t), helperExe(t)
	t.Setenv("GO_BOARD_HELPER_BUILD", "the-plugins-build")
	t.Setenv("GO_BOARD_HELPER_VERSION", "0.7.0")
	if err := Ensure(dir, port, exe, "0.7.0"); err != nil {
		t.Fatal(err)
	}
	first, err := Health(dir)
	if err != nil || first.Build != "the-plugins-build" {
		t.Fatalf("first server: %+v, %v", first, err)
	}

	t.Setenv("GO_BOARD_HELPER_BUILD", "")
	for _, v := range []string{"0.7.0", "0.6.2", "dev"} {
		if err := EnsureNotOlder(dir, port, exe, v); err != nil {
			t.Fatalf("version %s: %v", v, err)
		}
		if m, err := Health(dir); err != nil || m.PID != first.PID {
			t.Errorf("after EnsureNotOlder with version %s: %+v, %v; want pid %d still", v, m, err, first.PID)
		}
	}
	if n := starts(t, dir); n != 1 {
		t.Errorf("%d servers started, want 1", n)
	}

	t.Setenv("GO_BOARD_HELPER_VERSION", "0.10.0")
	if err := EnsureNotOlder(dir, port, exe, "0.10.0"); err != nil {
		t.Fatal(err)
	}
	m, err := Health(dir)
	if err != nil || m.PID == first.PID || m.Version != "0.10.0" {
		t.Errorf("after a newer EnsureNotOlder: %+v, %v; want version 0.10.0 from a new process", m, err)
	}
	other := freePort(t)
	if err := EnsureNotOlder(dir, other, exe, "0.10.0"); err != nil {
		t.Fatal(err)
	}
	if m, err := Health(dir); err != nil || m.Port != other {
		t.Errorf("after a port change: %+v, %v; want port %d", m, err, other)
	}
}

// A new token from the copy of herdr-tasks on PATH, which may be older than
// the plugin's binary, starts the plugin's server again, with the new token,
// unless that server is older than the copy.
func TestRotateNotOlderKeepsTheRunningServersBinary(t *testing.T) {
	dir, port, copied := stateDir(t), freePort(t), helperExe(t)
	t.Setenv("GO_BOARD_HELPER_VERSION", "0.8.0") // what the copy reports
	plugin := filepath.Join(t.TempDir(), "herdr-tasks")
	if err := os.Link(copied, plugin); err != nil {
		copyFile(t, copied, plugin)
	}
	for ext, text := range map[string]string{".version": "0.9.0", ".build": "the-plugins-build"} {
		if err := os.WriteFile(plugin+ext, []byte(text+"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := Ensure(dir, port, plugin, "0.9.0"); err != nil {
		t.Fatal(err)
	}
	first, err := Health(dir)
	if err != nil || first.Version != "0.9.0" || first.Build != "the-plugins-build" || first.Exe == "" {
		t.Fatalf("the plugin's server: %+v, %v", first, err)
	}
	old, err := EnsureToken(dir)
	if err != nil {
		t.Fatal(err)
	}

	if err := RotateNotOlder(dir, port, copied, "0.8.0"); err != nil {
		t.Fatal(err)
	}
	m, err := Health(dir) // with the token stored now
	if err != nil || m.PID == first.PID || m.Version != "0.9.0" || m.Build != "the-plugins-build" {
		t.Errorf("after a new token from the older copy: %+v, %v; want the plugin's server started again", m, err)
	}
	if fresh, err := EnsureToken(dir); err != nil || fresh == old {
		t.Errorf("the token didn't change: %v", err)
	}
	if got := status(t, port, "/api/health", old); got != http.StatusUnauthorized {
		t.Errorf("old token: %d, want 401", got)
	}

	// A server older than the copy gives way to the copy's.
	t.Setenv("GO_BOARD_HELPER_VERSION", "0.10.0")
	if err := RotateNotOlder(dir, port, copied, "0.10.0"); err != nil {
		t.Fatal(err)
	}
	if m, err := Health(dir); err != nil || m.Version != "0.10.0" || m.Build != BuildID(copied) {
		t.Errorf("after a new token from a newer copy: %+v, %v; want the copy's server", m, err)
	}
}

// copyFile copies the file at from to a new executable file at to.
func copyFile(t *testing.T, from, to string) {
	t.Helper()
	in, err := os.Open(from)
	if err != nil {
		t.Fatal(err)
	}
	defer in.Close()
	out, err := os.OpenFile(to, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o755)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		t.Fatal(err)
	}
	if err := out.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestOlderVersion(t *testing.T) {
	for _, c := range []struct {
		a, b string
		want bool
	}{
		{"0.6.0", "0.7.0", true},
		{"0.9.0", "0.10.0", true},
		{"0.7", "0.7.1", true},
		{"1.0.0", "0.10.0", false},
		{"0.7.0", "0.7.0", false},
		{"0.7.0", "0.7", false},
		{"test", "0.7.0", false},
		{"0.7.0", "", false},
	} {
		if got := older(c.a, c.b); got != c.want {
			t.Errorf("older(%q, %q) = %v, want %v", c.a, c.b, got, c.want)
		}
	}
}

func TestEnsureShowsTheLogWhenTheServerFails(t *testing.T) {
	dir, port, exe := stateDir(t), freePort(t), helperExe(t)
	t.Setenv("GO_BOARD_HELPER_FAIL", "helper: no luck today")
	start := time.Now()
	err := Ensure(dir, port, exe, testVersion)
	if err == nil || !strings.Contains(err.Error(), "helper: no luck today") {
		t.Fatalf("Ensure = %v, want an error with the end of board.log", err)
	}
	if d := time.Since(start); d > 2*time.Second {
		t.Errorf("noticing the server stopped took %v", d)
	}
	if listening(port) {
		t.Error("the port is still taken")
	}
}

func TestStopEndsServerWithOpenStream(t *testing.T) {
	dir, port, exe := stateDir(t), freePort(t), helperExe(t)
	if err := Ensure(dir, port, exe, testVersion); err != nil {
		t.Fatal(err)
	}
	resp := request(t, "GET", port, "/api/events", signIn(t, dir))
	if resp.StatusCode != http.StatusOK || resp.Header.Get("Content-Type") != "text/event-stream" {
		t.Fatalf("events: %s %q", resp.Status, resp.Header.Get("Content-Type"))
	}
	ended := make(chan struct{})
	go func() {
		io.Copy(io.Discard, resp.Body)
		close(ended)
	}()

	start := time.Now()
	if err := Stop(dir); err != nil {
		t.Fatal(err)
	}
	// The stream ends at once, so shutting down doesn't wait out its 2 s for
	// open requests.
	if d := time.Since(start); d > 1500*time.Millisecond {
		t.Errorf("Stop took %v", d)
	}
	select {
	case <-ended:
	case <-time.After(2 * time.Second):
		t.Error("the stream stayed open")
	}
	if listening(port) {
		t.Error("the port is still taken")
	}
	if _, err := Health(dir); err == nil {
		t.Error("Health answered after Stop")
	}
	if err := Stop(dir); err != nil {
		t.Errorf("Stop with nothing running = %v", err)
	}
}

// The lock Ensure passes the server is its own: a child it starts mustn't
// hold it once the server exits, or no server could start until the child
// ends.
func TestServerChildDoesntHoldTheLock(t *testing.T) {
	dir, port, exe := stateDir(t), freePort(t), helperExe(t)
	t.Setenv("GO_BOARD_HELPER_CHILD", "1")
	if err := Ensure(dir, port, exe, testVersion); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(filepath.Join(dir, "child.pid"))
	if err != nil {
		t.Fatal(err)
	}
	child, err := strconv.Atoi(string(b))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = syscall.Kill(child, syscall.SIGKILL) })

	if err := Stop(dir); err != nil {
		t.Fatalf("Stop with the server's child still running: %v", err)
	}
	if err := syscall.Kill(child, 0); err != nil {
		t.Fatalf("the child ended with the server (%v), so this tested nothing", err)
	}
	t.Setenv("GO_BOARD_HELPER_CHILD", "") // the next server starts none
	if err := Ensure(dir, port, exe, testVersion); err != nil {
		t.Errorf("Ensure with the old server's child still running: %v", err)
	}
}

func TestPortChangeStopsTheOldServer(t *testing.T) {
	dir, exe := stateDir(t), helperExe(t)
	a := freePort(t)
	if err := Ensure(dir, a, exe, testVersion); err != nil {
		t.Fatal(err)
	}
	b := freePort(t)
	if err := Ensure(dir, b, exe, testVersion); err != nil {
		t.Fatal(err)
	}
	if listening(a) {
		t.Errorf("port %d still answers", a)
	}
	m, err := Health(dir)
	if err != nil || m.Port != b {
		t.Errorf("health = %+v, %v; want port %d", m, err, b)
	}
}

func TestNewTokenRotation(t *testing.T) {
	dir, port, exe := stateDir(t), freePort(t), helperExe(t)
	if err := Ensure(dir, port, exe, testVersion); err != nil {
		t.Fatal(err)
	}
	old, err := EnsureToken(dir)
	if err != nil {
		t.Fatal(err)
	}
	key := signIn(t, dir)
	if got := status(t, port, "/api/health", key); got != http.StatusOK {
		t.Fatalf("browser key before rotation: %d", got)
	}

	// What `board --new-token` does.
	if err := Rotate(dir, port, exe); err != nil {
		t.Fatal(err)
	}
	fresh, err := EnsureToken(dir)
	if err != nil {
		t.Fatal(err)
	}
	if fresh == old {
		t.Fatal("Rotate kept the token")
	}

	if got := status(t, port, "/api/health", key); got != http.StatusUnauthorized {
		t.Errorf("old browser key: %d, want 401", got)
	}
	if got := status(t, port, "/api/health", old); got != http.StatusUnauthorized {
		t.Errorf("old token: %d, want 401", got)
	}
	if got := status(t, port, "/api/health", signIn(t, dir)); got != http.StatusOK {
		t.Errorf("new browser key: %d, want 200", got)
	}
}

// A page left open keeps sending its key to the port, and while no server
// runs, another program may have the port. So a browser key works only with
// the run of the server that gave it out, though the token stays the same.
func TestRestartSignsBrowsersOut(t *testing.T) {
	dir, port, exe := stateDir(t), freePort(t), helperExe(t)
	if err := Ensure(dir, port, exe, testVersion); err != nil {
		t.Fatal(err)
	}
	tok, err := EnsureToken(dir)
	if err != nil {
		t.Fatal(err)
	}
	key := signIn(t, dir)
	if got := status(t, port, "/api/board", key); got != http.StatusOK {
		t.Fatalf("browser key: %d", got)
	}

	// As when the server stops for want of herdr sessions, and the startup
	// hook starts it again.
	if err := Stop(dir); err != nil {
		t.Fatal(err)
	}
	if err := Ensure(dir, port, exe, testVersion); err != nil {
		t.Fatal(err)
	}
	if again, err := EnsureToken(dir); err != nil || again != tok {
		t.Fatalf("token after a restart: %v; want it kept", err)
	}
	if got := status(t, port, "/api/board", key); got != http.StatusUnauthorized {
		t.Errorf("the last run's browser key: %d, want 401", got)
	}
	if got := status(t, port, "/api/board", signIn(t, dir)); got != http.StatusOK {
		t.Errorf("a new sign-in: %d, want 200", got)
	}
}

// Rotating while a startup hook ensures the server leaves one server, with
// the new token: none starts with the old token in between.
func TestRotateWhileAnotherEnsures(t *testing.T) {
	dir, port, exe := stateDir(t), freePort(t), helperExe(t)
	if err := Ensure(dir, port, exe, testVersion); err != nil {
		t.Fatal(err)
	}
	for i := range 12 {
		old, err := EnsureToken(dir)
		if err != nil {
			t.Fatal(err)
		}
		key := signIn(t, dir)
		var wg sync.WaitGroup
		wg.Add(1)
		go func() {
			defer wg.Done()
			time.Sleep(time.Duration(rand.IntN(150)) * time.Millisecond)
			// It may lose its server to the rotation; only the end counts.
			if err := Ensure(dir, port, exe, testVersion); err != nil {
				t.Logf("pass %d: Ensure: %v", i, err)
			}
		}()
		err = Rotate(dir, port, exe)
		wg.Wait()
		if err != nil {
			t.Fatalf("pass %d: Rotate: %v", i, err)
		}

		if _, err := Health(dir); err != nil {
			t.Fatalf("pass %d: Health with the stored token: %v", i, err)
		}
		fresh, err := EnsureToken(dir)
		if err != nil {
			t.Fatal(err)
		}
		if fresh == old {
			t.Fatalf("pass %d: the token didn't change", i)
		}
		if got := status(t, port, "/api/health", old); got != http.StatusUnauthorized {
			t.Fatalf("pass %d: old token: %d, want 401", i, got)
		}
		if got := status(t, port, "/api/health", key); got != http.StatusUnauthorized {
			t.Fatalf("pass %d: old browser key: %d, want 401", i, got)
		}
		if got := status(t, port, "/api/health", signIn(t, dir)); got != http.StatusOK {
			t.Fatalf("pass %d: new browser key: %d, want 200", i, got)
		}
		if lock, err := tryLock(dir); !errors.Is(err, errLocked) {
			if err == nil {
				lock.Close()
			}
			t.Fatalf("pass %d: board.lock: %v, want a server holding it", i, err)
		}
	}
}

func TestStopWithNothingRunning(t *testing.T) {
	dir := t.TempDir()
	// A board.json left by a server that was killed.
	stale := filepath.Join(dir, "board", "board.json")
	if err := os.MkdirAll(filepath.Dir(stale), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(stale, []byte(`{"pid":1,"port":1}`), 0o600); err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	if err := Stop(dir); err != nil {
		t.Fatal(err)
	}
	if d := time.Since(start); d > time.Second {
		t.Errorf("Stop took %v", d)
	}
	if _, err := os.Stat(stale); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("the stale board.json is still there: %v", err)
	}
	if _, err := Health(dir); err == nil {
		t.Error("Health answered with nothing running")
	}
}

// A herdr session may be called board, and its tasks are then board.json and
// board.lock in the state dir. Starting and stopping the server leave them
// alone, and reading them doesn't wait on the server's lock.
func TestASessionCalledBoardKeepsItsTasks(t *testing.T) {
	dir, port, exe := stateDir(t), freePort(t), helperExe(t)
	st := &store.Store{Dir: dir, Session: "board"}
	err := st.Update(func(s *store.State) error {
		s.Tasks["w1:t1"] = &task.Task{TabID: "w1:t1", Workspace: "acme", Title: "ABC-12 checkout", Stage: task.Active, Since: now}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	saved, err := os.ReadFile(filepath.Join(dir, "board.json"))
	if err != nil {
		t.Fatal(err)
	}
	check := func(when string) {
		t.Helper()
		if got, err := os.ReadFile(filepath.Join(dir, "board.json")); err != nil || string(got) != string(saved) {
			t.Fatalf("%s: the session's board.json is %q, %v; want its tasks", when, got, err)
		}
		loaded := make(chan error, 1)
		go func() {
			_, err := st.Load()
			loaded <- err
		}()
		select {
		case err := <-loaded:
			if err != nil {
				t.Fatalf("%s: %v", when, err)
			}
		case <-time.After(2 * time.Second):
			t.Fatalf("%s: reading the session's tasks waits on the server's lock", when)
		}
	}

	if err := Stop(dir); err != nil {
		t.Fatal(err)
	}
	check("after Stop with no server running")
	if _, err := Health(dir); err == nil {
		t.Error("Health answered with no server running")
	}
	check("after Health with no server running")
	if err := Ensure(dir, port, exe, testVersion); err != nil {
		t.Fatal(err)
	}
	check("with the server running")
	if err := Stop(dir); err != nil {
		t.Fatal(err)
	}
	check("after Stop")
}

// Only a server writes board.json, and it holds the lock while it runs, so
// the board.json that a killed server left goes as soon as anyone takes the
// lock: nobody sends the token to the port it records meanwhile.
func TestTakingTheLockRemovesAStaleRecord(t *testing.T) {
	dir := t.TempDir()
	stale := filepath.Join(dir, "board", "board.json")
	if err := os.MkdirAll(filepath.Dir(stale), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(stale, []byte(`{"pid":1,"port":1}`), 0o600); err != nil {
		t.Fatal(err)
	}
	lock, err := tryLock(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Close()
	if _, err := os.Stat(stale); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("with the lock taken, the stale board.json is still there: %v", err)
	}
}

// A server that shuts down lets go of its port some time before it exits
// and its lock with it: up to 2 s while a connection is open, and anyone can
// open one. Another program may take the port meanwhile. The CLI never
// sends it the admin token, as board.json goes before the port does.
func TestNoTokenGoesToThePortOfAServerShuttingDown(t *testing.T) {
	dir, port, exe := stateDir(t), freePort(t), helperExe(t)
	if err := Ensure(dir, port, exe, testVersion); err != nil {
		t.Fatal(err)
	}
	addr := fmt.Sprintf("127.0.0.1:%d", port)
	quiet, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	defer quiet.Close()
	// Asked after the quiet connection, so the server has taken that one.
	m, err := Health(dir)
	if err != nil {
		t.Fatal(err)
	}

	// The other program takes the port the moment it's free, and keeps the
	// Authorization headers sent to it.
	var mu sync.Mutex
	var sent []string
	taken := make(chan net.Listener, 1)
	done := make(chan struct{})
	defer close(done)
	go func() {
		for {
			if ln, err := net.Listen("tcp", addr); err == nil {
				taken <- ln
				return
			}
			select {
			case <-done:
				return
			case <-time.After(time.Millisecond):
			}
		}
	}()

	if err := shutdown(dir, m, time.Second); err != nil {
		t.Fatal(err)
	}
	var ln net.Listener
	select {
	case ln = <-taken:
	case <-time.After(3 * time.Second):
		t.Fatal("the server never let go of the port")
	}
	defer ln.Close()
	go http.Serve(ln, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if a := r.Header.Get("Authorization"); a != "" {
			mu.Lock()
			sent = append(sent, r.Method+" "+r.URL.Path)
			mu.Unlock()
		}
		http.NotFound(w, r)
	}))

	// Meanwhile herdr-tasks board and the startup hook ask after the server.
	ensured := make(chan error, 1)
	go func() { ensured <- Ensure(dir, port, exe, testVersion) }()
	for end := time.Now().Add(time.Second); time.Now().Before(end); time.Sleep(poll) {
		if _, err := Health(dir); err == nil {
			t.Error("Health answered for a server shutting down")
		}
	}
	if err := <-ensured; !errors.Is(err, ErrPortTaken) {
		t.Logf("Ensure = %v", err)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(sent) > 0 {
		t.Errorf("the admin token went to the program that took the port %d times, first with %s", len(sent), sent[0])
	}
}

func TestServerEnvLeavesOutTheSession(t *testing.T) {
	env := serverEnv([]string{
		"HOME=/home/acme",
		"PATH=/usr/bin",
		"HERDR_SOCKET_PATH=/tmp/herdr.sock",
		"HERDR_SESSION=acme",
		"HERDR_PANE_ID=p1",
		"HERDR_TAB_ID=t1",
		"HERDR_WORKSPACE_ID=w1",
		"HERDR_PLUGIN_EVENT=startup",
		"HERDR_PLUGIN_EVENT_JSON={}",
		"HERDR_PLUGIN_CONTEXT_JSON={}",
		"HERDR_PLUGIN_ACTION_ID=board",
		"HERDR_PLUGIN_CLICKED_URL=https://acme.example",
		"HERDR_PLUGIN_LINK_HANDLER_ID=l1",
		"HERDR_PLUGIN_STATE_DIR=/state",
		"HERDR_PLUGIN_CONFIG_DIR=/config",
		"HERDR_TASKS_BOARD_FDS=0",
		"CLAUDE_CONFIG_DIR=/claude",
	})
	want := []string{
		"HOME=/home/acme",
		"PATH=/usr/bin",
		"HERDR_PLUGIN_STATE_DIR=/state",
		"HERDR_PLUGIN_CONFIG_DIR=/config",
		"CLAUDE_CONFIG_DIR=/claude",
		"HERDR_TASKS_BOARD_FDS=1",
	}
	if !slices.Equal(env, want) {
		t.Errorf("serverEnv =\n%q\nwant\n%q", env, want)
	}
}

func TestBuildIDIsTheSameForACopy(t *testing.T) {
	dir := t.TempDir()
	a, b, c := filepath.Join(dir, "a"), filepath.Join(dir, "b"), filepath.Join(dir, "c")
	for path, body := range map[string]string{a: "build one", b: "build one", c: "build two"} {
		if err := os.WriteFile(path, []byte(body), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Chtimes(b, time.Now(), time.Now().Add(-time.Hour)); err != nil {
		t.Fatal(err)
	}
	if BuildID(a) == "" || BuildID(a) != BuildID(b) {
		t.Errorf("a copy has another build: %q, %q", BuildID(a), BuildID(b))
	}
	if BuildID(a) == BuildID(c) {
		t.Error("two builds have one ID")
	}
}

func TestLoginURLSignsIn(t *testing.T) {
	dir, port := stateDir(t), freePort(t)
	// A board.json left by a server that was killed, at a port another
	// program has now: the token mustn't go there.
	other := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("a request went to the port a killed server left: %s %s", r.Method, r.URL)
	}))
	defer other.Close()
	stale := fmt.Sprintf(`{"pid":1,"port":%d}`, other.Listener.Addr().(*net.TCPAddr).Port)
	if err := os.MkdirAll(filepath.Join(dir, "board"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "board", "board.json"), []byte(stale), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := EnsureToken(dir); err != nil {
		t.Fatal(err)
	}
	if u, err := LoginURL(dir, ""); err == nil {
		t.Errorf("LoginURL with no server running = %q", u)
	}
	if err := Ensure(dir, port, helperExe(t), testVersion); err != nil {
		t.Fatal(err)
	}
	login, err := LoginURL(dir, "")
	if err != nil {
		t.Fatal(err)
	}
	u := login.Local
	prefix := fmt.Sprintf("http://127.0.0.1:%d/?code=", port)
	code, ok := strings.CutPrefix(u, prefix)
	if !ok || code == "" {
		t.Fatalf("LoginURL = %q, want %s<code>", u, prefix)
	}

	// The page swaps the code for the browser key, once.
	st, key := redeem(t, port, code)
	if st != http.StatusOK || !regexp.MustCompile(`^[0-9a-f]{64}$`).MatchString(key) {
		t.Fatalf("session = %d, key %q; want 200 and the browser key", st, key)
	}
	if got := status(t, port, "/api/board", key); got != http.StatusOK {
		t.Errorf("the browser key: %d, want 200", got)
	}
	if st, _ := redeem(t, port, code); st != http.StatusUnauthorized {
		t.Errorf("the code worked twice: %d", st)
	}
}

func TestLoginURLHasSession(t *testing.T) {
	dir, port := stateDir(t), freePort(t)
	if _, err := EnsureToken(dir); err != nil {
		t.Fatal(err)
	}
	if err := Ensure(dir, port, helperExe(t), testVersion); err != nil {
		t.Fatal(err)
	}
	for session, at := range map[string]string{"work": "/work/", "": "/", "my work": "/my%20work/"} {
		login, err := LoginURL(dir, session)
		if err != nil {
			t.Fatal(err)
		}
		prefix := fmt.Sprintf("http://127.0.0.1:%d%s?code=", port, at)
		if code, ok := strings.CutPrefix(login.Local, prefix); !ok || code == "" {
			t.Errorf("LoginURL(%q).Local = %q, want %s<code>", session, login.Local, prefix)
		}
		if login.Tailnet != "" {
			t.Errorf("LoginURL(%q).Tailnet = %q, want none", session, login.Tailnet)
		}
	}
}

// redeem swaps a login code for the browser key at the server on port, as
// the page does, and returns the status and the key.
func redeem(t *testing.T, port int, code string) (int, string) {
	t.Helper()
	body := strings.NewReader(fmt.Sprintf(`{"code":%q}`, code))
	resp, err := (&http.Client{Transport: &http.Transport{DisableKeepAlives: true}}).Post(fmt.Sprintf("http://127.0.0.1:%d/api/session", port), "application/json", body)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var got struct{ Key string }
	_ = json.NewDecoder(resp.Body).Decode(&got)
	return resp.StatusCode, got.Key
}

// signIn signs a browser in to the server running in dir, as herdr-tasks
// board and the page do, and returns its key.
func signIn(t *testing.T, dir string) string {
	t.Helper()
	login, err := LoginURL(dir, "")
	if err != nil {
		t.Fatal(err)
	}
	p, err := url.Parse(login.Local)
	if err != nil {
		t.Fatal(err)
	}
	port, err := strconv.Atoi(p.Port())
	if err != nil {
		t.Fatal(err)
	}
	st, key := redeem(t, port, p.Query().Get("code"))
	if st != http.StatusOK || key == "" {
		t.Fatalf("signing in: %d, key %q", st, key)
	}
	return key
}

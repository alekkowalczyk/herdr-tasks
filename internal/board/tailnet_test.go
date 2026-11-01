package board

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"maps"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/alekkowalczyk/herdr-tasks/internal/herdr"
	"github.com/alekkowalczyk/herdr-tasks/internal/reconcile"
	"github.com/alekkowalczyk/herdr-tasks/internal/tailnet"
	"github.com/alekkowalczyk/herdr-tasks/internal/task"
)

// tailnetTime is when the tailnet tests start, by the server's clock.
var tailnetTime = time.Date(2026, 11, 1, 9, 0, 0, 0, time.UTC)

// onTheTailnet is the settings with the board shared on the tailnet.
var onTheTailnet = reconcile.Options{Board: true, BoardPort: 7391, BoardTailnet: true}

// fakeTS is Tailscale in memory: this machine, its serve entries, and an
// error for Serve to give. It logs every call, as "self", "served",
// "serve <port> <target>" and "off <port>".
type fakeTS struct {
	mu       sync.Mutex
	self     tailnet.Self
	selfErr  error
	served   map[int]string
	serveErr error
	// spell is how Tailscale reports a target it was given; nil reports it
	// as given.
	spell func(string) string
	// hold, when set, keeps Self from answering until it's closed or the
	// call's context ends, as a tailscale that hangs.
	hold chan struct{}
	log  []string
}

func newFakeTS(served map[int]string) *fakeTS {
	if served == nil {
		served = map[int]string{}
	}
	return &fakeTS{self: tailnet.Self{DNSName: "devbox.tail0000.ts.net", Owner: "dana@acme.test"}, served: served}
}

func (f *fakeTS) Self(ctx context.Context) (tailnet.Self, error) {
	f.mu.Lock()
	f.log = append(f.log, "self")
	hold := f.hold
	f.mu.Unlock()
	if hold != nil {
		select {
		case <-hold:
		case <-ctx.Done():
			return tailnet.Self{}, ctx.Err()
		}
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.selfErr != nil {
		return tailnet.Self{}, f.selfErr
	}
	return f.self, nil
}

// failSelf has Self fail with err from now on, or work again with nil.
func (f *fakeTS) failSelf(err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.selfErr = err
}

func (f *fakeTS) Served(context.Context) (map[int]string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.log = append(f.log, "served")
	return maps.Clone(f.served), nil
}

func (f *fakeTS) Serve(_ context.Context, port int, target string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.log = append(f.log, fmt.Sprintf("serve %d %s", port, target))
	if f.serveErr != nil {
		return f.serveErr
	}
	if f.spell != nil {
		target = f.spell(target)
	}
	f.served[port] = target
	return nil
}

func (f *fakeTS) Off(_ context.Context, port int) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.log = append(f.log, fmt.Sprintf("off %d", port))
	delete(f.served, port)
	return nil
}

// setServed has Tailscale serve target at port, as when someone else
// changes its entries.
func (f *fakeTS) setServed(port int, target string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.served[port] = target
}

// calls is every call so far.
func (f *fakeTS) calls() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.log)
}

// changes is the calls so far that change what Tailscale serves.
func (f *fakeTS) changes() []string {
	var out []string
	for _, c := range f.calls() {
		if strings.HasPrefix(c, "serve ") || strings.HasPrefix(c, "off ") {
			out = append(out, c)
		}
	}
	return out
}

// entries is what Tailscale serves now.
func (f *fakeTS) entries() map[int]string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return maps.Clone(f.served)
}

// shortDir is an empty state dir with a short path, as a Unix socket's has
// to be: macOS allows 103 bytes, and t.TempDir's can be longer.
func shortDir(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("/tmp", "ht")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	return dir
}

// boardSock is the board's socket in the state dir.
func boardSock(dir string) string { return filepath.Join(dir, "board", "sockets", "board.sock") }

// tailnetServer is a board server with session work running, Tailscale
// faked, and a clock and settings the test changes.
type tailnetServer struct {
	*testServer
	ts     *fakeTS
	dir    string
	sock   string
	checks atomic.Int64 // the server's checks so far: each lists the sessions once

	mu   sync.Mutex
	now  time.Time
	opts reconcile.Options
}

// startTailnet runs a server in dir with ts and opts, on opts' port. Session
// work has a tab for each task.
func startTailnet(t *testing.T, dir string, ts *fakeTS, opts reconcile.Options, tasks ...*task.Task) *tailnetServer {
	t.Helper()
	f := &tailnetServer{ts: ts, dir: dir, sock: boardSock(dir), now: tailnetTime, opts: opts}
	var tabs []herdr.Tab
	for _, tk := range tasks {
		tabs = append(tabs, herdr.Tab{ID: tk.TabID})
	}
	f.testServer = startServer(t, Config{
		StateDir:  dir,
		Port:      opts.Port(),
		Tailscale: ts,
		Sessions:  &countedSessions{Sessions: running(newConn(t, "work", oneSpace(tabs...), tasks...)), n: &f.checks},
		Options: func() (reconcile.Options, error) {
			f.mu.Lock()
			defer f.mu.Unlock()
			return f.opts, nil
		},
		Now: func() time.Time {
			f.mu.Lock()
			defer f.mu.Unlock()
			return f.now
		},
	}, nil)
	return f
}

// countedSessions counts how many times the sessions were listed.
type countedSessions struct {
	Sessions
	n *atomic.Int64
}

func (c *countedSessions) List() []SessionConn {
	defer c.n.Add(1)
	return c.Sessions.List()
}

func (f *tailnetServer) setTailnet(on bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.opts.BoardTailnet = on
}

func (f *tailnetServer) advance(d time.Duration) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.now = f.now.Add(d)
}

// waitTicks waits up to 3 s for the server to check n times from now. The
// check under way may have read the settings before, and a check syncs with
// Tailscale after it lists the sessions, so it waits for two more lists.
func (f *tailnetServer) waitTicks(t *testing.T, n int) {
	t.Helper()
	until := f.checks.Load() + int64(n) + 2
	for deadline := time.Now().Add(3 * time.Second); f.checks.Load() < until; time.Sleep(5 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatalf("the server didn't check %d times", n)
		}
	}
}

// work waits up to 3 s for the board of session work to be what ok wants,
// and returns it.
func (f *tailnetServer) work(t *testing.T, ok func(Board) bool) Board {
	t.Helper()
	var b Board
	for deadline := time.Now().Add(3 * time.Second); ; time.Sleep(10 * time.Millisecond) {
		req, err := http.NewRequest("GET", f.url("/api/board?session=work"), nil)
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Authorization", "Bearer "+f.key)
		resp, err := plain.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		raw, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode == http.StatusOK {
			b = Board{}
			if err := json.Unmarshal(raw, &b); err != nil {
				t.Fatalf("%s: %v", raw, err)
			}
			if ok(b) {
				return b
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("the board of work is still %s %s", resp.Status, raw)
		}
	}
}

// line waits for the board's tailnet line to be what ok wants.
func (f *tailnetServer) line(t *testing.T, ok func(*TailnetLine) bool) *TailnetLine {
	t.Helper()
	return f.work(t, func(b Board) bool { return b.Tailnet != nil && ok(b.Tailnet) }).Tailnet
}

func shared(l *TailnetLine) bool     { return l.Address != "" }
func hasProblem(l *TailnetLine) bool { return l.Problem != "" }

// loginCode asks for a login code as herdr-tasks board does, and returns
// the reply.
func (f *tailnetServer) loginCode(t *testing.T) map[string]string {
	t.Helper()
	resp := request(t, "POST", f.port, "/api/login-code", testToken)
	defer resp.Body.Close()
	var reply map[string]string
	if err := json.NewDecoder(resp.Body).Decode(&reply); err != nil || resp.StatusCode != http.StatusOK {
		t.Fatalf("login-code: %s, %v", resp.Status, err)
	}
	return reply
}

// unixClient sends every request to the Unix socket sock, as tailscale
// serve does.
func unixClient(sock string) *http.Client {
	return &http.Client{Timeout: 10 * time.Second, Transport: &http.Transport{
		DisableKeepAlives: true,
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			var d net.Dialer
			return d.DialContext(ctx, "unix", sock)
		},
	}}
}

// throughTailnet sends a request to the board's socket as tailscale serve
// does, with Host localhost, and returns the status and the body.
func throughTailnet(t *testing.T, sock, method, path string, headers map[string]string, body string) (int, string) {
	t.Helper()
	req, err := http.NewRequest(method, "http://localhost"+path, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := unixClient(sock).Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b)
}

func TestTailnetSharesBoardWhenOn(t *testing.T) {
	ts := newFakeTS(nil)
	f := startTailnet(t, shortDir(t), ts, onTheTailnet)
	line := f.line(t, shared)
	if line.Address != "https://devbox.tail0000.ts.net:7391" || line.Problem != "" {
		t.Errorf("tailnet line = %+v, want the address and no problem", line)
	}
	if want := []string{"serve 7391 unix:" + f.sock}; !slices.Equal(ts.changes(), want) {
		t.Errorf("Tailscale was asked %q, want %q", ts.changes(), want)
	}
	rec, err := loadRecord(f.dir)
	if err != nil {
		t.Fatal(err)
	}
	if b := rec.Board; b == nil || b.Port != 7391 || b.Target != "unix:"+f.sock || !b.At.Equal(tailnetTime) {
		t.Errorf("recorded %+v, want the board's entry", b)
	}
	for path, want := range map[string]fs.FileMode{
		serverFile(f.dir, "tailnet.json"): 0o600,
		filepath.Dir(f.sock):              0o700 | fs.ModeDir,
		f.sock:                            0o600 | fs.ModeSocket,
	} {
		if fi, err := os.Lstat(path); err != nil || fi.Mode() != want {
			t.Errorf("%s: %v, %v; want %v", path, fi.Mode(), err, want)
		}
	}
	if got := f.tailnetOrigin(); got != line.Address {
		t.Errorf("the tailnet door's Origin = %q, want %q", got, line.Address)
	}
	if reply := f.loginCode(t); reply["tailnet"] != line.Address || reply["code"] == "" {
		t.Errorf("login-code = %q, want a code and the tailnet address", reply)
	}
}

func TestTailnetLeavesOthersEntries(t *testing.T) {
	others := map[int]string{7391: "http://127.0.0.1:9999"}
	ts := newFakeTS(maps.Clone(others))
	f := startTailnet(t, shortDir(t), ts, onTheTailnet)
	line := f.line(t, hasProblem)
	if want := "Port 7391 is already shared with your tailnet for something else."; line.Problem != want || line.Address != "" {
		t.Errorf("tailnet line = %+v, want only %q", line, want)
	}
	f.waitTicks(t, 1)
	if got := ts.changes(); len(got) > 0 {
		t.Errorf("Tailscale was asked %q, want nothing changed", got)
	}
	if !maps.Equal(ts.entries(), others) {
		t.Errorf("Tailscale serves %v, want %v", ts.entries(), others)
	}
	if got := f.tailnetOrigin(); got != "" {
		t.Errorf("the tailnet door's Origin = %q, want none", got)
	}
	if _, err := os.Stat(serverFile(f.dir, "tailnet.json")); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("a record of someone else's entry: %v", err)
	}
	if reply := f.loginCode(t); reply["tailnet"] != "" || reply["tailnetProblem"] != line.Problem {
		t.Errorf("login-code = %q, want the problem and no tailnet address", reply)
	}
}

func TestTailnetOffRemovesOnlyOwnEntry(t *testing.T) {
	// Ours, as recorded: turning it off takes it out.
	dir := shortDir(t)
	ours := "unix:" + boardSock(dir)
	if err := saveRecord(dir, tailnetRecord{Board: &sharedEntry{Port: 7391, Target: ours, At: tailnetTime}}); err != nil {
		t.Fatal(err)
	}
	ts := newFakeTS(map[int]string{7391: ours})
	f := startTailnet(t, dir, ts, onTheTailnet)
	f.line(t, shared)
	if got := ts.changes(); len(got) > 0 {
		t.Errorf("with the board shared already, Tailscale was asked %q", got)
	}
	f.setTailnet(false)
	f.waitTicks(t, 1)
	if want := []string{"off 7391"}; !slices.Equal(ts.changes(), want) {
		t.Errorf("Tailscale was asked %q, want %q", ts.changes(), want)
	}
	if _, err := os.Stat(serverFile(dir, "tailnet.json")); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("the record is still there: %v", err)
	}
	if _, err := os.Lstat(f.sock); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("the socket is still there: %v", err)
	}
	f.work(t, func(b Board) bool { return b.Tailnet == nil })

	// Recorded, but Tailscale serves something else there now: it's left
	// alone, and the record goes.
	dir = shortDir(t)
	if err := saveRecord(dir, tailnetRecord{Board: &sharedEntry{Port: 7391, Target: "unix:" + boardSock(dir), At: tailnetTime}}); err != nil {
		t.Fatal(err)
	}
	ts = newFakeTS(map[int]string{7391: "http://127.0.0.1:9999"})
	f = startTailnet(t, dir, ts, reconcile.Options{Board: true, BoardPort: 7391})
	f.waitTicks(t, 1)
	if got := ts.changes(); len(got) > 0 {
		t.Errorf("Tailscale was asked %q, want nothing changed", got)
	}
	if _, err := os.Stat(serverFile(dir, "tailnet.json")); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("the record is still there: %v", err)
	}
}

func TestNoTailscaleCallsWhenOff(t *testing.T) {
	ts := newFakeTS(nil)
	f := startTailnet(t, shortDir(t), ts, reconcile.Options{Board: true, BoardPort: 7391})
	for range 3 {
		f.advance(time.Minute)
		f.waitTicks(t, 1)
	}
	if got := ts.calls(); len(got) > 0 {
		t.Errorf("with board_tailnet off, Tailscale was asked %q", got)
	}
	if _, err := os.Lstat(f.sock); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("a socket with board_tailnet off: %v", err)
	}
	if b := f.work(t, func(Board) bool { return true }); b.Tailnet != nil {
		t.Errorf("tailnet line with board_tailnet off: %+v", b.Tailnet)
	}
}

func TestTailnetDoorServesBoard(t *testing.T) {
	f := startTailnet(t, shortDir(t), newFakeTS(nil), onTheTailnet)
	address := f.line(t, shared).Address
	signIn := map[string]string{"Origin": address, "Content-Type": "application/json", "Tailscale-User-Login": "dana@acme.test"}
	st, body := throughTailnet(t, f.sock, "POST", "/api/session", signIn, "{}")
	var reply struct{ Key string }
	if err := json.Unmarshal([]byte(body), &reply); err != nil || st != http.StatusOK || reply.Key != f.key {
		t.Fatalf("the owner's sign-in: %d %s, want 200 and the browser key", st, body)
	}
	if st, body := throughTailnet(t, f.sock, "GET", "/api/board?session=work", map[string]string{"Authorization": "Bearer " + reply.Key}, ""); st != http.StatusOK {
		t.Errorf("the board with the key: %d %s, want 200", st, body)
	}
	// Anyone else needs a code.
	signIn["Tailscale-User-Login"] = "rui@acme.test"
	if st, _ := throughTailnet(t, f.sock, "POST", "/api/session", signIn, "{}"); st != http.StatusUnauthorized {
		t.Errorf("another login's sign-in: %d, want 401", st)
	}
}

func TestTailnetProblemShownAndBacksOff(t *testing.T) {
	ts := newFakeTS(nil)
	ts.serveErr = errors.New("access denied (run: sudo tailscale set --operator=$USER)")
	f := startTailnet(t, shortDir(t), ts, onTheTailnet)
	line := f.line(t, hasProblem)
	if !strings.Contains(line.Problem, "tailscale set --operator") || line.Address != "" {
		t.Errorf("tailnet line = %+v, want the operator hint and no address", line)
	}
	serves := func() int { return len(ts.changes()) }
	if serves() != 1 {
		t.Fatalf("Tailscale was asked %q, want one serve", ts.changes())
	}
	for range 5 {
		f.advance(10 * time.Second)
		f.waitTicks(t, 1)
	}
	if serves() != 1 {
		t.Errorf("within a minute of the failure, Tailscale was asked %q", ts.changes())
	}
	if got := status(t, f.port, "/api/board?session=work", f.key); got != http.StatusOK {
		t.Errorf("the 127.0.0.1 board: %d, want 200", got)
	}
	// A minute on, it tries again.
	f.advance(10 * time.Second)
	f.waitTicks(t, 1)
	if serves() != 2 {
		t.Errorf("a minute after the failure, Tailscale was asked %q, want a second serve", ts.changes())
	}
}

func TestStaleSocketReplaced(t *testing.T) {
	dir := shortDir(t)
	// A server that was killed left its socket; this one is a plain file,
	// in a folder that's too open.
	if err := os.MkdirAll(filepath.Dir(boardSock(dir)), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(filepath.Dir(boardSock(dir)), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(boardSock(dir), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	f := startTailnet(t, dir, newFakeTS(nil), onTheTailnet)
	f.line(t, shared)
	st, body := throughTailnet(t, f.sock, "GET", "/api/health", map[string]string{"Authorization": "Bearer " + f.key}, "")
	if st != http.StatusOK || !strings.Contains(body, `"app":"herdr-tasks"`) {
		t.Errorf("health through the socket: %d %s", st, body)
	}
	if fi, err := os.Stat(filepath.Dir(f.sock)); err != nil || fi.Mode().Perm() != 0o700 {
		t.Errorf("the sockets folder: %v, %v; want mode 700", fi.Mode(), err)
	}
}

// Something other than a file or a socket at the socket's path is left
// alone, and the board says so.
func TestTailnetLeavesALinkAtTheSocket(t *testing.T) {
	dir := shortDir(t)
	target := filepath.Join(dir, "kept")
	if err := os.WriteFile(target, []byte("kept"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(boardSock(dir)), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, boardSock(dir)); err != nil {
		t.Fatal(err)
	}
	ts := newFakeTS(nil)
	f := startTailnet(t, dir, ts, onTheTailnet)
	if line := f.line(t, hasProblem); !strings.Contains(line.Problem, f.sock) || line.Address != "" {
		t.Errorf("tailnet line = %+v, want the socket's path as the problem", line)
	}
	if fi, err := os.Lstat(f.sock); err != nil || fi.Mode().Type() != fs.ModeSymlink {
		t.Errorf("the link at the socket's path: %v, %v", fi, err)
	}
	if b, err := os.ReadFile(target); err != nil || string(b) != "kept" {
		t.Errorf("the link's target: %q, %v", b, err)
	}
	if got := ts.calls(); len(got) > 0 {
		t.Errorf("Tailscale was asked %q", got)
	}
}

// A socket's path that's too long for the system is a problem the board
// shows, in words.
func TestTailnetSocketPathTooLong(t *testing.T) {
	dir := filepath.Join(shortDir(t), strings.Repeat("d", 100))
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	f := startTailnet(t, dir, newFakeTS(nil), onTheTailnet)
	if line := f.line(t, hasProblem); !strings.Contains(line.Problem, "too long") {
		t.Errorf("tailnet line = %+v, want the socket's path too long", line)
	}
}

// Tailscale may report the target in its own spelling. That's what's
// recorded, so the board's entry is still its own a minute later.
func TestTailnetRecordsTailscalesSpelling(t *testing.T) {
	ts := newFakeTS(nil)
	ts.spell = func(target string) string { return strings.Replace(target, "unix:", "unix://", 1) }
	f := startTailnet(t, shortDir(t), ts, onTheTailnet)
	f.line(t, shared)
	rec, err := loadRecord(f.dir)
	if err != nil || rec.Board == nil || rec.Board.Target != "unix://"+f.sock {
		t.Fatalf("recorded %+v, %v; want Tailscale's spelling", rec.Board, err)
	}
	f.advance(time.Minute)
	f.waitTicks(t, 1)
	if line := f.line(t, func(*TailnetLine) bool { return true }); line.Problem != "" || line.Address == "" {
		t.Errorf("a minute later: %+v, want it shared", line)
	}
	if got := ts.changes(); len(got) != 1 {
		t.Errorf("Tailscale was asked %q, want one serve", got)
	}
}

// An entry for the board's own socket is the board's, though the record of
// it was lost.
func TestTailnetTakesItsOwnEntryBack(t *testing.T) {
	dir := shortDir(t)
	ts := newFakeTS(map[int]string{7391: "unix:" + boardSock(dir)})
	f := startTailnet(t, dir, ts, onTheTailnet)
	f.line(t, shared)
	if got := ts.changes(); len(got) > 0 {
		t.Errorf("Tailscale was asked %q, want nothing changed", got)
	}
	if rec, err := loadRecord(dir); err != nil || rec.Board == nil || rec.Board.Target != "unix:"+f.sock {
		t.Errorf("recorded %+v, %v; want the board's entry", rec.Board, err)
	}
}

// While it's shared and nothing changes, Tailscale is read once a minute.
func TestTailnetRereadsOnceAMinute(t *testing.T) {
	ts := newFakeTS(nil)
	f := startTailnet(t, shortDir(t), ts, onTheTailnet)
	f.line(t, shared)
	before := len(ts.calls())
	f.advance(50 * time.Second)
	f.waitTicks(t, 3)
	if got := ts.calls(); len(got) != before {
		t.Errorf("within the minute, Tailscale was asked %q", got[before:])
	}
	f.advance(10 * time.Second)
	f.waitTicks(t, 1)
	if got := ts.calls()[before:]; !slices.Equal(got, []string{"self", "served"}) {
		t.Errorf("a minute on, Tailscale was asked %q, want it read", got)
	}
}

// The board moved to another port: its entry on the old one goes, and the
// new one is shared.
func TestTailnetFollowsThePort(t *testing.T) {
	dir := shortDir(t)
	ours := "unix:" + boardSock(dir)
	if err := saveRecord(dir, tailnetRecord{Board: &sharedEntry{Port: 7391, Target: ours, At: tailnetTime}}); err != nil {
		t.Fatal(err)
	}
	ts := newFakeTS(map[int]string{7391: ours})
	opts := onTheTailnet
	opts.BoardPort = 7400
	f := startTailnet(t, dir, ts, opts)
	if line := f.line(t, shared); line.Address != "https://devbox.tail0000.ts.net:7400" {
		t.Errorf("tailnet line = %+v, want port 7400", line)
	}
	if want := []string{"off 7391", "serve 7400 " + ours}; !slices.Equal(ts.changes(), want) {
		t.Errorf("Tailscale was asked %q, want %q", ts.changes(), want)
	}
	if rec, err := loadRecord(dir); err != nil || rec.Board == nil || rec.Board.Port != 7400 {
		t.Errorf("recorded %+v, %v; want port 7400", rec.Board, err)
	}
}

// Turning the board off, with the Slack watcher off too, takes it off the
// tailnet before the server stops.
func TestTailnetOffWithTheBoard(t *testing.T) {
	dir := shortDir(t)
	ours := "unix:" + boardSock(dir)
	if err := saveRecord(dir, tailnetRecord{Board: &sharedEntry{Port: 7391, Target: ours, At: tailnetTime}}); err != nil {
		t.Fatal(err)
	}
	ts := newFakeTS(map[int]string{7391: ours})
	f := startTailnet(t, dir, ts, reconcile.Options{BoardPort: 7391, BoardTailnet: true})
	select {
	case <-f.stopped:
	case <-time.After(3 * time.Second):
		t.Fatal("the server didn't stop")
	}
	if want := []string{"off 7391"}; !slices.Equal(ts.changes(), want) {
		t.Errorf("Tailscale was asked %q, want %q", ts.changes(), want)
	}
}

// A server that stops keeps its entry, so the next one is shared again at
// once.
func TestTailnetKeptWhenTheServerStops(t *testing.T) {
	dir := shortDir(t)
	ts := newFakeTS(nil)
	f := startTailnet(t, dir, ts, onTheTailnet)
	f.line(t, shared)
	f.cancel()
	select {
	case <-f.stopped:
	case <-time.After(3 * time.Second):
		t.Fatal("the server didn't stop")
	}
	if _, err := os.Lstat(f.sock); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("the socket is still there: %v", err)
	}
	f = startTailnet(t, dir, ts, onTheTailnet)
	f.line(t, shared)
	if want := []string{"serve 7391 unix:" + f.sock}; !slices.Equal(ts.changes(), want) {
		t.Errorf("Tailscale was asked %q, want %q", ts.changes(), want)
	}
}

func TestUnshareOnlyOwnEntries(t *testing.T) {
	dir := t.TempDir()
	ours := "unix:" + boardSock(dir)
	share := "unix:" + filepath.Join(dir, "board", "sockets", "share-4100.sock")
	err := saveRecord(dir, tailnetRecord{
		Board:  &sharedEntry{Port: 7391, Target: ours, At: tailnetTime},
		Shares: []sharedEntry{{Port: 4100, Target: share, At: tailnetTime}},
	})
	if err != nil {
		t.Fatal(err)
	}
	ts := newFakeTS(map[int]string{7391: ours, 4100: "http://127.0.0.1:4100"})
	if err := Unshare(dir, ts); err != nil {
		t.Fatal(err)
	}
	if want := []string{"off 7391"}; !slices.Equal(ts.changes(), want) {
		t.Errorf("Tailscale was asked %q, want %q", ts.changes(), want)
	}
	if _, err := os.Stat(serverFile(dir, "tailnet.json")); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("the record is still there: %v", err)
	}
	// With nothing recorded, Tailscale isn't asked.
	ts = newFakeTS(nil)
	if err := Unshare(dir, ts); err != nil || len(ts.calls()) > 0 {
		t.Errorf("Unshare with nothing recorded = %v, asked %q", err, ts.calls())
	}
}

// Turning board_tailnet off ends a stream open through the tailnet at once,
// not after the 2 s other requests get.
func TestTailnetOffEndsItsStreams(t *testing.T) {
	f := startTailnet(t, shortDir(t), newFakeTS(nil), onTheTailnet)
	f.line(t, shared)
	req, err := http.NewRequest("GET", "http://localhost/api/events?session=work", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+f.key)
	resp, err := unixClient(f.sock).Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("events through the tailnet: %s", resp.Status)
	}
	ended := make(chan struct{})
	go func() {
		defer close(ended)
		io.Copy(io.Discard, resp.Body)
	}()
	off := time.Now()
	f.setTailnet(false)
	select {
	case <-ended:
	case <-time.After(5 * time.Second):
		t.Fatal("the stream through the tailnet is still open")
	}
	if took := time.Since(off); took >= 1500*time.Millisecond {
		t.Errorf("the stream ended %v after board_tailnet went off", took)
	}
}

// When Tailscale can't be read while the board is shared, it stays shared:
// the door still answers, and the header says the check failed. It's tried
// again a minute later.
func TestTailnetRecheckFails(t *testing.T) {
	ts := newFakeTS(nil)
	f := startTailnet(t, shortDir(t), ts, onTheTailnet)
	address := f.line(t, shared).Address
	ts.failSelf(errors.New("tailscale status --json: timed out after 10s"))
	f.advance(time.Minute)
	line := f.line(t, hasProblem)
	if want := "Couldn't check the board's tailnet share: tailscale status --json: timed out after 10s"; line.Problem != want || line.Address != address {
		t.Errorf("tailnet line = %+v, want %s and the address", line, want)
	}
	signIn := map[string]string{"Origin": address, "Content-Type": "application/json", "Tailscale-User-Login": "dana@acme.test"}
	if st, body := throughTailnet(t, f.sock, "POST", "/api/session", signIn, "{}"); st != http.StatusOK {
		t.Errorf("the owner's sign-in: %d %s, want 200", st, body)
	}
	if reply := f.loginCode(t); reply["tailnet"] != address || reply["tailnetProblem"] != line.Problem {
		t.Errorf("login-code = %q, want the address and the problem", reply)
	}
	selfs := func() int { return len(slices.DeleteFunc(ts.calls(), func(c string) bool { return c != "self" })) }
	before := selfs()
	f.advance(50 * time.Second)
	f.waitTicks(t, 2)
	if selfs() != before {
		t.Errorf("within a minute of the failure, Tailscale was asked %q", ts.calls())
	}
	ts.failSelf(nil)
	f.advance(10 * time.Second)
	f.line(t, func(l *TailnetLine) bool { return l.Problem == "" && l.Address == address })
	if got := ts.changes(); len(got) != 1 {
		t.Errorf("Tailscale was asked %q, want one serve", got)
	}
}

// A browser leaves :443 out of the Origin, so the address does too.
func TestTailnetAddressLeavesOut443(t *testing.T) {
	for port, want := range map[int]string{443: "https://devbox.tail0000.ts.net", 7391: "https://devbox.tail0000.ts.net:7391"} {
		if got := tailnetAddress("devbox.tail0000.ts.net", port); got != want {
			t.Errorf("port %d: %q, want %q", port, got, want)
		}
	}
}

// The board is built again when the tailnet line changes, and not when a
// check finds it as it was.
func TestShowTailnetSignalsOnlyChanges(t *testing.T) {
	s := NewServer(Config{StateDir: t.TempDir(), Version: testVersion}, NewAuth(testToken), nil)
	built := func() bool {
		select {
		case <-s.hub.dirty:
			return true
		default:
			return false
		}
	}
	const address = "https://devbox.tail0000.ts.net:7391"
	for i, c := range []struct {
		line *TailnetLine
		want bool
	}{
		{nil, true}, // the first
		{nil, false},
		{&TailnetLine{Address: address}, true},
		{&TailnetLine{Address: address}, false},
		{&TailnetLine{Address: address, Problem: "Couldn't check the board's tailnet share: timed out"}, true},
		{nil, true},
	} {
		s.showTailnet(c.line, "dana@acme.test")
		if got := built(); got != c.want {
			t.Errorf("%d: %+v built the board: %v, want %v", i, c.line, got, c.want)
		}
	}
}

// A tailscale that's slow to answer doesn't keep the board from finding the
// sessions.
func TestTailnetSlowDoesntHoldUpSessions(t *testing.T) {
	ts := newFakeTS(nil)
	ts.hold = make(chan struct{})
	f := startTailnet(t, shortDir(t), ts, onTheTailnet)
	f.work(t, func(Board) bool { return true })
	close(ts.hold)
	f.line(t, shared)
}

// failingListener fails to accept once fail is closed.
type failingListener struct {
	net.Listener
	fail chan struct{}
}

func (l *failingListener) Accept() (net.Conn, error) {
	<-l.fail
	return nil, errors.New("accept: the listener broke")
}

// When serving fails, the server stops at once, even while a sync waits on
// tailscale: that call ends with the server's context.
func TestTailnetSyncDoesntHoldUpStopping(t *testing.T) {
	dir := shortDir(t)
	ts := newFakeTS(nil)
	ts.hold = make(chan struct{})
	inner, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ln := &failingListener{Listener: inner, fail: make(chan struct{})}
	s := NewServer(Config{
		StateDir: dir, Port: 7391, Version: testVersion, Tailscale: ts,
		Options: func() (reconcile.Options, error) { return onTheTailnet, nil },
	}, NewAuth(testToken), nil)
	ran := make(chan error, 1)
	stopped := make(chan struct{})
	go func() {
		defer close(stopped)
		ran <- s.Run(context.Background(), ln)
	}()
	// Before dir goes, whatever happens: Tailscale answers, and Run returns.
	t.Cleanup(func() {
		close(ts.hold)
		select {
		case <-stopped:
		case <-time.After(5 * time.Second):
			t.Error("the server never stopped")
		}
	})
	for deadline := time.Now().Add(3 * time.Second); !slices.Contains(ts.calls(), "self"); time.Sleep(5 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatal("the server never asked Tailscale")
		}
	}
	close(ln.fail)
	select {
	case err := <-ran:
		if err == nil {
			t.Error("Run = nil, want the listener's error")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("the server didn't stop while a sync waited on tailscale")
	}
}

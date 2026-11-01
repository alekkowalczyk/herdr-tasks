package board

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"maps"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/alekkowalczyk/herdr-tasks/internal/reconcile"
	"github.com/alekkowalczyk/herdr-tasks/internal/task"
)

// shareTab is the tab of the task the share tests share links of, in
// session work.
const shareTab = "w1:t1"

// linksTask is the task in shareTab, with links to urls.
func linksTask(urls ...string) *task.Task {
	tk := &task.Task{TabID: shareTab, Stage: task.Active, Since: now}
	for _, u := range urls {
		tk.Links = append(tk.Links, task.Link{URL: u, Added: now})
	}
	return tk
}

// shareSock is the socket of a shared port in the state dir.
func shareSock(dir string, port int) string {
	return filepath.Join(dir, "board", "sockets", fmt.Sprintf("share-%d.sock", port))
}

// startSharing runs a tailnet server whose task has links to urls, and
// waits for the board to be on the tailnet.
func startSharing(t *testing.T, dir string, ts *fakeTS, urls ...string) *tailnetServer {
	t.Helper()
	f := startTailnet(t, dir, ts, onTheTailnet, linksTask(urls...))
	f.line(t, shared)
	return f
}

// shareBody is what the page posts to share url, a link on the task.
func shareBody(url string) string {
	b, err := json.Marshal(map[string]string{"session": "work", "tab": shareTab, "url": url})
	if err != nil {
		panic(err)
	}
	return string(b)
}

// tailnetAPI is the headers of a signed-in page on the board's tailnet
// address. It sends no Tailscale login, as from a tagged machine.
func (f *tailnetServer) tailnetAPI() map[string]string {
	return map[string]string{"Origin": f.tailnetOrigin(), "Authorization": "Bearer " + f.key, "Content-Type": "application/json"}
}

// share asks the board, through the tailnet, to share url, and returns the
// status and the reply.
func (f *tailnetServer) share(t *testing.T, url string) (int, map[string]string) {
	t.Helper()
	st, body := throughTailnet(t, f.sock, "POST", "/api/share", f.tailnetAPI(), shareBody(url))
	var reply map[string]string
	if err := json.Unmarshal([]byte(body), &reply); err != nil {
		t.Fatalf("share %s: %d %q: %v", url, st, body, err)
	}
	return st, reply
}

// shareOK shares url and returns the address it's shared at.
func (f *tailnetServer) shareOK(t *testing.T, url string) string {
	t.Helper()
	st, reply := f.share(t, url)
	if st != http.StatusOK || reply["href"] == "" {
		t.Fatalf("share %s: %d %q, want 200 and the address", url, st, reply)
	}
	return reply["href"]
}

// stopSharing asks the board on 127.0.0.1 to stop sharing port, and returns
// the status and the error.
func (f *tailnetServer) stopSharing(t *testing.T, port int) (int, string) {
	t.Helper()
	return f.postTo(t, "/api/unshare", fmt.Sprintf(`{"port":%d}`, port))
}

// serves counts the serve calls for port so far.
func (f *fakeTS) serves(port int) int {
	n := 0
	for _, c := range f.changes() {
		if strings.HasPrefix(c, fmt.Sprintf("serve %d ", port)) {
			n++
		}
	}
	return n
}

// shareClient sends requests to a shared port's socket as tailscale serve
// does, and doesn't follow redirects.
func shareClient(sock string) *http.Client {
	c := unixClient(sock)
	c.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	return c
}

// portOf is the port a test server listens on.
func portOf(srv *httptest.Server) int { return srv.Listener.Addr().(*net.TCPAddr).Port }

func TestShareOnlyThroughTailnet(t *testing.T) {
	ts := newFakeTS(nil)
	f := startSharing(t, shortDir(t), ts, "http://localhost:4100/")
	code, msg := f.postTo(t, "/api/share", shareBody("http://localhost:4100/"))
	if want := "Share ports from the board's tailnet address."; code != http.StatusForbidden || msg != want {
		t.Errorf("share on 127.0.0.1: %d %q, want 403 %q", code, msg, want)
	}
	if n := ts.serves(4100); n > 0 {
		t.Errorf("Tailscale was asked %q", ts.changes())
	}
}

func TestShareServesAndRecords(t *testing.T) {
	ts := newFakeTS(nil)
	dir := shortDir(t)
	link := "http://localhost:4100/app?x=1"
	f := startSharing(t, dir, ts, link)
	if href := f.shareOK(t, link); href != "https://devbox.tail0000.ts.net:4100/app?x=1" {
		t.Errorf("href = %q", href)
	}
	sock := shareSock(dir, 4100)
	if want := "serve 4100 unix:" + sock; !slices.Contains(ts.changes(), want) {
		t.Errorf("Tailscale was asked %q, want %q", ts.changes(), want)
	}
	rec, err := loadRecord(dir)
	if err != nil {
		t.Fatal(err)
	}
	if want := []sharedEntry{{Port: 4100, Target: "unix:" + sock, At: tailnetTime}}; !slices.Equal(rec.Shares, want) {
		t.Errorf("recorded %+v, want %+v", rec.Shares, want)
	}
	if fi, err := os.Lstat(sock); err != nil || fi.Mode() != 0o600|fs.ModeSocket {
		t.Errorf("%s: %v, %v; want a 0600 socket", sock, fi, err)
	}
	line := f.line(t, func(l *TailnetLine) bool { return len(l.Shares) > 0 })
	if len(line.Shares) != 1 || line.Shares[0].Port != 4100 || !line.Shares[0].Since.Equal(tailnetTime) {
		t.Errorf("tailnet line = %+v, want share 4100", line)
	}

	// Two more clicks at once: both get the address, and it's shared once.
	var wg sync.WaitGroup
	errs := make(chan error, 2)
	for range 2 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			req, _ := http.NewRequest("POST", "http://localhost/api/share", strings.NewReader(shareBody(link)))
			for k, v := range f.tailnetAPI() {
				req.Header.Set(k, v)
			}
			resp, err := unixClient(f.sock).Do(req)
			if err != nil {
				errs <- err
				return
			}
			defer resp.Body.Close()
			var reply map[string]string
			if err := json.NewDecoder(resp.Body).Decode(&reply); err != nil || resp.StatusCode != http.StatusOK || reply["href"] != "https://devbox.tail0000.ts.net:4100/app?x=1" {
				errs <- fmt.Errorf("%s %q %v", resp.Status, reply, err)
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Errorf("a click on a shared port: %v", err)
	}
	if n := ts.serves(4100); n != 1 {
		t.Errorf("Tailscale was asked %q, want one serve for 4100", ts.changes())
	}
}

func TestShareRefusesTakenPort(t *testing.T) {
	others := map[int]string{8443: "http://127.0.0.1:8801"}
	ts := newFakeTS(maps.Clone(others))
	f := startSharing(t, shortDir(t), ts, "http://localhost:8443/")
	st, reply := f.share(t, "http://localhost:8443/")
	if want := "Port 8443 is already shared with your tailnet for something else."; st != http.StatusConflict || reply["error"] != want {
		t.Errorf("share: %d %q, want 409 %q", st, reply, want)
	}
	if n := ts.serves(8443); n > 0 {
		t.Errorf("Tailscale was asked %q", ts.changes())
	}
	if got := ts.entries()[8443]; got != others[8443] {
		t.Errorf("Tailscale serves %q at 8443, want %q", got, others[8443])
	}
}

func TestShareRefusesBadLinks(t *testing.T) {
	ts := newFakeTS(nil)
	bad := []string{"http://localhost:0/", "http://localhost:70000/", "http://localhost:7391/", "https://example.test/", "http://localhost/"}
	f := startSharing(t, shortDir(t), ts, bad...)
	for _, url := range bad {
		if st, reply := f.share(t, url); st != http.StatusBadRequest || reply["error"] == "" {
			t.Errorf("%s: %d %q, want 400 with the reason", url, st, reply)
		}
	}
	if st, reply := f.share(t, "http://localhost:4100/"); st != http.StatusNotFound || reply["error"] != "That link isn't on the task." {
		t.Errorf("a link that isn't on the task: %d %q, want 404", st, reply)
	}
	if want := []string{"serve 7391 unix:" + f.sock}; !slices.Equal(ts.changes(), want) {
		t.Errorf("Tailscale was asked %q, want only the board's %q", ts.changes(), want)
	}
}

func TestShareProxyRewritesHost(t *testing.T) {
	var mu sync.Mutex
	var host, origin string
	var p int
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/old":
			http.Redirect(w, r, fmt.Sprintf("http://localhost:%d/next", p), http.StatusFound)
			return
		case "/away":
			http.Redirect(w, r, r.URL.Query().Get("to"), http.StatusFound)
			return
		}
		mu.Lock()
		host, origin = r.Host, r.Header.Get("Origin")
		mu.Unlock()
		io.WriteString(w, "the dev server")
	}))
	defer backend.Close()
	p = portOf(backend)
	dir := shortDir(t)
	link := fmt.Sprintf("http://localhost:%d/", p)
	f := startSharing(t, dir, newFakeTS(nil), link)
	f.shareOK(t, link)
	c := shareClient(shareSock(dir, p))

	get := func(path, from string) *http.Response {
		t.Helper()
		req, err := http.NewRequest("GET", "http://localhost"+path, nil)
		if err != nil {
			t.Fatal(err)
		}
		if from != "" {
			req.Header.Set("Origin", from)
		}
		resp, err := c.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
		return resp
	}
	tailnet := fmt.Sprintf("https://devbox.tail0000.ts.net:%d", p)
	if resp := get("/app", tailnet); resp.StatusCode != http.StatusOK {
		t.Fatalf("through the share: %s", resp.Status)
	}
	mu.Lock()
	if want := fmt.Sprintf("localhost:%d", p); host != want || origin != "http://"+want {
		t.Errorf("the dev server saw Host %q, Origin %q; want %q and %q", host, origin, want, "http://"+want)
	}
	mu.Unlock()
	get("/app", "")
	mu.Lock()
	if origin != "" {
		t.Errorf("the dev server saw Origin %q, though none was sent", origin)
	}
	mu.Unlock()
	if resp := get("/old", ""); resp.Header.Get("Location") != tailnet+"/next" {
		t.Errorf("redirect to %q, want %q", resp.Header.Get("Location"), tailnet+"/next")
	}
	// A redirect anywhere else is left as it is.
	for _, to := range []string{fmt.Sprintf("http://localhost:%d0/x", p), "https://example.test/", "/next"} {
		if resp := get("/away?to="+url.QueryEscape(to), ""); resp.Header.Get("Location") != to {
			t.Errorf("redirect to %q, want %q", resp.Header.Get("Location"), to)
		}
	}
}

// A dev server that's on [::1] only, as Node's often are on macOS, is
// reached all the same.
func TestShareProxyReachesIPv6Loopback(t *testing.T) {
	ln, err := net.Listen("tcp6", "[::1]:0")
	if err != nil {
		t.Skipf("no IPv6 loopback: %v", err)
	}
	p := ln.Addr().(*net.TCPAddr).Port
	if c, err := net.DialTimeout("tcp4", fmt.Sprintf("127.0.0.1:%d", p), time.Second); err == nil {
		c.Close()
		ln.Close()
		t.Skipf("something answers at 127.0.0.1:%d", p)
	}
	backend := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, "on [::1] at "+r.Host)
	}))
	backend.Listener.Close()
	backend.Listener = ln
	backend.Start()
	defer backend.Close()
	dir := shortDir(t)
	link := fmt.Sprintf("http://127.0.0.1:%d/", p)
	f := startSharing(t, dir, newFakeTS(nil), link)
	f.shareOK(t, link)
	resp, err := shareClient(shareSock(dir, p)).Get("http://localhost/")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	if want := fmt.Sprintf("on [::1] at localhost:%d", p); resp.StatusCode != http.StatusOK || string(b) != want {
		t.Errorf("through the share: %s %q, want %q", resp.Status, b, want)
	}
}

func TestShareProxyWebSocket(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.EqualFold(r.Header.Get("Connection"), "Upgrade") || r.Header.Get("Upgrade") != "echo" {
			http.Error(w, "not an upgrade", http.StatusBadRequest)
			return
		}
		conn, rw, err := http.NewResponseController(w).Hijack()
		if err != nil {
			return
		}
		defer conn.Close()
		rw.WriteString("HTTP/1.1 101 Switching Protocols\r\nConnection: Upgrade\r\nUpgrade: echo\r\n\r\n")
		rw.Flush()
		line, err := rw.ReadString('\n')
		if err != nil {
			return
		}
		rw.WriteString(line)
		rw.Flush()
		io.Copy(io.Discard, rw) // until the proxy lets go
	}))
	defer backend.Close()
	p := portOf(backend)
	dir := shortDir(t)
	link := fmt.Sprintf("http://localhost:%d/", p)
	f := startSharing(t, dir, newFakeTS(nil), link)
	f.shareOK(t, link)

	conn, err := net.Dial("unix", shareSock(dir, p))
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	conn.SetDeadline(time.Now().Add(5 * time.Second))
	io.WriteString(conn, "GET /hmr HTTP/1.1\r\nHost: localhost\r\nConnection: Upgrade\r\nUpgrade: echo\r\n\r\n")
	br := bufio.NewReader(conn)
	resp, err := http.ReadResponse(br, nil)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusSwitchingProtocols {
		t.Fatalf("upgrade through the share: %s", resp.Status)
	}
	io.WriteString(conn, "hello\n")
	if line, err := br.ReadString('\n'); err != nil || line != "hello\n" {
		t.Fatalf("echo through the share: %q, %v", line, err)
	}

	// Stopping the share ends the connection.
	if code, msg := f.stopSharing(t, p); code != http.StatusNoContent {
		t.Fatalf("stop sharing: %d %s", code, msg)
	}
	conn.SetDeadline(time.Now().Add(3 * time.Second))
	if _, err := br.ReadByte(); !errors.Is(err, io.EOF) {
		t.Errorf("after the share stopped, the upgraded connection reads %v, want EOF", err)
	}
}

func TestShareProxyNothingListening(t *testing.T) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	p := l.Addr().(*net.TCPAddr).Port
	l.Close()
	dir := shortDir(t)
	link := fmt.Sprintf("http://localhost:%d/", p)
	f := startSharing(t, dir, newFakeTS(nil), link)
	f.shareOK(t, link)
	resp, err := shareClient(shareSock(dir, p)).Get("http://localhost/")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	if want := fmt.Sprintf("Nothing answers at localhost:%d.", p); resp.StatusCode != http.StatusBadGateway || !strings.Contains(string(b), want) {
		t.Errorf("with no dev server: %s %q, want 502 %q", resp.Status, b, want)
	}
}

func TestUnshareOnlyOwn(t *testing.T) {
	ts := newFakeTS(nil)
	dir := shortDir(t)
	link := "http://localhost:4100/"
	f := startSharing(t, dir, ts, link)
	f.shareOK(t, link)
	f.line(t, func(l *TailnetLine) bool { return len(l.Shares) == 1 })
	if code, msg := f.stopSharing(t, 4100); code != http.StatusNoContent {
		t.Fatalf("stop sharing 4100: %d %s", code, msg)
	}
	if !slices.Contains(ts.changes(), "off 4100") {
		t.Errorf("Tailscale was asked %q, want off 4100", ts.changes())
	}
	if _, err := os.Lstat(shareSock(dir, 4100)); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("the share's socket is still there: %v", err)
	}
	f.line(t, func(l *TailnetLine) bool { return len(l.Shares) == 0 })

	// Not one of herdr-tasks' shares.
	if code, msg := f.stopSharing(t, 8443); code != http.StatusNotFound || msg != "herdr-tasks isn't sharing port 8443." {
		t.Errorf("stop sharing 8443: %d %q, want 404", code, msg)
	}

	// Shared again, then someone else serves something else there.
	f.shareOK(t, link)
	ts.setServed(4100, "http://127.0.0.1:9999")
	offs := len(slices.DeleteFunc(ts.changes(), func(c string) bool { return c != "off 4100" }))
	if code, msg := f.stopSharing(t, 4100); code != http.StatusNotFound || msg != "herdr-tasks isn't sharing port 4100." {
		t.Errorf("stop sharing someone else's 4100: %d %q, want 404", code, msg)
	}
	if n := len(slices.DeleteFunc(ts.changes(), func(c string) bool { return c != "off 4100" })); n != offs {
		t.Errorf("Tailscale was asked %q", ts.changes())
	}
	if got := ts.entries()[4100]; got != "http://127.0.0.1:9999" {
		t.Errorf("Tailscale serves %q at 4100, want someone else's entry kept", got)
	}
	if rec, err := loadRecord(dir); err != nil || len(rec.Shares) > 0 {
		t.Errorf("recorded %+v, %v; want no shares", rec.Shares, err)
	}
}

// A server that stops leaves its shares in Tailscale, and the next one
// listens for them again at its first look.
func TestSharesComeBackAfterRestart(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, "the dev server")
	}))
	defer backend.Close()
	p := portOf(backend)
	dir := shortDir(t)
	ts := newFakeTS(nil)
	link := fmt.Sprintf("http://localhost:%d/", p)
	f := startSharing(t, dir, ts, link)
	f.shareOK(t, link)
	f.cancel()
	select {
	case <-f.stopped:
	case <-time.After(3 * time.Second):
		t.Fatal("the server didn't stop")
	}
	sock := shareSock(dir, p)
	if _, err := os.Lstat(sock); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("the share's socket is still there: %v", err)
	}
	if rec, err := loadRecord(dir); err != nil || len(rec.Shares) != 1 {
		t.Fatalf("recorded %+v, %v; want the share", rec.Shares, err)
	}

	f = startTailnet(t, dir, ts, onTheTailnet, linksTask(link))
	f.line(t, func(l *TailnetLine) bool { return len(l.Shares) == 1 && l.Shares[0].Port == p })
	resp, err := shareClient(sock).Get("http://localhost/")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if b, _ := io.ReadAll(resp.Body); resp.StatusCode != http.StatusOK || string(b) != "the dev server" {
		t.Errorf("through the share after a restart: %s %q", resp.Status, b)
	}
	if got := ts.changes(); slices.ContainsFunc(got, func(c string) bool { return strings.HasPrefix(c, "off ") }) || ts.serves(p) != 1 {
		t.Errorf("Tailscale was asked %q, want the share served once and never taken out", got)
	}
}

func TestTailnetOffStopsShares(t *testing.T) {
	dir := shortDir(t)
	board := "unix:" + boardSock(dir)
	ours := "unix:" + shareSock(dir, 4100)
	err := saveRecord(dir, tailnetRecord{
		Board:  &sharedEntry{Port: 7391, Target: board, At: tailnetTime},
		Shares: []sharedEntry{{Port: 4100, Target: ours, At: tailnetTime}},
	})
	if err != nil {
		t.Fatal(err)
	}
	ts := newFakeTS(map[int]string{7391: board, 4100: ours})
	f := startTailnet(t, dir, ts, onTheTailnet)
	f.line(t, func(l *TailnetLine) bool { return len(l.Shares) == 1 })
	if _, err := os.Lstat(shareSock(dir, 4100)); err != nil {
		t.Fatalf("the share's socket: %v", err)
	}
	f.setTailnet(false)
	f.waitTicks(t, 1)
	if want := []string{"off 7391", "off 4100"}; !slices.Equal(ts.changes(), want) {
		t.Errorf("Tailscale was asked %q, want %q", ts.changes(), want)
	}
	if _, err := os.Lstat(shareSock(dir, 4100)); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("the share's socket is still there: %v", err)
	}
	if _, err := os.Stat(serverFile(dir, "tailnet.json")); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("the record is still there: %v", err)
	}

	// Off from the start: only the entries still as recorded go.
	dir = shortDir(t)
	board = "unix:" + boardSock(dir)
	ours = "unix:" + shareSock(dir, 4100)
	err = saveRecord(dir, tailnetRecord{
		Board: &sharedEntry{Port: 7391, Target: board, At: tailnetTime},
		Shares: []sharedEntry{
			{Port: 4100, Target: ours, At: tailnetTime},
			{Port: 8443, Target: "unix:" + shareSock(dir, 8443), At: tailnetTime},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	ts = newFakeTS(map[int]string{7391: board, 4100: ours, 8443: "http://127.0.0.1:8801"})
	f = startTailnet(t, dir, ts, reconcile.Options{Board: true, BoardPort: 7391})
	f.waitTicks(t, 1)
	if want := []string{"off 7391", "off 4100"}; !slices.Equal(ts.changes(), want) {
		t.Errorf("Tailscale was asked %q, want %q", ts.changes(), want)
	}
	if got := ts.entries()[8443]; got != "http://127.0.0.1:8801" {
		t.Errorf("Tailscale serves %q at 8443, want someone else's entry kept", got)
	}
}

// A share whose serve fails isn't recorded, and its socket goes.
func TestShareServeFails(t *testing.T) {
	ts := newFakeTS(nil)
	dir := shortDir(t)
	f := startSharing(t, dir, ts, "http://localhost:4100/")
	ts.mu.Lock()
	ts.serveErr = errors.New("access denied (run: sudo tailscale set --operator=$USER)")
	ts.mu.Unlock()
	st, reply := f.share(t, "http://localhost:4100/")
	if st != http.StatusBadGateway || !strings.Contains(reply["error"], "tailscale set --operator") {
		t.Errorf("share: %d %q, want 502 with the operator hint", st, reply)
	}
	if _, err := os.Lstat(shareSock(dir, 4100)); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("the share's socket is still there: %v", err)
	}
	if rec, err := loadRecord(dir); err != nil || len(rec.Shares) > 0 {
		t.Errorf("recorded %+v, %v; want no shares", rec.Shares, err)
	}
}

// A share waits for a sync with Tailscale under way, but not forever.
func TestTailnetLockGivesUp(t *testing.T) {
	var tl tailnetState
	tl.mu.Lock()
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if tl.lock(ctx) {
		t.Fatal("took a lock that's held")
	}
	tl.mu.Unlock()
	ctx, cancel = context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if !tl.lock(ctx) {
		t.Fatal("couldn't take the lock once it was let go")
	}
	tl.mu.Unlock()
}

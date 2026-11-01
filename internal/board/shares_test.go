package board

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log"
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
	"sync/atomic"
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
	// 8443 proxies over HTTPS, and 5432 is a --tcp forward.
	others := map[int]string{8443: "http://127.0.0.1:8801", 5432: "127.0.0.1:5432"}
	ts := newFakeTS(maps.Clone(others))
	f := startSharing(t, shortDir(t), ts, "http://localhost:8443/", "http://localhost:5432/")
	for _, port := range []int{8443, 5432} {
		st, reply := f.share(t, fmt.Sprintf("http://localhost:%d/", port))
		if want := fmt.Sprintf("Port %d is already shared with your tailnet for something else.", port); st != http.StatusConflict || reply["error"] != want {
			t.Errorf("share %d: %d %q, want 409 %q", port, st, reply, want)
		}
		if n := ts.serves(port); n > 0 {
			t.Errorf("Tailscale was asked %q", ts.changes())
		}
	}
	if !maps.Equal(ts.entries(), map[int]string{8443: others[8443], 5432: others[5432], 7391: "unix:" + f.sock}) {
		t.Errorf("Tailscale serves %v, want the others' entries kept", ts.entries())
	}
}

func TestShareRefusesBadLinks(t *testing.T) {
	ts := newFakeTS(nil)
	bad := map[string]string{
		"http://localhost:0/":     "There's no port 0: ports go from 1 to 65535.",
		"http://localhost:70000/": "There's no port 70000: ports go from 1 to 65535.",
		"http://localhost:7391/":  "Port 7391 is the board's own.",
		"https://example.test/":   "Only localhost links can be shared.",
		"http://localhost/":       "Only a localhost link with a port can be shared.",
	}
	f := startSharing(t, shortDir(t), ts, slices.Sorted(maps.Keys(bad))...)
	for link, why := range bad {
		if st, reply := f.share(t, link); st != http.StatusBadRequest || reply["error"] != why {
			t.Errorf("%s: %d %q, want 400 %q", link, st, reply, why)
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

// syncBuffer is a log the server writes to while the test reads it.
type syncBuffer struct {
	mu sync.Mutex
	b  strings.Builder
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

// logged has the log written to a buffer until the test ends.
func logged(t *testing.T) *syncBuffer {
	b := &syncBuffer{}
	log.SetOutput(b)
	t.Cleanup(func() { log.SetOutput(os.Stderr) })
	return b
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
	logs := logged(t)
	resp, err := shareClient(shareSock(dir, p)).Get("http://localhost/")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	if want := fmt.Sprintf("Nothing answers at localhost:%d.", p); resp.StatusCode != http.StatusBadGateway || !strings.Contains(string(b), want) {
		t.Errorf("with no dev server: %s %q, want 502 %q", resp.Status, b, want)
	}
	// The log says why, once.
	if got := strings.Count(logs.String(), "connection refused"); got != 1 {
		t.Errorf("the log says connection refused %d times, want once:\n%s", got, logs)
	}
	if resp, err := shareClient(shareSock(dir, p)).Get("http://localhost/"); err == nil {
		resp.Body.Close()
	}
	if got := strings.Count(logs.String(), "connection refused"); got != 1 {
		t.Errorf("after a second request, the log says connection refused %d times, want once", got)
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

// A shared port answers only the tailnet: a request from the internet,
// through Funnel, which Tailscale marks, never reaches the dev server.
func TestShareRefusesFunnel(t *testing.T) {
	var reached atomic.Int64
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		reached.Add(1)
		io.WriteString(w, "the dev server")
	}))
	defer backend.Close()
	p := portOf(backend)
	dir := shortDir(t)
	link := fmt.Sprintf("http://localhost:%d/", p)
	f := startSharing(t, dir, newFakeTS(nil), link)
	f.shareOK(t, link)
	req, err := http.NewRequest("GET", "http://localhost/", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Tailscale-Funnel-Request", "?1")
	resp, err := shareClient(shareSock(dir, p)).Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	if want := fmt.Sprintf("localhost:%d isn't shared with the internet.\n", p); resp.StatusCode != http.StatusForbidden || string(b) != want {
		t.Errorf("through Funnel: %s %q, want 403 %q", resp.Status, b, want)
	}
	if reached.Load() > 0 {
		t.Error("the request reached the dev server")
	}
}

// A share recorded at the port the board has moved to is only forgotten:
// the board's own port is never shared, and its entry in Tailscale may be
// the board's now, so it's left alone.
func TestShareAtTheBoardsPortForgotten(t *testing.T) {
	dir := shortDir(t)
	share := "unix:" + shareSock(dir, 7391)
	if err := saveRecord(dir, tailnetRecord{Shares: []sharedEntry{{Port: 7391, Target: share, At: tailnetTime}}}); err != nil {
		t.Fatal(err)
	}
	ts := newFakeTS(map[int]string{7391: share})
	f := startTailnet(t, dir, ts, onTheTailnet)
	f.line(t, hasProblem)
	f.waitTicks(t, 1)
	if _, err := os.Lstat(shareSock(dir, 7391)); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("listening for a share at the board's port: %v", err)
	}
	if rec, err := loadRecord(dir); err == nil && len(rec.Shares) > 0 {
		t.Errorf("recorded %+v; want the share forgotten", rec.Shares)
	}
	if got := ts.changes(); len(got) > 0 {
		t.Errorf("Tailscale was asked %q, want nothing changed", got)
	}
	if line := f.line(t, func(*TailnetLine) bool { return true }); len(line.Shares) > 0 {
		t.Errorf("the header lists %+v", line.Shares)
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

// A share Tailscale doesn't list after its serve didn't take: it's refused
// with why, isn't recorded, and its socket goes.
func TestShareNotTaken(t *testing.T) {
	ts := newFakeTS(nil)
	dir := shortDir(t)
	f := startSharing(t, dir, ts, "http://localhost:4100/")
	ts.mu.Lock()
	ts.drop = true
	ts.mu.Unlock()
	st, reply := f.share(t, "http://localhost:4100/")
	want := "Couldn't share port 4100 with your tailnet: Tailscale didn't take the share of port 4100. Is HTTPS turned on for your tailnet?"
	if st != http.StatusBadGateway || reply["error"] != want {
		t.Errorf("share: %d %q, want 502 %q", st, reply, want)
	}
	if _, err := os.Lstat(shareSock(dir, 4100)); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("the share's socket is still there: %v", err)
	}
	if rec, err := loadRecord(dir); err != nil || len(rec.Shares) > 0 {
		t.Errorf("recorded %+v, %v; want no shares", rec.Shares, err)
	}
}

// A dev server on https is shared over TLS, also after a restart. A link to
// the same port over http then opens the share as it is.
func TestShareTLS(t *testing.T) {
	var mu sync.Mutex
	var host, origin string
	var p int
	backend := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/old" {
			http.Redirect(w, r, fmt.Sprintf("https://localhost:%d/next", p), http.StatusFound)
			return
		}
		mu.Lock()
		host, origin = r.Host, r.Header.Get("Origin")
		mu.Unlock()
		io.WriteString(w, "the dev server, on https")
	}))
	defer backend.Close()
	p = portOf(backend)
	dir := shortDir(t)
	ts := newFakeTS(nil)
	link := fmt.Sprintf("https://localhost:%d/app", p)
	plainLink := fmt.Sprintf("http://localhost:%d/other", p)
	f := startSharing(t, dir, ts, link, plainLink)
	tailnet := fmt.Sprintf("https://devbox.tail0000.ts.net:%d", p)
	if href := f.shareOK(t, link); href != tailnet+"/app" {
		t.Errorf("href = %q", href)
	}
	if rec, err := loadRecord(dir); err != nil || len(rec.Shares) != 1 || !rec.Shares[0].TLS {
		t.Fatalf("recorded %+v, %v; want a TLS share", rec.Shares, err)
	}
	c := shareClient(shareSock(dir, p))
	get := func(path, from string) (*http.Response, string) {
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
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return resp, string(b)
	}
	if resp, body := get("/app", tailnet); resp.StatusCode != http.StatusOK || body != "the dev server, on https" {
		t.Fatalf("through the share: %s %q", resp.Status, body)
	}
	mu.Lock()
	if want := fmt.Sprintf("localhost:%d", p); host != want || origin != "https://"+want {
		t.Errorf("the dev server saw Host %q, Origin %q; want %q and %q", host, origin, want, "https://"+want)
	}
	mu.Unlock()
	if resp, _ := get("/old", ""); resp.Header.Get("Location") != tailnet+"/next" {
		t.Errorf("redirect to %q, want %q", resp.Header.Get("Location"), tailnet+"/next")
	}

	// The same port from an http link: the share as it is.
	if href := f.shareOK(t, plainLink); href != tailnet+"/other" {
		t.Errorf("href = %q", href)
	}
	if rec, err := loadRecord(dir); err != nil || len(rec.Shares) != 1 || !rec.Shares[0].TLS || ts.serves(p) != 1 {
		t.Errorf("recorded %+v, %v, asked %q; want the TLS share kept", rec.Shares, err, ts.changes())
	}

	// After a restart, still over TLS.
	f.cancel()
	select {
	case <-f.stopped:
	case <-time.After(3 * time.Second):
		t.Fatal("the server didn't stop")
	}
	f = startTailnet(t, dir, ts, onTheTailnet, linksTask(link))
	f.line(t, func(l *TailnetLine) bool { return len(l.Shares) == 1 })
	if resp, body := get("/app", ""); resp.StatusCode != http.StatusOK || body != "the dev server, on https" {
		t.Errorf("through the share after a restart: %s %q", resp.Status, body)
	}
}

// Stopping a share closes its connections to the dev server, kept open
// for the next request until then.
func TestStopClosesIdleConnections(t *testing.T) {
	var open atomic.Int64
	backend := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, "the dev server")
	}))
	backend.Config.ConnState = func(_ net.Conn, st http.ConnState) {
		switch st {
		case http.StateNew:
			open.Add(1)
		case http.StateClosed, http.StateHijacked:
			open.Add(-1)
		}
	}
	backend.Start()
	defer backend.Close()
	p := portOf(backend)
	dir := shortDir(t)
	link := fmt.Sprintf("http://localhost:%d/", p)
	f := startSharing(t, dir, newFakeTS(nil), link)
	f.shareOK(t, link)
	resp, err := shareClient(shareSock(dir, p)).Get("http://localhost/")
	if err != nil {
		t.Fatal(err)
	}
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if open.Load() != 1 {
		t.Fatalf("the dev server has %d connections open, want the proxy's one", open.Load())
	}
	if code, msg := f.stopSharing(t, p); code != http.StatusNoContent {
		t.Fatalf("stop sharing: %d %s", code, msg)
	}
	for deadline := time.Now().Add(3 * time.Second); open.Load() > 0; time.Sleep(10 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatalf("after the share stopped, the dev server still has %d connections open", open.Load())
		}
	}
}

// A share Tailscale serves but that can't be recorded is taken back out,
// and the answer says so, with why taking it out failed too.
func TestShareUndoneWhenNotRecorded(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root writes in a read-only folder, so the record can't be made to fail")
	}
	ts := newFakeTS(nil)
	dir := shortDir(t)
	f := startSharing(t, dir, ts, "http://localhost:4100/")
	board := filepath.Dir(serverFile(dir, "tailnet.json"))
	if err := os.Chmod(board, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chmod(board, 0o700) })
	ts.mu.Lock()
	ts.offErr = errors.New("tailscale serve: timed out after 10s")
	ts.mu.Unlock()
	st, reply := f.share(t, "http://localhost:4100/")
	if st != http.StatusInternalServerError || !strings.Contains(reply["error"], "Couldn't record the share") || !strings.Contains(reply["error"], "timed out after 10s") {
		t.Errorf("share: %d %q, want 500 saying it wasn't recorded, nor taken out", st, reply)
	}
	if !slices.Contains(ts.changes(), "off 4100") {
		t.Errorf("Tailscale was asked %q, want off 4100", ts.changes())
	}
	if _, err := os.Lstat(shareSock(dir, 4100)); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("the share's socket is still there: %v", err)
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

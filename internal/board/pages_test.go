package board

import (
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/alekkowalczyk/herdr-tasks/internal/herdr"
	"github.com/alekkowalczyk/herdr-tasks/internal/task"
)

// pageFixture is a temporary HOME with a report page and what's around it, a
// board that has the page and home's top.html on a task, and a server.
type pageFixture struct {
	ts     *testServer
	home   string
	report string
	top    string
}

func startPages(t *testing.T) *pageFixture {
	t.Helper()
	home, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", home)
	dir := filepath.Join(home, "acme", "report")
	for name, text := range map[string]string{
		"acme/report/report.html":      "<!doctype html><title>Report</title>",
		"acme/report/img/chart.png":    "png",
		"acme/report/fonts/x.woff2":    "font",
		"acme/report/.git/config":      "[core]",
		"acme/report/certs/server.key": "not a key",
		"acme/report/.env":             "A=1",
		"outside.txt":                  "outside",
		"top.html":                     "<!doctype html><title>Top</title>",
		"second.html":                  "<!doctype html><title>Second</title>",
		".ssh/keys.html":               "<!doctype html><title>Keys</title>",
		".ssh/config":                  "Host acme",
		"acme/report/id_r\u017fa":      "not a key",
		"acme/report/.hidden.html":     "<!doctype html>",
	} {
		p := filepath.Join(home, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(text), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Mkdir(filepath.Join(dir, "notes"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(home, "outside.txt"), filepath.Join(dir, "link-out")); err != nil {
		t.Fatal(err)
	}
	f := &pageFixture{home: home, report: filepath.Join(dir, "report.html"), top: filepath.Join(home, "top.html")}
	work := newConn(t, "work", oneSpace(herdr.Tab{ID: "w1:t1"}),
		&task.Task{TabID: "w1:t1", Stage: task.Active, Since: now, Links: []task.Link{
			{URL: task.FileURL(f.report), Added: now}, {URL: task.FileURL(f.top), Added: now},
			{URL: task.FileURL(filepath.Join(home, "second.html")), Added: now},
			{URL: task.FileURL(filepath.Join(home, ".ssh", "keys.html")), Added: now},
			{URL: task.FileURL(filepath.Join(dir, ".hidden.html")), Added: now},
		}})
	f.ts = startServer(t, Config{Sessions: running(work)}, nil)
	f.ts.shows(t, "work", "w1:t1")
	return f
}

// href asks for the address of a page of the task.
func (f *pageFixture) href(t *testing.T, file string) string {
	t.Helper()
	var reply map[string]string
	b := `{"session":"work","tab":"w1:t1","url":"` + task.FileURL(file) + `"}`
	req, err := http.NewRequest("POST", f.ts.url("/api/open"), strings.NewReader(b))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+f.ts.key)
	resp, err := plain.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("open %s: %d %s", file, resp.StatusCode, raw)
	}
	if err := json.Unmarshal(raw, &reply); err != nil {
		t.Fatal(err)
	}
	if len(reply) != 1 || reply["href"] == "" {
		t.Fatalf("open reply %s, want only an href", raw)
	}
	return reply["href"]
}

// page gets a path of the board without the key, as a page's browser does.
func (f *pageFixture) page(t *testing.T, path string) (*http.Response, string) {
	t.Helper()
	resp, err := plain.Get(f.ts.url(path))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	return resp, string(raw)
}

var pageHref = regexp.MustCompile(`^/-/page/[0-9a-f]{32}/`)

func TestOpenGivesPageAddress(t *testing.T) {
	f := startPages(t)
	href := f.href(t, f.report)
	if !pageHref.MatchString(href) || strings.TrimPrefix(href, pageHref.FindString(href)) != "report.html" {
		t.Fatalf("href %q, want /-/page/<32 hex>/report.html", href)
	}
	if again := f.href(t, f.report); again != href {
		t.Errorf("asked twice: %q then %q", href, again)
	}
	other := filepath.Join(f.home, "outside.html")
	if code, _ := f.ts.openLink(t, "w1:t1", task.FileURL(other)); code != http.StatusNotFound {
		t.Errorf("a link that isn't on the task: %d, want 404", code)
	}
}

func TestPageServesFolderFiles(t *testing.T) {
	f := startPages(t)
	href := f.href(t, f.report)
	resp, body := f.page(t, href)
	if resp.StatusCode != 200 || !strings.HasPrefix(resp.Header.Get("Content-Type"), "text/html") || !strings.Contains(body, "Report") {
		t.Fatalf("the page: %d %q %q", resp.StatusCode, resp.Header.Get("Content-Type"), body)
	}
	csp := resp.Header.Get("Content-Security-Policy")
	if want := "sandbox allow-scripts allow-forms allow-modals allow-popups allow-popups-to-escape-sandbox allow-downloads"; csp != want {
		t.Errorf("CSP %q, want %q", csp, want)
	}
	if strings.Contains(csp, "allow-same-origin") {
		t.Errorf("CSP %q lets the page act as the board", csp)
	}
	for h, want := range map[string]string{
		"X-Content-Type-Options": "nosniff", "Cache-Control": "no-store", "Referrer-Policy": "no-referrer",
	} {
		if got := resp.Header.Get(h); got != want {
			t.Errorf("%s: %q, want %q", h, got, want)
		}
	}
	if got := resp.Header.Get("Access-Control-Allow-Origin"); got != "" {
		t.Errorf("the page has ACAO %q", got)
	}
	resp, body = f.page(t, strings.TrimSuffix(href, "report.html")+"img/chart.png")
	if resp.StatusCode != 200 || resp.Header.Get("Content-Type") != "image/png" || body != "png" || resp.Header.Get("Access-Control-Allow-Origin") != "" {
		t.Errorf("chart.png: %d %q %q ACAO %q", resp.StatusCode, resp.Header.Get("Content-Type"), body, resp.Header.Get("Access-Control-Allow-Origin"))
	}
}

func TestPageFontsAllowCORS(t *testing.T) {
	f := startPages(t)
	dir := strings.TrimSuffix(f.href(t, f.report), "report.html")
	resp, _ := f.page(t, dir+"fonts/x.woff2")
	if resp.StatusCode != 200 || resp.Header.Get("Access-Control-Allow-Origin") != "*" {
		t.Errorf("x.woff2: %d ACAO %q, want 200 and *", resp.StatusCode, resp.Header.Get("Access-Control-Allow-Origin"))
	}
}

func TestPageRefuses(t *testing.T) {
	f := startPages(t)
	dir := strings.TrimSuffix(f.href(t, f.report), "report.html")
	for _, p := range []string{
		dir + "%2e%2e/outside.txt", dir + "..%2foutside.txt", dir + "../outside.txt", dir + ".git/config",
		dir + "certs/server.key", dir + ".env", dir + "notes/", dir + "notes", dir + "link-out", dir + "id_r%C5%BFa", dir + "nothing.txt", dir,
		"/-/page/" + strings.Repeat("0", 32) + "/report.html",
	} {
		if resp, _ := f.page(t, p); resp.StatusCode != http.StatusNotFound {
			t.Errorf("GET %s: %d, want 404", p, resp.StatusCode)
		}
	}
	resp, body := f.page(t, "/-/page/"+strings.Repeat("0", 32)+"/report.html")
	if want := "This page's address works until the board's server restarts. Open it from the board again."; !strings.Contains(body, want) {
		t.Errorf("an unknown id said %q (%d)", body, resp.StatusCode)
	}
}

// A file replaced by a link out of the folder, after the address was made, is
// still refused.
func TestPageRefusesALinkMadeLater(t *testing.T) {
	f := startPages(t)
	dir := strings.TrimSuffix(f.href(t, f.report), "report.html")
	img := filepath.Join(filepath.Dir(f.report), "img", "chart.png")
	if err := os.Remove(img); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(f.home, "outside.txt"), img); err != nil {
		t.Fatal(err)
	}
	if resp, _ := f.page(t, dir+"img/chart.png"); resp.StatusCode != http.StatusNotFound {
		t.Errorf("a link out: %d, want 404", resp.StatusCode)
	}
	// A link inside the folder to a secret is refused by its real name.
	if err := os.Remove(img); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(filepath.Dir(f.report), "certs", "server.key"), img); err != nil {
		t.Fatal(err)
	}
	if resp, _ := f.page(t, dir+"img/chart.png"); resp.StatusCode != http.StatusNotFound {
		t.Errorf("a link to a key: %d, want 404", resp.StatusCode)
	}
}

func TestPageInHomeServesOnlyItself(t *testing.T) {
	f := startPages(t)
	href := f.href(t, f.top)
	if resp, _ := f.page(t, href); resp.StatusCode != 200 {
		t.Fatalf("top.html: %d", resp.StatusCode)
	}
	id := pageHref.FindString(href)
	for _, p := range []string{"outside.txt", "acme/report/report.html"} {
		if resp, _ := f.page(t, id+p); resp.StatusCode != http.StatusNotFound {
			t.Errorf("%s under top.html's id: %d, want 404", p, resp.StatusCode)
		}
	}
	// Another page in home gets its own id.
	if pageHref.FindString(f.href(t, filepath.Join(f.home, "second.html"))) == id {
		t.Errorf("two pages in home share an id")
	}
}

func TestPageGoneAfterDelete(t *testing.T) {
	f := startPages(t)
	href := f.href(t, f.report)
	if err := os.Remove(f.report); err != nil {
		t.Fatal(err)
	}
	if resp, _ := f.page(t, href); resp.StatusCode != http.StatusNotFound {
		t.Errorf("a deleted page: %d, want 404", resp.StatusCode)
	}
}

func TestPageInKeyFolderServesOnlyItself(t *testing.T) {
	f := startPages(t)
	href := f.href(t, filepath.Join(f.home, ".ssh", "keys.html"))
	if resp, _ := f.page(t, href); resp.StatusCode != 200 {
		t.Fatalf("keys.html: %d", resp.StatusCode)
	}
	if resp, _ := f.page(t, pageHref.FindString(href)+"config"); resp.StatusCode != http.StatusNotFound {
		t.Errorf("a file beside it: %d, want 404", resp.StatusCode)
	}
}

func TestLone(t *testing.T) {
	home, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", home)
	for dir, want := range map[string]bool{
		"/": true, filepath.Dir(home): true, home: true,
		filepath.Join(home, ".aws"): true, filepath.Join(home, ".kube", "x"): true,
		filepath.Join(home, "acme"): false, filepath.Join(home, "acme", "report"): false,
	} {
		if got := lone(dir); got != want {
			t.Errorf("lone(%s) = %v, want %v", dir, got, want)
		}
	}
}

func TestPageRefusesAFolderMadeALink(t *testing.T) {
	f := startPages(t)
	href := f.href(t, f.report)
	dir := filepath.Dir(f.report)
	moved := dir + "-moved"
	if err := os.Rename(dir, moved); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(moved, dir); err != nil {
		t.Fatal(err)
	}
	if resp, _ := f.page(t, href); resp.StatusCode != http.StatusNotFound {
		t.Errorf("a folder that is now a link: %d, want 404", resp.StatusCode)
	}
}

func TestPageRefusesAFIFO(t *testing.T) {
	f := startPages(t)
	dir := strings.TrimSuffix(f.href(t, f.report), "report.html")
	if err := syscall.Mkfifo(filepath.Join(filepath.Dir(f.report), "pipe.txt"), 0o600); err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	if resp, _ := f.page(t, dir+"pipe.txt"); resp.StatusCode != http.StatusNotFound || time.Since(start) > 3*time.Second {
		t.Errorf("a FIFO: %d after %v, want a prompt 404", resp.StatusCode, time.Since(start))
	}
}

func TestOpenRefusesWhatThePageRouteRefuses(t *testing.T) {
	f := startPages(t)
	hidden := filepath.Join(filepath.Dir(f.report), ".hidden.html")
	if code, msg := f.ts.openLink(t, "w1:t1", task.FileURL(hidden)); code != http.StatusBadRequest || msg == "" {
		t.Errorf("a hidden page: %d %q, want 400 with a message", code, msg)
	}
	if dirs := len(f.ts.pages.dirs); dirs != 0 {
		t.Errorf("a refused page was given an address")
	}
}

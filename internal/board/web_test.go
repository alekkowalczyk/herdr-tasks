package board

import (
	"io"
	"io/fs"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// get fetches path from the test server, without a key.
func get(t *testing.T, ts *testServer, path string) (*http.Response, string) {
	t.Helper()
	resp, err := plain.Get(ts.url(path))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	return resp, string(b)
}

// pageScripts are the page's own scripts, by name: every embedded .js file
// outside vendor/.
func pageScripts(t *testing.T) map[string]string {
	t.Helper()
	scripts := map[string]string{}
	err := fs.WalkDir(web, "web", func(name string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() && name == "web/vendor" {
			return fs.SkipDir
		}
		if !d.IsDir() && strings.HasSuffix(name, ".js") {
			b, err := fs.ReadFile(web, name)
			scripts[name] = string(b)
			return err
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"web/app.js", "web/term.js", "web/keymap.js"} {
		if _, ok := scripts[name]; !ok {
			t.Errorf("%s isn't embedded", name)
		}
	}
	return scripts
}

// withoutComments drops a script's comments and keeps its strings. It
// doesn't know regular expression literals, so the page's scripts keep
// quotes and double slashes out of theirs.
func withoutComments(src string) string {
	var b strings.Builder
	for i := 0; i < len(src); i++ {
		c := src[i]
		switch {
		case c == '/' && strings.HasPrefix(src[i:], "//"):
			for i < len(src) && src[i] != '\n' {
				i++
			}
			b.WriteByte('\n')
		case c == '/' && strings.HasPrefix(src[i:], "/*"):
			end := strings.Index(src[i+2:], "*/")
			if end < 0 {
				return b.String()
			}
			i += end + 3
		case c == '\'' || c == '"' || c == '`':
			j := i + 1
			for j < len(src) && src[j] != c {
				if src[j] == '\\' {
					j++
				}
				j++
			}
			b.WriteString(src[i:min(j+1, len(src))])
			i = j
		default:
			b.WriteByte(c)
		}
	}
	return b.String()
}

func TestIndexServesAssets(t *testing.T) {
	ts := startServer(t, Config{Sessions: running()}, nil)
	resp, page := get(t, ts, "/")
	if resp.StatusCode != http.StatusOK || !strings.HasPrefix(resp.Header.Get("Content-Type"), "text/html") {
		t.Fatalf("GET /: %s %q", resp.Status, resp.Header.Get("Content-Type"))
	}
	for path, want := range map[string]string{
		"/-/static/app.js":    "text/javascript",
		"/-/static/board.css": "text/css",
		"/-/static/icon.svg":  "image/svg+xml",
	} {
		if !strings.Contains(page, `"`+path+`"`) {
			t.Errorf("the page doesn't load %s", path)
		}
		resp, body := get(t, ts, path)
		if resp.StatusCode != http.StatusOK || body == "" {
			t.Errorf("GET %s: %s, %d bytes", path, resp.Status, len(body))
		}
		if got := resp.Header.Get("Content-Type"); !strings.HasPrefix(got, want) {
			t.Errorf("GET %s: Content-Type %q, want %s", path, got, want)
		}
	}
}

// The terminals' scripts and styles are the page's own copies of xterm.js,
// served like the rest of the page, with its license.
func TestVendorFilesAreServed(t *testing.T) {
	ts := startServer(t, Config{Sessions: running()}, nil)
	_, page := get(t, ts, "/")
	for path, want := range map[string]string{
		"/-/static/vendor/xterm.js":           "text/javascript",
		"/-/static/vendor/addon-unicode11.js": "text/javascript",
		"/-/static/vendor/xterm.css":          "text/css",
		"/-/static/keymap.js":                 "text/javascript",
		"/-/static/term.js":                   "text/javascript",
	} {
		if !strings.Contains(page, `"`+path+`"`) {
			t.Errorf("the page doesn't load %s", path)
		}
		resp, body := get(t, ts, path)
		if resp.StatusCode != http.StatusOK || body == "" {
			t.Errorf("GET %s: %s, %d bytes", path, resp.Status, len(body))
		}
		if got := resp.Header.Get("Content-Type"); !strings.HasPrefix(got, want) {
			t.Errorf("GET %s: Content-Type %q, want %s", path, got, want)
		}
	}
	resp, body := get(t, ts, "/-/static/vendor/LICENSE-xterm")
	if resp.StatusCode != http.StatusOK || !strings.Contains(body, "Permission is hereby granted") {
		t.Errorf("GET /static/vendor/LICENSE-xterm: %s, %d bytes", resp.Status, len(body))
	}
}

// The terminals' key map runs in node, when node is installed.
func TestKeymap(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node isn't installed")
	}
	out, err := exec.Command(node, filepath.Join("web", "keymap_test.mjs")).CombinedOutput()
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
}

// The terminal view types into panes only what was typed, in order, and
// leaves copy, paste and Cmd shortcuts to the browser. It runs the view's
// scripts with node, against a stand-in xterm, when node is installed.
func TestTerminalView(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node isn't installed")
	}
	out, err := exec.Command(node, filepath.Join("testdata", "term.js"), filepath.Join("web", "keymap.js"), filepath.Join("web", "term.js")).CombinedOutput()
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
}

// The board page shows a card with its changes still on their way, and the
// last one made wins. A 401 for a key it has replaced since signs it out
// no more. It runs the page's script with node, against a stand-in DOM and
// server, when node is installed.
func TestBoardPage(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node isn't installed")
	}
	out, err := exec.Command(node, filepath.Join("testdata", "board.js"), filepath.Join("web", "app.js")).CombinedOutput()
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
}

// The CSP runs only the page's own script files, so the page must not
// need an inline script or event handler.
func TestPageHasNoInlineScript(t *testing.T) {
	b, err := fs.ReadFile(web, "web/index.html")
	if err != nil {
		t.Fatal(err)
	}
	page := string(b)
	tags := regexp.MustCompile(`(?is)<script\b[^>]*>`).FindAllString(page, -1)
	if len(tags) == 0 {
		t.Error("the page loads no script")
	}
	for _, tag := range tags {
		if !strings.Contains(tag, "src=") {
			t.Errorf("inline script: %s", tag)
		}
	}
	if m := regexp.MustCompile(`(?i)\son[a-z]+\s*=`).FindString(page); m != "" {
		t.Errorf("inline event handler: %q", m)
	}
}

func TestWebAssetsAvoidHTMLInjection(t *testing.T) {
	for name, src := range pageScripts(t) {
		for _, sink := range []string{"innerHTML", "outerHTML", "insertAdjacentHTML", "document.write", "createContextualFragment"} {
			if strings.Contains(src, sink) {
				t.Errorf("%s uses %s", name, sink)
			}
		}
	}
}

func TestPageHasNoExternalURLs(t *testing.T) {
	external := regexp.MustCompile(`(?i)https?://`)
	if got := withoutComments("a // http://x\nb /* https://y */ 'http://z' \"it's\" `https://w`"); got != "a \nb  'http://z' \"it's\" `https://w`" {
		t.Fatalf("withoutComments = %q", got)
	}
	for _, name := range []string{"web/index.html", "web/board.css"} {
		b, err := fs.ReadFile(web, name)
		if err != nil {
			t.Fatal(err)
		}
		if m := external.Find(b); m != nil {
			t.Errorf("%s has an external URL (%s)", name, m)
		}
	}
	for name, src := range pageScripts(t) {
		if m := external.FindString(withoutComments(src)); m != "" {
			t.Errorf("%s has an external URL (%s) outside a comment", name, m)
		}
	}
}

// A tab follows a sign-in in another tab, such as the one that
// `board --new-token` opens, even when it holds its key in storage only.
// It runs the page's script with node, when node is installed.
func TestPageFollowsASignInInAnotherTab(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node isn't installed")
	}
	app := filepath.Join(t.TempDir(), "app.js")
	b, err := fs.ReadFile(web, "web/app.js")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(app, b, 0o600); err != nil {
		t.Fatal(err)
	}
	out, err := exec.Command(node, filepath.Join("testdata", "signin.js"), app).CombinedOutput()
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
}

// jsFunction is the source of the function name in src, from "function
// name(" to its closing brace. It skips braces in strings.
func jsFunction(t *testing.T, src, name string) string {
	t.Helper()
	start := strings.Index(src, "function "+name+"(")
	if start < 0 {
		t.Fatalf("no function %s", name)
	}
	depth := 0
	for i := start; i < len(src); i++ {
		switch c := src[i]; c {
		case '\'', '"', '`':
			for i++; i < len(src) && src[i] != c; i++ {
				if src[i] == '\\' {
					i++
				}
			}
		case '{':
			depth++
		case '}':
			if depth--; depth == 0 {
				return src[start : i+1]
			}
		}
	}
	t.Fatalf("function %s doesn't end", name)
	return ""
}

// The Slack line's Fetch button, and the skipped list with Take on each
// ask, are built of elements and text, as Slack text is untrusted. They post
// to the board's Slack endpoints.
func TestPageHasFetchAndSkipped(t *testing.T) {
	src := withoutComments(pageScripts(t)["web/app.js"])
	if el := jsFunction(t, src, "el"); !strings.Contains(el, "document.createElement(") || !strings.Contains(el, ".textContent =") {
		t.Errorf("el doesn't build with createElement and textContent:\n%s", el)
	}
	for name, wants := range map[string][]string{
		"buildSlack":   {"el('button'", "'Fetch'"},
		"drawFetch":    {"'Fetching…'", ".disabled =", ".textContent ="},
		"fetchSlack":   {"'/api/slack/fetch'"},
		"renderSlack":  {"' skipped'", ".textContent ="},
		"skippedItems": {"el('div', 'ask-label', a.label)", "el('div', 'ask-why', a.why)", "'Take'", "'Open in Slack'", "linkable(a.link)", "'noopener noreferrer'"},
		"takeAsk":      {"'/api/slack/take'"},
	} {
		body := jsFunction(t, src, name)
		for _, want := range wants {
			if !strings.Contains(body, want) {
				t.Errorf("%s has no %s", name, want)
			}
		}
		for _, sink := range []string{"innerHTML", "outerHTML", "insertAdjacentHTML", "document.write", "createContextualFragment", "setHTML"} {
			if strings.Contains(body, sink) {
				t.Errorf("%s uses %s", name, sink)
			}
		}
	}
}

// Each session has its own board page, so the page has no switcher between
// sessions and keeps no choice of one.
func TestPageHasNoSessionSwitcher(t *testing.T) {
	b, err := fs.ReadFile(web, "web/index.html")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(b), `id="sessions"`) {
		t.Error(`index.html has id="sessions"`)
	}
	if strings.Contains(pageScripts(t)["web/app.js"], "herdr-tasks-board-session") {
		t.Error("app.js keeps a chosen session")
	}
}

// The page asks for its own session's board and events.
func TestPageScopesRequestsBySession(t *testing.T) {
	src := withoutComments(pageScripts(t)["web/app.js"])
	for _, want := range []string{"/api/board?session=", "/api/events?session="} {
		if !strings.Contains(src, want) {
			t.Errorf("app.js doesn't ask for %s", want)
		}
	}
}

// Signing in takes the code out of the address and stays on the page's path,
// with its hash.
func TestSignInKeepsThePath(t *testing.T) {
	body := jsFunction(t, withoutComments(pageScripts(t)["web/app.js"]), "signIn")
	if !strings.Contains(body, "history.replaceState(null, '', location.pathname + location.hash)") {
		t.Errorf("signIn doesn't replace the address with the path and hash:\n%s", body)
	}
}

// The list of sessions at / links to each board, with the session name
// encoded.
func TestPageHasSessionList(t *testing.T) {
	src := withoutComments(pageScripts(t)["web/app.js"])
	if !strings.Contains(src, "'sessions'") {
		t.Error("app.js doesn't handle the sessions event")
	}
	if body := jsFunction(t, src, "pageOf"); !strings.Contains(body, "encodeURIComponent(") {
		t.Errorf("pageOf doesn't encode the session name:\n%s", body)
	}
}

// A session's page says why Slack asks aren't on it, when the board says.
func TestPageShowsSlackWaiting(t *testing.T) {
	if !strings.Contains(pageScripts(t)["web/app.js"], "slackWaiting") {
		t.Error("app.js doesn't read slackWaiting")
	}
}

// The header shows where the board is on the tailnet, or why it isn't
// there.
func TestPageShowsTailnet(t *testing.T) {
	src := withoutComments(pageScripts(t)["web/app.js"])
	body := jsFunction(t, src, "renderTailnet")
	for _, want := range []string{"line.problem", "line.address"} {
		if !strings.Contains(body, want) {
			t.Errorf("renderTailnet doesn't read %s:\n%s", want, body)
		}
	}
	for _, want := range []string{"renderTailnet(board.tailnet)", "renderTailnet(l.tailnet)"} {
		if !strings.Contains(src, want) {
			t.Errorf("app.js doesn't call %s", want)
		}
	}
}

// On a board seen through the tailnet, a localhost link asks in the page
// before it's shared, never with a browser dialog, and the header lists
// the shares with Stop.
func TestPageHasShareConfirmation(t *testing.T) {
	src := withoutComments(pageScripts(t)["web/app.js"])
	for _, want := range []string{
		"'Share localhost:' + port + ' with your tailnet?'",
		"'Anyone in your tailnet can open it while it’s shared.'",
		"'/api/share'",
		"'/api/unshare'",
		"'Stop'",
	} {
		if !strings.Contains(src, want) {
			t.Errorf("app.js doesn't have %s", want)
		}
	}
	if regexp.MustCompile(`\bconfirm\(`).MatchString(src) {
		t.Error("app.js asks with confirm(")
	}
}

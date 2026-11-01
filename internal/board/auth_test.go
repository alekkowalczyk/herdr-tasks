package board

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"
)

// authPort is the port the requests below claim to reach. Nothing listens on it.
const authPort = 41234

var testToken = strings.Repeat("ab", 32)

// guarded is a handler that answers "ok" behind Wrap, with /api/shutdown
// for the admin, and the browser key it takes.
func guarded() (http.Handler, string) {
	ok := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, "ok") })
	a := NewAuth(testToken)
	return a.Wrap(ok, func(p string) bool { return p == "/api/shutdown" }), a.key
}

// send runs one request through h. headers may set Host.
func send(h http.Handler, method, target string, headers map[string]string, body string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, target, strings.NewReader(body))
	r.Host = fmt.Sprintf("127.0.0.1:%d", authPort)
	for k, v := range headers {
		if k == "Host" {
			r.Host = v
			continue
		}
		r.Header.Set(k, v)
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}

func bearer(secret string) map[string]string {
	return map[string]string{"Authorization": "Bearer " + secret}
}

func with(h map[string]string, k, v string) map[string]string {
	out := map[string]string{k: v}
	for k, v := range h {
		out[k] = v
	}
	return out
}

func TestRejectsForeignHost(t *testing.T) {
	h, _ := guarded()
	for _, host := range []string{
		"evil.example",
		fmt.Sprintf("evil.example:%d", authPort),
		fmt.Sprintf("localhost.evil.example:%d", authPort),
		"127.0.0.1",
		"127.0.0.1:",
		"127.0.0.1:123456",
		"127.0.0.1:80.evil.example",
		fmt.Sprintf("[::1]:%d", authPort),
		"",
	} {
		for _, target := range []string{"/", "/-/static/app.js", "/api/board"} {
			if w := send(h, "GET", target, with(bearer(testToken), "Host", host), ""); w.Code != http.StatusForbidden {
				t.Errorf("Host %q, %s: got %d, want 403", host, target, w.Code)
			}
		}
	}
	for _, host := range []string{fmt.Sprintf("127.0.0.1:%d", authPort), fmt.Sprintf("localhost:%d", authPort)} {
		if w := send(h, "GET", "/api/board", with(bearer(testToken), "Host", host), ""); w.Code != http.StatusOK {
			t.Errorf("Host %q: got %d, want 200", host, w.Code)
		}
	}
}

func TestAPIWithoutKeyIs401(t *testing.T) {
	h, _ := guarded()
	for _, auth := range []string{
		"",
		"Bearer",
		"Bearer ",
		"Bearer nope",
		"Basic " + testToken,
		testToken,
		"Bearer " + testToken + "x",
		"Bearer " + NewAuth("another token").key,
		"Bearer " + NewAuth(testToken).key, // another run's
	} {
		for _, target := range []string{"/api/board", "/api", "//api/board", "/-/static/../../api/board"} {
			w := send(h, "GET", target, map[string]string{"Authorization": auth}, "")
			if w.Code != http.StatusUnauthorized {
				t.Errorf("Authorization %q, %s: got %d, want 401", auth, target, w.Code)
				continue
			}
			var body struct{ Error string }
			if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
				t.Fatalf("401 body %q: %v", w.Body, err)
			}
			if want := "Sign in again: run herdr-tasks board, or use Open task board in herdr."; body.Error != want {
				t.Errorf("401 error = %q, want %q", body.Error, want)
			}
			if ct := w.Header().Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
				t.Errorf("401 Content-Type = %q", ct)
			}
		}
	}
}

func TestBrowserKeyWorksButNotForAdminRoutes(t *testing.T) {
	h, key := guarded()
	cases := []struct {
		method, target, secret string
		want                   int
	}{
		{"GET", "/api/board", key, 200},
		{"POST", "/api/task", key, 200},
		{"POST", "/api/shutdown", key, 401},
		{"POST", "/-/static/../../api/shutdown", key, 401},
		{"POST", "/api//shutdown", key, 401},
		{"GET", "/api/board", testToken, 200},
		{"POST", "/api/shutdown", testToken, 200},
	}
	for _, c := range cases {
		if w := send(h, c.method, c.target, bearer(c.secret), ""); w.Code != c.want {
			t.Errorf("%s %s with the %s: got %d, want %d", c.method, c.target, map[bool]string{true: "browser key", false: "token"}[c.secret == key], w.Code, c.want)
		}
	}
	if w := send(h, "GET", "/api/board", map[string]string{"Authorization": "bearer " + key}, ""); w.Code != 200 {
		t.Errorf("lower-case bearer: got %d, want 200", w.Code)
	}
}

func TestForeignOriginIs403(t *testing.T) {
	h, _ := guarded()
	for _, origin := range []string{
		"http://evil.example",
		"http://127.0.0.1:1",
		fmt.Sprintf("https://127.0.0.1:%d", authPort),
		fmt.Sprintf("http://127.0.0.1:%d.evil.example", authPort),
		"null",
	} {
		for _, target := range []string{"/api/board", "/api/session"} {
			if w := send(h, "POST", target, with(bearer(testToken), "Origin", origin), ""); w.Code != http.StatusForbidden {
				t.Errorf("Origin %q, %s: got %d, want 403", origin, target, w.Code)
			}
		}
	}
	for _, origin := range []string{fmt.Sprintf("http://127.0.0.1:%d", authPort), ""} {
		hs := with(bearer(testToken), "Origin", origin)
		if origin == "" {
			hs = bearer(testToken)
		}
		if w := send(h, "POST", "/api/board", hs, ""); w.Code != http.StatusOK {
			t.Errorf("Origin %q: got %d, want 200", origin, w.Code)
		}
	}
	// The Origin has to be the Host's own.
	hs := with(with(bearer(testToken), "Host", fmt.Sprintf("localhost:%d", authPort)), "Origin", fmt.Sprintf("http://127.0.0.1:%d", authPort))
	if w := send(h, "POST", "/api/board", hs, ""); w.Code != http.StatusForbidden {
		t.Errorf("Origin of the other name: got %d, want 403", w.Code)
	}
}

func TestCrossSiteFetchIs403(t *testing.T) {
	h, _ := guarded()
	for site, want := range map[string]int{"cross-site": 403, "same-site": 403, "same-origin": 200, "none": 200} {
		if w := send(h, "GET", "/api/board", with(bearer(testToken), "Sec-Fetch-Site", site), ""); w.Code != want {
			t.Errorf("Sec-Fetch-Site %s: got %d, want %d", site, w.Code, want)
		}
	}
}

func TestStaticNeedsNoKeyAndHasCSP(t *testing.T) {
	h, _ := guarded()
	want := map[string]string{
		"Content-Security-Policy": "default-src 'self'; script-src 'self'; style-src 'self' 'unsafe-inline'; img-src 'self' data:; connect-src 'self'; frame-ancestors 'none'; base-uri 'none'; form-action 'none'",
		"X-Frame-Options":         "DENY",
		"Referrer-Policy":         "no-referrer",
		"Cache-Control":           "no-store",
	}
	check := func(what string, w *httptest.ResponseRecorder) {
		for k, v := range want {
			if got := w.Header().Get(k); got != v {
				t.Errorf("%s: %s = %q, want %q", what, k, got, v)
			}
		}
	}
	for _, target := range []string{"/", "/-/static/app.js", "/index.html"} {
		w := send(h, "GET", target, nil, "")
		if w.Code != http.StatusOK || w.Body.String() != "ok" {
			t.Errorf("GET %s without a key: %d %q, want 200 ok", target, w.Code, w.Body)
		}
		check("GET "+target, w)
	}
	check("401", send(h, "GET", "/api/board", nil, ""))
	check("403", send(h, "GET", "/", map[string]string{"Host": "evil.example"}, ""))
}

func TestLoginCodeIsSingleUseAndExpires(t *testing.T) {
	now := time.Date(2026, 10, 31, 9, 0, 0, 0, time.UTC)
	a := NewAuth(testToken)
	code := a.LoginCode(now)
	if !regexp.MustCompile(`^[0-9a-f]{32,}$`).MatchString(code) {
		t.Fatalf("code %q isn't long random hex", code)
	}
	if other := a.LoginCode(now); other == code {
		t.Fatalf("two codes are the same: %q", code)
	}
	key, ok := a.Redeem(code, now.Add(59*time.Second))
	if !ok || key != a.key {
		t.Fatalf("Redeem within the minute = %q, %v; want the browser key", key, ok)
	}
	if _, ok := a.Redeem(code, now.Add(59*time.Second)); ok {
		t.Error("a code worked twice")
	}
	late := a.LoginCode(now)
	if _, ok := a.Redeem(late, now.Add(60*time.Second)); ok {
		t.Error("a code worked after a minute")
	}
	if _, ok := a.Redeem(late, now); ok {
		t.Error("an expired code came back")
	}
	for _, bad := range []string{"", "nope", strings.Repeat("0", 32)} {
		if _, ok := a.Redeem(bad, now); ok {
			t.Errorf("Redeem(%q) worked", bad)
		}
	}

	// The same through the server: the CLI asks for a code with the admin
	// token, and the page swaps it for the key without any credential.
	a = NewAuth(testToken)
	s := NewServer(Config{StateDir: t.TempDir(), Port: authPort, Version: "test"}, a, nil)
	if w := send(s.handler, "POST", "/api/login-code", bearer(a.key), ""); w.Code != http.StatusUnauthorized {
		t.Errorf("login code with the browser key: got %d, want 401", w.Code)
	}
	w := send(s.handler, "POST", "/api/login-code", bearer(testToken), "")
	var got struct{ Code string }
	if w.Code != http.StatusOK || json.Unmarshal(w.Body.Bytes(), &got) != nil || got.Code == "" {
		t.Fatalf("login code: %d %q", w.Code, w.Body)
	}
	body := fmt.Sprintf(`{"code":%q}`, got.Code)
	w = send(s.handler, "POST", "/api/session", map[string]string{"Content-Type": "application/json"}, body)
	var session struct{ Key string }
	if w.Code != http.StatusOK || json.Unmarshal(w.Body.Bytes(), &session) != nil || session.Key != a.key {
		t.Fatalf("session: %d %q, want the browser key", w.Code, w.Body)
	}
	if w := send(s.handler, "POST", "/api/session", nil, body); w.Code != http.StatusUnauthorized {
		t.Errorf("second session with one code: got %d, want 401", w.Code)
	}
	if w := send(s.handler, "POST", "/api/session", nil, "not json"); w.Code != http.StatusBadRequest {
		t.Errorf("session with a bad body: got %d, want 400", w.Code)
	}
}

// A page left open keeps sending its key to the port, also to a program
// that takes the port after the server exits. The key belongs to one run of
// the server, so the next one turns it away, though the token is the same.
func TestBrowserKeyIsNewForEachRun(t *testing.T) {
	first, next := NewAuth(testToken), NewAuth(testToken)
	if !regexp.MustCompile(`^[0-9a-f]{64}$`).MatchString(first.key) || first.key == testToken {
		t.Fatalf("browser key %q isn't 32 bytes of hex apart from the token", first.key)
	}
	if first.key == next.key {
		t.Fatal("two runs give one key")
	}
	ok := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {})
	h := next.Wrap(ok, nil)
	if w := send(h, "GET", "/api/board", bearer(first.key), ""); w.Code != http.StatusUnauthorized {
		t.Errorf("the last run's key: got %d, want 401", w.Code)
	}
	if w := send(h, "GET", "/api/board", bearer(next.key), ""); w.Code != http.StatusOK {
		t.Errorf("this run's key: got %d, want 200", w.Code)
	}
	if browserKey("a", "run") == browserKey("b", "run") {
		t.Error("two tokens give one key")
	}
}

func TestTokenFileIs0600(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "state") // made on first use
	path := filepath.Join(dir, "board-token")
	checkMode := func(when string) {
		t.Helper()
		fi, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		if perm := fi.Mode().Perm(); perm != 0o600 {
			t.Errorf("%s: board-token mode %o, want 600", when, perm)
		}
	}
	tok, err := EnsureToken(dir)
	if err != nil {
		t.Fatal(err)
	}
	if !regexp.MustCompile(`^[0-9a-f]{64}$`).MatchString(tok) {
		t.Fatalf("token %q isn't 32 bytes of hex", tok)
	}
	checkMode("created")
	if again, err := EnsureToken(dir); err != nil || again != tok {
		t.Fatalf("second EnsureToken = %q, %v; want the same token", again, err)
	}

	fresh, err := NewToken(dir)
	if err != nil {
		t.Fatal(err)
	}
	if fresh == tok {
		t.Fatal("NewToken kept the old token")
	}
	checkMode("replaced")
	if got, err := EnsureToken(dir); err != nil || got != fresh {
		t.Fatalf("EnsureToken after NewToken = %q, %v; want the new token", got, err)
	}
	if left, _ := filepath.Glob(filepath.Join(dir, ".board-token*")); len(left) > 0 {
		t.Errorf("temp files left: %v", left)
	}
}

func TestEnsureTokenAgreesWhenRacing(t *testing.T) {
	dir := t.TempDir()
	toks := make([]string, 8)
	var wg sync.WaitGroup
	for i := range toks {
		wg.Add(1)
		go func() {
			defer wg.Done()
			tok, err := EnsureToken(dir)
			if err != nil {
				t.Error(err)
			}
			toks[i] = tok
		}()
	}
	wg.Wait()
	for _, tok := range toks {
		if tok != toks[0] {
			t.Fatalf("racing first runs got different tokens: %q", toks)
		}
	}
}

func TestDamagedTokenIsAnError(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "board-token"), []byte("half"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := EnsureToken(dir); err == nil || !strings.Contains(err.Error(), "--new-token") {
		t.Errorf("EnsureToken with a damaged file = %v, want an error naming --new-token", err)
	}
}

func TestLoopbackAcceptsAnyPort(t *testing.T) {
	h, key := guarded()
	for _, c := range []struct {
		host, origin string
		want         int
	}{
		{"127.0.0.1:8000", "http://127.0.0.1:8000", 200},
		{"localhost:9", "", 200},
		{"evil.test:7391", "", 403},
		{"127.0.0.1:8000", "http://127.0.0.1:7391", 403},
		{"127.0.0.1", "", 403},
	} {
		hs := with(bearer(key), "Host", c.host)
		if c.origin != "" {
			hs["Origin"] = c.origin
		}
		if w := send(h, "GET", "/api/board", hs, ""); w.Code != c.want {
			t.Errorf("Host %q, Origin %q: got %d, want %d", c.host, c.origin, w.Code, c.want)
		}
	}
}

// tailnet is a handler that answers "ok" behind WrapTailnet, as tailscale
// serve reaches it: with Host localhost.
func tailnet(origin string) (http.Handler, *Auth) {
	ok := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, "ok") })
	a := NewAuth(testToken)
	return a.WrapTailnet(ok, func(p string) bool { return p == "/api/login-code" }, func() string { return origin }), a
}

const tailnetOrigin = "https://devbox.tail0000.ts.net:7391"

func TestTailnetDoorChecksOrigin(t *testing.T) {
	h, a := tailnet(tailnetOrigin)
	key := func() map[string]string { return with(bearer(a.BrowserKey()), "Host", "localhost") }
	if w := send(h, "GET", "/api/board", with(key(), "Origin", tailnetOrigin), ""); w.Code != http.StatusOK {
		t.Errorf("the tailnet Origin: got %d, want 200", w.Code)
	}
	if w := send(h, "GET", "/api/board", with(key(), "Origin", "https://evil.test"), ""); w.Code != http.StatusForbidden {
		t.Errorf("another Origin: got %d, want 403", w.Code)
	}
	if w := send(h, "GET", "/api/board", with(key(), "Sec-Fetch-Site", "cross-site"), ""); w.Code != http.StatusForbidden {
		t.Errorf("cross-site: got %d, want 403", w.Code)
	}
	if w := send(h, "GET", "/api/board", map[string]string{"Host": "localhost"}, ""); w.Code != http.StatusUnauthorized {
		t.Errorf("no key: got %d, want 401", w.Code)
	}
	// Pages need no key, as on the loopback door.
	if w := send(h, "GET", "/-/static/app.js", map[string]string{"Host": "localhost"}, ""); w.Code != http.StatusOK {
		t.Errorf("a page: got %d, want 200", w.Code)
	}
	if w := send(h, "GET", "/-/static/app.js", map[string]string{"Host": "localhost"}, ""); w.Header().Get("X-Frame-Options") != "DENY" {
		t.Error("the tailnet door sets no X-Frame-Options")
	}
	none, a := tailnet("")
	for _, origin := range []string{tailnetOrigin, ""} {
		hs := with(bearer(a.BrowserKey()), "Host", "localhost")
		if origin != "" {
			hs["Origin"] = origin
		}
		if w := send(none, "GET", "/api/board", hs, ""); w.Code != http.StatusForbidden {
			t.Errorf("no tailnet address, Origin %q: got %d, want 403", origin, w.Code)
		}
	}
}

func TestTailnetDoorRefusesAdminPaths(t *testing.T) {
	a := NewAuth(testToken)
	s := NewServer(Config{StateDir: t.TempDir(), Version: "test"}, a, nil)
	h := s.TailnetHandler(func() string { return tailnetOrigin })
	for _, p := range []string{"/api/login-code", "/api/shutdown"} {
		w := send(h, "POST", p, with(bearer(testToken), "Host", "localhost"), "")
		if w.Code != http.StatusForbidden || !strings.Contains(w.Body.String(), "Only herdr-tasks on this machine can do that.") {
			t.Errorf("%s with the admin token: %d %q, want 403 and the message", p, w.Code, w.Body)
		}
	}
}

func TestSessionSignsInTailnetOwner(t *testing.T) {
	for _, c := range []struct {
		owner, login string
		want         int
	}{
		{"dana@acme.test", "dana@acme.test", 200},
		{"dana@acme.test", "rui@acme.test", 401},
		{"dana@acme.test", "", 401},
		{"", "dana@acme.test", 401}, // a tagged machine
		{"", "", 401},
	} {
		a := NewAuth(testToken)
		s := NewServer(Config{StateDir: t.TempDir(), Version: "test", TailnetOwner: func() string { return c.owner }}, a, nil)
		h := s.TailnetHandler(func() string { return tailnetOrigin })
		hs := map[string]string{"Host": "localhost", "Origin": tailnetOrigin}
		if c.login != "" {
			hs["Tailscale-User-Login"] = c.login
		}
		w := send(h, "POST", "/api/session", hs, "{}")
		if w.Code != c.want {
			t.Errorf("owner %q, login %q: got %d, want %d", c.owner, c.login, w.Code, c.want)
		}
		if c.want == 200 && !strings.Contains(w.Body.String(), a.BrowserKey()) {
			t.Errorf("owner sign-in: %q, want the browser key", w.Body)
		}
	}
}

// The code alone decides when there is one, and no body is no code.
func TestSessionCodeAndEmptyBodyOnTailnetDoor(t *testing.T) {
	a := NewAuth(testToken)
	s := NewServer(Config{StateDir: t.TempDir(), Version: "test", TailnetOwner: func() string { return "dana@acme.test" }}, a, nil)
	h := s.TailnetHandler(func() string { return tailnetOrigin })
	hs := map[string]string{"Host": "localhost", "Origin": tailnetOrigin, "Tailscale-User-Login": "dana@acme.test"}
	w := send(h, "POST", "/api/session", hs, "")
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), a.BrowserKey()) {
		t.Errorf("owner with no body: %d %q, want 200 and the key", w.Code, w.Body)
	}
	if w := send(h, "POST", "/api/session", hs, `{"code":"nope"}`); w.Code != http.StatusUnauthorized {
		t.Errorf("owner with a wrong code: got %d, want 401", w.Code)
	}
}

func TestLoopbackIgnoresTailscaleHeaders(t *testing.T) {
	a := NewAuth(testToken)
	s := NewServer(Config{StateDir: t.TempDir(), Version: "test", TailnetOwner: func() string { return "dana@acme.test" }}, a, nil)
	hs := map[string]string{"Tailscale-User-Login": "dana@acme.test"}
	if w := send(s.handler, "POST", "/api/session", hs, "{}"); w.Code != http.StatusUnauthorized {
		t.Errorf("owner header on the loopback door: got %d, want 401", w.Code)
	}
}

package board

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"
)

const (
	tokenFile = "board-token"
	// How long a login code works.
	codeLife = time.Minute
	// signInAgain is the error for a missing or wrong key.
	signInAgain = "Sign in again: run herdr-tasks board, or use Open task board in herdr."
	// csp lets the page load only its own files, and no other page frame it.
	csp = "default-src 'self'; script-src 'self'; style-src 'self' 'unsafe-inline'; img-src 'self' data:; connect-src 'self'; frame-ancestors 'none'; base-uri 'none'; form-action 'none'"
)

var tokenFormat = regexp.MustCompile(`^[0-9a-f]{64}$`)

// EnsureToken returns the admin token from <stateDir>/board-token, creating
// it the first time.
func EnsureToken(stateDir string) (string, error) {
	path := filepath.Join(stateDir, tokenFile)
	tok, err := readToken(path)
	if !errors.Is(err, fs.ErrNotExist) {
		return tok, err
	}
	if tok, err = secret(32); err != nil {
		return "", err
	}
	// The new token is linked into place, which fails if there's one already,
	// so first runs at the same time agree on one token and none of them
	// reads it half written.
	err = writePrivate(path, tok+"\n", true)
	if errors.Is(err, fs.ErrExist) {
		return readToken(path)
	}
	if err != nil {
		return "", err
	}
	return tok, nil
}

// NewToken replaces the admin token, and with it the browser key. The server
// only knows the token it started with, so hold board.lock, with no server
// running, while you replace it: Rotate does.
func NewToken(stateDir string) (string, error) {
	tok, err := secret(32)
	if err != nil {
		return "", err
	}
	if err := writePrivate(filepath.Join(stateDir, tokenFile), tok+"\n", false); err != nil {
		return "", err
	}
	return tok, nil
}

func readToken(path string) (string, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	tok := strings.TrimSpace(string(b))
	if !tokenFormat.MatchString(tok) {
		return "", fmt.Errorf("%s is damaged; run herdr-tasks board --new-token", path)
	}
	return tok, nil
}

// secret is n random bytes as hex.
func secret(n int) (string, error) {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

// writePrivate writes data to a new 0600 file next to path, then moves it to
// path: by renaming over whatever is there, or, with link, by hard-linking,
// which fails with fs.ErrExist when path exists.
func writePrivate(path, data string, link bool) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	f, err := os.CreateTemp(dir, "."+filepath.Base(path)+"-*") // O_EXCL, mode 0600
	if err != nil {
		return err
	}
	_, err = f.WriteString(data)
	if err == nil {
		err = f.Sync()
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		if link {
			err = os.Link(f.Name(), path)
		} else if err = os.Rename(f.Name(), path); err == nil {
			return nil
		}
	}
	os.Remove(f.Name())
	return err
}

// browserKey is the key a signed-in page sends. It's derived from the token
// and run, a secret each run of the server makes as it starts, so a new
// token or a new run ends every browser's sign-in. A page that's left open
// keeps sending its key to the port, also to a program that takes the port
// while no server runs; to the next server, that key is no use.
func browserKey(token, run string) string {
	m := hmac.New(sha256.New, []byte(token))
	m.Write([]byte("herdr-tasks board browser\x00" + run))
	return hex.EncodeToString(m.Sum(nil))
}

// Auth checks the board's requests. The admin token is for the CLI, and the
// browser key for a signed-in page. A page signs in by swapping a login code
// that the CLI asked for. The browser key works only with the Auth that gave
// it out.
type Auth struct {
	token, key string

	mu    sync.Mutex
	codes map[string]time.Time // login code → when it stops working
}

// NewAuth checks requests for the server. It makes a new browser key, so a
// server makes one once, as it starts.
func NewAuth(token string) *Auth {
	run, err := secret(32)
	if err != nil {
		panic(err) // crypto/rand doesn't fail on the systems herdr runs on
	}
	return &Auth{token: token, key: browserKey(token, run), codes: map[string]time.Time{}}
}

// BrowserKey is the key a signed-in page sends.
func (a *Auth) BrowserKey() string { return a.key }

// LoginCode makes a code that can be swapped for the browser key once,
// within a minute.
func (a *Auth) LoginCode(now time.Time) string {
	code, err := secret(16)
	if err != nil {
		panic(err) // crypto/rand doesn't fail on the systems herdr runs on
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	a.expire(now)
	a.codes[code] = now.Add(codeLife)
	return code
}

// Redeem swaps a login code for the browser key.
func (a *Auth) Redeem(code string, now time.Time) (key string, ok bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.expire(now)
	for c := range a.codes {
		if subtle.ConstantTimeCompare([]byte(c), []byte(code)) == 1 {
			delete(a.codes, c)
			return a.key, true
		}
	}
	return "", false
}

func (a *Auth) expire(now time.Time) {
	for c, until := range a.codes {
		if !now.Before(until) {
			delete(a.codes, c)
		}
	}
}

// loopbackHost is the Host of a request that reached the board on this
// machine. The port is any: an SSH tunnel brings it to another one.
var loopbackHost = regexp.MustCompile(`^(127\.0\.0\.1|localhost):[0-9]{1,5}$`)

type loginKey struct{}

// loginOf is the Tailscale login WrapTailnet saw on the request: "" on the
// loopback door, which ignores Tailscale's headers.
func loginOf(ctx context.Context) string {
	login, _ := ctx.Value(loginKey{}).(string)
	return login
}

// Wrap is the loopback door. It applies the board's security rules before h:
//   - Every response gets headers that keep other pages from framing it or
//     loading anything into it.
//   - The Host must be 127.0.0.1 or localhost, with a port, which stops DNS
//     rebinding.
//   - /api/ requests from another site or origin are refused.
//   - /api/ requests need the admin token or the browser key, and the paths
//     adminOnly picks need the admin token. /api/session needs neither: the
//     login code it takes is the credential.
func (a *Auth) Wrap(h http.Handler, adminOnly func(path string) bool) http.Handler {
	return a.guard(h, adminOnly, func(w http.ResponseWriter, r *http.Request) bool {
		if !loopbackHost.MatchString(r.Host) {
			writeError(w, http.StatusForbidden, "The board only answers at 127.0.0.1 or localhost.")
			return false
		}
		return true
	}, func(w http.ResponseWriter, r *http.Request, p string) bool {
		return sameOrigin(w, r, "http://"+r.Host)
	})
}

// WrapTailnet is the door for requests that come through tailscale serve.
// Tailscale sends its own Host, so none is checked; every Origin must be
// origin(), the board's tailnet address, and with none every /api/ request
// is refused. The paths adminOnly picks are for the CLI on this machine, so
// they are refused here. The Tailscale-User-Login header is put in the
// request's context, for loginOf: Tailscale sets it itself, after deleting
// any the client sent, and a tagged machine has none.
func (a *Auth) WrapTailnet(h http.Handler, adminOnly func(path string) bool, origin func() string) http.Handler {
	signed := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ctx := context.WithValue(r.Context(), loginKey{}, r.Header.Get("Tailscale-User-Login"))
		h.ServeHTTP(w, r.WithContext(ctx))
	})
	return a.guard(signed, adminOnly, nil, func(w http.ResponseWriter, r *http.Request, p string) bool {
		if adminOnly != nil && adminOnly(p) {
			writeError(w, http.StatusForbidden, "Only herdr-tasks on this machine can do that.")
			return false
		}
		o := origin()
		if o == "" {
			writeError(w, http.StatusForbidden, "Other sites can't use the board.")
			return false
		}
		return sameOrigin(w, r, o)
	})
}

// sameOrigin refuses the request, and says so, unless every Origin it sends
// is want. No Origin passes.
func sameOrigin(w http.ResponseWriter, r *http.Request, want string) bool {
	for _, o := range r.Header.Values("Origin") {
		if o != want {
			writeError(w, http.StatusForbidden, "Other sites can't use the board.")
			return false
		}
	}
	return true
}

// guard is what both doors share. door, if there is one, judges every
// request, and apiDoor the /api/ ones, before the Sec-Fetch-Site and key
// checks; each says false once it has answered.
func (a *Auth) guard(
	h http.Handler,
	adminOnly func(path string) bool,
	door func(http.ResponseWriter, *http.Request) bool,
	apiDoor func(http.ResponseWriter, *http.Request, string) bool,
) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hd := w.Header()
		hd.Set("Content-Security-Policy", csp)
		hd.Set("X-Frame-Options", "DENY")
		hd.Set("Referrer-Policy", "no-referrer")
		hd.Set("Cache-Control", "no-store")
		hd.Set("X-Content-Type-Options", "nosniff")
		if door != nil && !door(w, r) {
			return
		}
		// Judge the path the router will use, so /-/static/../../api/ is still the API.
		p := path.Clean("/" + r.URL.Path)
		if p != "/api" && !strings.HasPrefix(p, "/api/") {
			h.ServeHTTP(w, r)
			return
		}
		if !apiDoor(w, r, p) {
			return
		}
		if s := r.Header.Get("Sec-Fetch-Site"); s == "cross-site" || s == "same-site" {
			writeError(w, http.StatusForbidden, "Other sites can't use the board.")
			return
		}
		if p != "/api/session" && !a.allows(r.Header.Get("Authorization"), adminOnly != nil && adminOnly(p)) {
			hd.Set("WWW-Authenticate", "Bearer")
			writeError(w, http.StatusUnauthorized, signInAgain)
			return
		}
		h.ServeHTTP(w, r)
	})
}

// allows reports whether an Authorization header holds the admin token, or
// the browser key when the admin token isn't needed.
func (a *Auth) allows(header string, admin bool) bool {
	scheme, got, _ := strings.Cut(header, " ")
	if !strings.EqualFold(scheme, "Bearer") || got == "" {
		return false
	}
	if subtle.ConstantTimeCompare([]byte(got), []byte(a.token)) == 1 {
		return true
	}
	return !admin && subtle.ConstantTimeCompare([]byte(got), []byte(a.key)) == 1
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}

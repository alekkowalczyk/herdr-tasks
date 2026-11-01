package board

import (
	"crypto/rand"
	"encoding/hex"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"strings"
	"sync"

	"github.com/alekkowalczyk/herdr-tasks/internal/slack"
)

// maxPage is the largest file the board serves from a page's folder.
const maxPage = 64 << 20

// pageSandbox lets a page run its scripts, but not act as the board's
// origin: it can't read the board, or call its API.
const pageSandbox = "sandbox allow-scripts allow-forms allow-modals allow-popups allow-popups-to-escape-sandbox allow-downloads"

const pageGone = "This page's address works until the board's server restarts. Open it from the board again."

// pageFileTypes are the types of what a page's folder holds, besides
// pageTypes, so they don't depend on the system's MIME tables either.
var pageFileTypes = map[string]string{
	".htm": pageTypes[".html"], ".json": "application/json", ".txt": "text/plain; charset=utf-8",
	".png": "image/png", ".jpg": "image/jpeg", ".jpeg": "image/jpeg", ".gif": "image/gif", ".webp": "image/webp",
	".ico": "image/x-icon", ".woff": "font/woff", ".woff2": "font/woff2", ".ttf": "font/ttf", ".otf": "font/otf",
	".mp4": "video/mp4", ".webm": "video/webm", ".pdf": "application/pdf", ".wasm": "application/wasm",
}

// pages are the folders of the pages the board serves, by the address id
// each was given. The id is the credential: it's random, and lasts as long as
// the server.
type pages struct {
	mu   sync.Mutex
	ids  map[string]string // folder (or page, for a lone page) to id
	dirs map[string]pageDir
}

// pageDir is what an id serves: a folder, or when only is set, only the page
// in it, as the home folder and / hold far more than the page.
type pageDir struct {
	dir  string
	only string
}

// href gives the address of a page, as a path, whose symlinks are resolved.
// Pages of one folder share an id.
func (p *pages) href(page string) string {
	dir := filepath.Dir(page)
	d := pageDir{dir: dir}
	key := dir
	if home, err := os.UserHomeDir(); dir == string(filepath.Separator) || (err == nil && sameFile(dir, home)) {
		d.only, key = page, page
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	id, ok := p.ids[key]
	if !ok {
		b := make([]byte, 16)
		if _, err := rand.Read(b); err != nil {
			panic(err) // the system has no randomness
		}
		id = hex.EncodeToString(b)
		if p.ids == nil {
			p.ids, p.dirs = map[string]string{}, map[string]pageDir{}
		}
		p.ids[key], p.dirs[id] = id, d
	}
	return "/-/page/" + id + "/" + url.PathEscape(filepath.Base(page))
}

// sameFile reports whether two paths are the same folder, with symlinks
// followed.
func sameFile(a, b string) bool {
	a, errA := filepath.EvalSymlinks(a)
	b, errB := filepath.EvalSymlinks(b)
	return errA == nil && errB == nil && a == b
}

// serve answers GET /-/page/{id}/{path...} with a file of the page's folder.
// Every refusal is the same 404, so it doesn't say what is there.
func (p *pages) serve(w http.ResponseWriter, r *http.Request) {
	p.mu.Lock()
	d, ok := p.dirs[r.PathValue("id")]
	p.mu.Unlock()
	if !ok {
		http.Error(w, pageGone, http.StatusNotFound)
		return
	}
	f, name := d.open(r.PathValue("path"))
	if f == nil {
		http.NotFound(w, r)
		return
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil || !fi.Mode().IsRegular() || fi.Size() > maxPage {
		http.NotFound(w, r)
		return
	}
	h := w.Header()
	h.Set("Content-Security-Policy", pageSandbox)
	ext := strings.ToLower(path.Ext(name))
	t, ok := pageTypes[ext]
	if !ok {
		if t, ok = pageFileTypes[ext]; !ok {
			t = "application/octet-stream"
		}
	}
	h.Set("Content-Type", t)
	switch ext {
	case ".woff", ".woff2", ".ttf", ".otf":
		h.Set("Access-Control-Allow-Origin", "*")
	}
	http.ServeContent(w, r, name, fi.ModTime(), f)
}

// open opens the file at rel in the page's folder, or gives nil when it
// can't be served. It looks at the file as it is now, so a file replaced by a
// link since the address was made is judged by where the link goes.
func (d pageDir) open(rel string) (*os.File, string) {
	if !servable(rel) {
		return nil, ""
	}
	dir, err := filepath.EvalSymlinks(d.dir)
	if err != nil {
		return nil, ""
	}
	real, err := filepath.EvalSymlinks(filepath.Join(dir, filepath.FromSlash(rel)))
	if err != nil {
		return nil, ""
	}
	inside, err := filepath.Rel(dir, real)
	if err != nil || !servable(filepath.ToSlash(inside)) {
		return nil, ""
	}
	if d.only != "" && real != d.only {
		return nil, ""
	}
	f, err := os.Open(real)
	if err != nil {
		return nil, ""
	}
	return f, filepath.Base(real)
}

// servable reports whether a path inside a page's folder, with / between
// its parts, may be served by its name: no part is hidden, empty or .., and
// the file isn't named like a secret.
func servable(rel string) bool {
	parts := strings.Split(rel, "/")
	for _, p := range parts {
		if p == "" || strings.HasPrefix(p, ".") || strings.ContainsRune(p, 0) {
			return false
		}
	}
	return !slack.LooksSecret(parts[len(parts)-1])
}

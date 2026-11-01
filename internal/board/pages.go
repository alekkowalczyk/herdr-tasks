package board

import (
	"crypto/rand"
	"encoding/hex"
	"io"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"syscall"

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
// Pages of one folder share an id, except where the folder holds more than
// the page's own files: see lone.
func (p *pages) href(page string) string {
	dir := filepath.Dir(page)
	d := pageDir{dir: dir}
	key := dir
	if lone(dir) {
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

// lone reports whether a page in dir, with symlinks resolved, gets only
// itself served: dir is / or the home folder, holds the home folder, or is or
// is in a folder of keys under home, as these hold far more than the page.
// Folders are told apart by identity, as a file system may not tell cases
// apart in a path.
func lone(dir string) bool {
	home, err := os.UserHomeDir()
	if err == nil {
		home, err = filepath.EvalSymlinks(home)
	}
	if err != nil {
		return true
	}
	hi, errH := os.Stat(home)
	di, errD := os.Stat(dir)
	if errH != nil || errD != nil || os.SameFile(di, hi) {
		return true
	}
	// Home's ancestors, up to /.
	for p := filepath.Dir(home); ; p = filepath.Dir(p) {
		if pi, err := os.Stat(p); err != nil || os.SameFile(pi, di) {
			return true
		}
		if p == filepath.Dir(p) {
			break
		}
	}
	// dir's names below home, if it's below home.
	var below []string
	for p := dir; ; p = filepath.Dir(p) {
		if pi, err := os.Stat(p); err != nil {
			return true
		} else if os.SameFile(pi, hi) {
			slices.Reverse(below)
			return slack.LooksSecretPath(below)
		}
		below = append(below, filepath.Base(p))
		if p == filepath.Dir(p) {
			return false
		}
	}
}

const sep = string(filepath.Separator)

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
	http.ServeContent(w, r, name, fi.ModTime(), io.NewSectionReader(f, 0, fi.Size()))
}

// open opens the file at rel in the page's folder, or gives nil when it
// can't be served. It looks at the file as it is now, so a file replaced by a
// link since the address was made is judged by where the link goes.
func (d pageDir) open(rel string) (*os.File, string) {
	if !servable(rel) {
		return nil, ""
	}
	dir, err := filepath.EvalSymlinks(d.dir)
	if err != nil || dir != d.dir {
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
	// A folder in the chain swapped for a link between the EvalSymlinks above
	// and this open gets past the checks; that takes write access to the
	// folder. O_NONBLOCK keeps a FIFO from holding the request: the caller
	// checks that the file is a regular one.
	f, err := os.OpenFile(real, os.O_RDONLY|syscall.O_NONBLOCK|syscall.O_NOFOLLOW, 0)
	if err != nil {
		return nil, ""
	}
	return f, filepath.Base(real)
}

// refused says why the page route won't serve a page of this name and size,
// or "" when it will.
func refused(name string, size int64) string {
	switch {
	case !servable(name):
		return "The board doesn't serve a page named like that."
	case size > maxPage:
		return "That page is over 64 MiB, too big for the board to serve."
	}
	return ""
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

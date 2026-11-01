package board

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"
)

// shareWait is how long a share or a stop waits for a sync with Tailscale
// under way, which makes a few calls of up to 10 s each.
const shareWait = 30 * time.Second

// shareProxy is a shared port's door: it sends what comes through
// tailscale serve to localhost:port, as a request made on this machine.
// address is the port's address on the tailnet, or "" while the board has
// none.
func shareProxy(port int, address func() string) http.Handler {
	p := strconv.Itoa(port)
	local := "localhost:" + p
	return &httputil.ReverseProxy{
		Rewrite: func(r *httputil.ProxyRequest) {
			r.SetURL(&url.URL{Scheme: "http", Host: "127.0.0.1:" + p})
			// Dev servers such as Vite refuse a Host or an Origin that isn't
			// theirs.
			r.Out.Host = local
			if r.In.Header.Get("Origin") != "" {
				r.Out.Header.Set("Origin", "http://"+local)
			}
		},
		Transport: &http.Transport{
			// Node's dev servers often listen on [::1] only.
			DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
				var d net.Dialer
				c, err := d.DialContext(ctx, "tcp", "127.0.0.1:"+p)
				if err == nil {
					return c, nil
				}
				if c, err6 := d.DialContext(ctx, "tcp", "[::1]:"+p); err6 == nil {
					return c, nil
				}
				return nil, err
			},
			IdleConnTimeout: 90 * time.Second,
		},
		ModifyResponse: func(resp *http.Response) error {
			loc := resp.Header.Get("Location")
			for _, from := range []string{"http://" + local, "http://127.0.0.1:" + p} {
				rest, ok := strings.CutPrefix(loc, from)
				if ok && (rest == "" || strings.ContainsAny(rest[:1], "/?#")) {
					if a := address(); a != "" {
						resp.Header.Set("Location", a+rest)
					}
				}
			}
			return nil
		},
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
			http.Error(w, "Nothing answers at "+local+".", http.StatusBadGateway)
		},
	}
}

// shareSocket is the name of a shared port's socket.
func shareSocket(port int) string { return fmt.Sprintf("share-%d.sock", port) }

// shareAddress is a shared port's address on the tailnet: the board's,
// with that port. It's "" while the board has none.
func (s *Server) shareAddress(port int) string {
	u, err := url.Parse(s.tailnetOrigin())
	if err != nil || u.Hostname() == "" {
		return ""
	}
	return tailnetAddress(u.Hostname(), port)
}

// localLink is the port of a link to localhost or 127.0.0.1, and what
// follows the port, or why it can't be shared.
func localLink(raw string, boardPort int) (port int, rest string, why string) {
	u, err := url.Parse(raw)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") {
		return 0, "", "Only localhost links can be shared."
	}
	if h := strings.ToLower(u.Hostname()); h != "localhost" && h != "127.0.0.1" {
		return 0, "", "Only localhost links can be shared."
	}
	if u.Port() == "" {
		return 0, "", "Only a localhost link with a port can be shared."
	}
	port, err = strconv.Atoi(u.Port())
	switch {
	case err != nil || port < 1 || port > 65535:
		return 0, "", fmt.Sprintf("There's no port %s: ports go from 1 to 65535.", u.Port())
	case port == boardPort:
		return 0, "", fmt.Sprintf("Port %d is the board's own.", port)
	}
	rest = u.EscapedPath()
	if u.RawQuery != "" {
		rest += "?" + u.RawQuery
	}
	if u.Fragment != "" {
		rest += "#" + u.EscapedFragment()
	}
	return port, rest, ""
}

// shareLink shares the port of a task's localhost link with the tailnet,
// and answers with the link's address there. It's for a board seen through
// the tailnet, where localhost is the browser's own machine.
func (s *Server) shareLink(w http.ResponseWriter, r *http.Request) {
	if !onTailnetDoor(r.Context()) {
		writeError(w, http.StatusForbidden, "Share ports from the board's tailnet address.")
		return
	}
	var req openRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10)).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "That isn't a share request.")
		return
	}
	if req.Session == "" || req.Tab == "" {
		writeError(w, http.StatusBadRequest, "A share request needs a session and a tab.")
		return
	}
	if e := s.linkOnTask(r.Context(), req.Session, req.Tab, req.URL); e != nil {
		writeError(w, e.status, e.msg)
		return
	}
	port, rest, why := localLink(req.URL, s.cfg.Port)
	if why != "" {
		writeError(w, http.StatusBadRequest, why)
		return
	}
	address, e := s.sharePort(r.Context(), port)
	if e != nil {
		writeError(w, e.status, e.msg)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"href": address + rest})
}

// sharePort shares localhost:port with the tailnet, unless herdr-tasks does
// already or Tailscale serves the port for something else, and says at
// what address. A share whose serve fails isn't recorded, and its socket
// goes.
func (s *Server) sharePort(ctx context.Context, port int) (string, *taskError) {
	t := &s.tailnet
	wait, cancel := context.WithTimeout(ctx, shareWait)
	defer cancel()
	if !t.lock(wait) {
		return "", &taskError{http.StatusServiceUnavailable, "Tailscale is slow to answer. Try again in a minute."}
	}
	defer t.mu.Unlock()
	address := s.shareAddress(port)
	if !t.on || t.closed || address == "" {
		return "", &taskError{http.StatusConflict, "The board isn't on your tailnet now."}
	}
	// Tailscale is asked on the server's context, so a page closed meanwhile
	// leaves no share half made.
	ctx, ts := s.ctx, s.cfg.Tailscale
	failed := func(status int, err error) *taskError {
		return &taskError{status, fmt.Sprintf("Couldn't share port %d with your tailnet: %v", port, err)}
	}
	served, err := ts.Served(ctx)
	if err != nil {
		return "", failed(http.StatusBadGateway, err)
	}
	rec := s.tailnetRecord()
	i := slices.IndexFunc(rec.Shares, func(e sharedEntry) bool { return e.Port == port })
	target := "unix:" + socketPath(s.cfg.StateDir, shareSocket(port))
	got, listed := served[port]
	made := false
	switch {
	case i >= 0 && listed && got == rec.Shares[i].Target:
		if err := s.listenShare(port); err != nil {
			return "", failed(http.StatusInternalServerError, err)
		}
		return address, nil
	case listed && got == target:
		// Its own entry, though the record of it was lost.
		if err := s.listenShare(port); err != nil {
			return "", failed(http.StatusInternalServerError, err)
		}
		target = got
	case listed:
		if i >= 0 {
			// Recorded, but someone else's now: it's only forgotten.
			t.stopShare(port)
			rec.Shares = slices.Delete(rec.Shares, i, i+1)
			if err := saveRecord(s.cfg.StateDir, rec); err != nil {
				logOnce(&t.shareSaid, "tailnet: "+err.Error())
			}
			s.showShares(rec.Shares)
			log.Printf("tailnet: port %d serves something else now; left it alone", port)
		}
		return "", &taskError{http.StatusConflict, fmt.Sprintf("Port %d is already shared with your tailnet for something else.", port)}
	default:
		if err := s.listenShare(port); err != nil {
			return "", failed(http.StatusInternalServerError, err)
		}
		if err := ts.Serve(ctx, port, target); err != nil {
			t.stopShare(port)
			return "", failed(http.StatusBadGateway, err)
		}
		made = true
		// Recorded as Tailscale reports it, as the board's entry is.
		if after, err := ts.Served(ctx); err == nil && after[port] != "" {
			target = after[port]
		}
	}
	e := sharedEntry{Port: port, Target: target, At: s.now()}
	if i >= 0 {
		rec.Shares[i] = e
	} else {
		rec.Shares = append(rec.Shares, e)
		slices.SortFunc(rec.Shares, func(a, b sharedEntry) int { return a.Port - b.Port })
	}
	if err := saveRecord(s.cfg.StateDir, rec); err != nil {
		if made {
			// Not recorded, it would never be taken out.
			_ = ts.Off(ctx, port)
			t.stopShare(port)
		}
		return "", &taskError{http.StatusInternalServerError, "Couldn't record the share: " + err.Error()}
	}
	log.Printf("tailnet: shared localhost:%d with the tailnet", port)
	s.showShares(rec.Shares)
	return address, nil
}

// stopSharing stops sharing a port with the tailnet, from either door: only
// a share herdr-tasks recorded, and only while Tailscale still serves it as
// recorded. One that's someone else's now is only forgotten.
func (s *Server) stopSharing(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Port int `json:"port"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096)).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "Say which port to stop sharing.")
		return
	}
	if e := s.unsharePort(r.Context(), req.Port); e != nil {
		writeError(w, e.status, e.msg)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// unsharePort takes a recorded share out of tailscale serve, stops
// listening for it, and forgets it.
func (s *Server) unsharePort(ctx context.Context, port int) *taskError {
	notSharing := &taskError{http.StatusNotFound, fmt.Sprintf("herdr-tasks isn't sharing port %d.", port)}
	if s.cfg.Tailscale == nil {
		return notSharing
	}
	t := &s.tailnet
	wait, cancel := context.WithTimeout(ctx, shareWait)
	defer cancel()
	if !t.lock(wait) {
		return &taskError{http.StatusServiceUnavailable, "Tailscale is slow to answer. Try again in a minute."}
	}
	defer t.mu.Unlock()
	rec := s.tailnetRecord()
	i := slices.IndexFunc(rec.Shares, func(e sharedEntry) bool { return e.Port == port })
	if i < 0 {
		return notSharing
	}
	ctx, ts := s.ctx, s.cfg.Tailscale
	served, err := ts.Served(ctx)
	off := false
	if err == nil {
		off, err = unshare(ctx, ts, served, rec.Shares[i])
	}
	if err != nil {
		return &taskError{http.StatusBadGateway, fmt.Sprintf("Couldn't stop sharing port %d: %v", port, err)}
	}
	t.stopShare(port)
	rec.Shares = slices.Delete(rec.Shares, i, i+1)
	s.showShares(rec.Shares)
	if err := saveRecord(s.cfg.StateDir, rec); err != nil {
		return &taskError{http.StatusInternalServerError, "Couldn't record that: " + err.Error()}
	}
	if !off {
		log.Printf("tailnet: port %d serves something else now; left it alone", port)
		return notSharing
	}
	log.Printf("tailnet: stopped sharing localhost:%d", port)
	return nil
}

// listenShare listens on a shared port's socket and serves its proxy there,
// unless it does already.
func (s *Server) listenShare(port int) error {
	t := &s.tailnet
	if d := t.shares[port]; d != nil {
		select {
		case <-d.done: // it stopped on its own, so it opens again
			t.stopShare(port)
		default:
			return nil
		}
	}
	sock := socketPath(s.cfg.StateDir, shareSocket(port))
	d, err := s.serveSocket(sock, shareProxy(port, func() string { return s.shareAddress(port) }))
	if err != nil {
		return err
	}
	if t.shares == nil {
		t.shares = map[int]*tailnetDoor{}
	}
	t.shares[port] = d
	return nil
}

// stopShare stops listening for a shared port. Its socket goes, and its
// requests and WebSockets end.
func (t *tailnetState) stopShare(port int) {
	if d := t.shares[port]; d != nil {
		delete(t.shares, port)
		d.close()
	}
}

// stopShares stops listening for every shared port.
func (t *tailnetState) stopShares() {
	for port := range t.shares {
		t.stopShare(port)
	}
}

// showShares has the header list shares.
func (s *Server) showShares(shares []sharedEntry) {
	v := s.tailnet.view.Load()
	if v == nil || v.line == nil {
		return
	}
	line := *v.line
	line.Shares = sharedPorts(shares)
	s.showTailnet(&line, v.owner)
}

// sharedPorts is shares as the header lists them.
func sharedPorts(shares []sharedEntry) []SharedPort {
	var out []SharedPort
	for _, e := range shares {
		out = append(out, SharedPort{Port: e.Port, Since: e.At})
	}
	return out
}

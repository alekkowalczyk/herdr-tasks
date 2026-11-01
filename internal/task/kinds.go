package task

import (
	"net/url"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

// Kind is what a link points at. It decides where the link shows.
type Kind string

const (
	KindLinear Kind = "linear"
	// https://*.slack.com/archives/…, any number of subdomain labels.
	KindSlack Kind = "slack"
	// https://claude.ai/artifact/<id> or /code/artifact/<id>.
	KindArtifact Kind = "artifact"
	// http(s) on a static host: *.github.io, *.gitlab.io, *.vercel.app,
	// *.netlify.app, *.pages.dev, *.web.app, *.firebaseapp.com, *.surge.sh.
	KindHosted Kind = "hosted"
	// file:///abs/path; the host is empty or "localhost".
	KindFile Kind = "file"
	// http(s) to localhost, 127.0.0.1 or [::1].
	KindLocal Kind = "local"
	KindOther Kind = "other"
)

var staticHosts = []string{".github.io", ".gitlab.io", ".vercel.app", ".netlify.app", ".pages.dev", ".web.app", ".firebaseapp.com", ".surge.sh"}

// KindOf tells what a link URL points at.
func KindOf(raw string) Kind {
	if _, ok := LinearID(raw); ok {
		return KindLinear
	}
	if _, ok := FilePath(raw); ok {
		return KindFile
	}
	u, err := url.Parse(raw)
	if err != nil || (u.Scheme != "https" && u.Scheme != "http") {
		return KindOther
	}
	host := strings.ToLower(u.Hostname())
	parts := strings.Split(strings.Trim(u.EscapedPath(), "/"), "/")
	switch {
	case host == "localhost" || host == "127.0.0.1" || host == "::1":
		return KindLocal
	case u.Scheme == "https" && strings.HasSuffix(host, ".slack.com") && len(parts) >= 2 && parts[0] == "archives" && parts[1] != "":
		return KindSlack
	case u.Scheme == "https" && host == "claude.ai" && artifactPath(parts):
		return KindArtifact
	}
	for _, s := range staticHosts {
		if strings.HasSuffix(host, s) {
			return KindHosted
		}
	}
	return KindOther
}

// artifactPath reports whether a claude.ai path, split on "/", is
// artifact/<id> or code/artifact/<id>.
func artifactPath(parts []string) bool {
	if len(parts) == 3 && parts[0] == "code" {
		parts = parts[1:]
	}
	return len(parts) == 2 && parts[0] == "artifact" && parts[1] != ""
}

// Section is where a kind shows: "Linear", "Slack", "Pages" (artifact,
// hosted, file, local) or "Links".
func (k Kind) Section() string {
	switch k {
	case KindLinear:
		return "Linear"
	case KindSlack:
		return "Slack"
	case KindArtifact, KindHosted, KindFile, KindLocal:
		return "Pages"
	}
	return "Links"
}

// Sections in display order.
var Sections = []string{"Linear", "Slack", "Pages", "Links"}

// LinkKey is a link's identity, for dedupe and dismissal: the same page under
// another URL form has the same key.
func LinkKey(raw string) string {
	kind := KindOf(raw)
	switch kind {
	case KindLinear:
		id, _ := LinearID(raw)
		return "linear:" + id
	case KindFile:
		p, _ := FilePath(raw)
		return "file:" + p
	case KindOther:
		return raw
	}
	u, _ := url.Parse(raw) // KindOf parsed it already.
	host := strings.ToLower(u.Host)
	p := strings.TrimRight(u.EscapedPath(), "/")
	switch kind {
	case KindSlack:
		// The query only says which reply or view it opens.
		return "slack:" + host + p
	case KindArtifact:
		// The query holds the share key.
		return "artifact:" + path.Base(p)
	}
	key := string(kind) + ":" + u.Scheme + "://" + host + p
	if u.RawQuery != "" {
		key += "?" + u.RawQuery
	}
	if u.Fragment != "" {
		key += "#" + u.EscapedFragment()
	}
	return key
}

var slackMessage = regexp.MustCompile(`^p(\d{10})(\d{6})$`)

// SlackTime is when a Slack message was posted, from its link's /p<16
// digits>: the first 10 are seconds, the rest microseconds.
func SlackTime(raw string) (time.Time, bool) {
	if KindOf(raw) != KindSlack {
		return time.Time{}, false
	}
	u, _ := url.Parse(raw)
	m := slackMessage.FindStringSubmatch(path.Base(u.Path))
	if m == nil {
		return time.Time{}, false
	}
	sec, _ := strconv.ParseInt(m[1], 10, 64)
	usec, _ := strconv.ParseInt(m[2], 10, 64)
	return time.Unix(sec, usec*1000), true
}

// FileURL is the file:// URL of an absolute path.
func FileURL(path string) string {
	return (&url.URL{Scheme: "file", Path: filepath.Clean(path)}).String()
}

// FilePath is the absolute path a file:// URL points at. It's false for a
// file on another host, or a relative path.
func FilePath(raw string) (string, bool) {
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "file" || (u.Host != "" && !strings.EqualFold(u.Host, "localhost")) || !filepath.IsAbs(u.Path) {
		return "", false
	}
	return filepath.Clean(u.Path), true
}

// SafeURL reports whether raw has no control characters (C0, DEL, C1) once
// percent-decoded, and parses.
func SafeURL(raw string) bool {
	if hasControl(raw) || hasControl(percentDecode(raw)) {
		return false
	}
	_, err := url.Parse(raw)
	return err == nil
}

// percentDecode decodes every valid %XX in s, and leaves the rest as it is.
func percentDecode(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] == '%' && i+2 < len(s) {
			if n, err := strconv.ParseUint(s[i+1:i+3], 16, 8); err == nil {
				b.WriteByte(byte(n))
				i += 2
				continue
			}
		}
		b.WriteByte(s[i])
	}
	return b.String()
}

// isControl reports whether r is a C0 or C1 control character, or DEL.
func isControl(r rune) bool { return r < 0x20 || r == 0x7f || r >= 0x80 && r <= 0x9f }

// hasControl reports whether s has a control character. A byte that isn't
// valid UTF-8 counts as its own value, so a lone 0x9b is a control.
func hasControl(s string) bool {
	for i := 0; i < len(s); {
		r, n := utf8.DecodeRuneInString(s[i:])
		if r == utf8.RuneError && n == 1 {
			r = rune(s[i])
		}
		if isControl(r) {
			return true
		}
		i += n
	}
	return false
}

// CleanLabel drops control characters (C0, DEL, C1) and bytes that aren't
// UTF-8, collapses whitespace runs to one space, trims, and cuts to 80 runes
// ending in "…".
func CleanLabel(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); {
		r, n := utf8.DecodeRuneInString(s[i:])
		i += n
		switch {
		case unicode.IsSpace(r):
			b.WriteByte(' ')
		case r == utf8.RuneError && n == 1, isControl(r):
		default:
			b.WriteRune(r)
		}
	}
	s = strings.Join(strings.Fields(b.String()), " ")
	if r := []rune(s); len(r) > 80 {
		s = strings.TrimRight(string(r[:79]), " ") + "…"
	}
	return s
}

// CleanText is CleanLabel for text of several lines, such as a note: it keeps
// line breaks, turns tabs into spaces, and drops every other control
// character (C0, DEL, C1) and bytes that aren't UTF-8. It doesn't collapse
// whitespace or cut the text.
func CleanText(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); {
		r, n := utf8.DecodeRuneInString(s[i:])
		i += n
		switch {
		case r == '\n':
			b.WriteByte('\n')
		case r == '\t':
			b.WriteByte(' ')
		case r == utf8.RuneError && n == 1, isControl(r):
		default:
			b.WriteRune(r)
		}
	}
	return b.String()
}

// LinkInput turns what you typed into a link URL. It's an existing file
// (absolute, ~/…, or relative to cwd when cwd != "") as a file:// URL; else
// raw, with https:// added when it has no "://".
func LinkInput(raw, cwd string) string {
	if strings.Contains(raw, "://") {
		return raw
	}
	p := raw
	if rest, ok := strings.CutPrefix(raw, "~/"); ok {
		if home, err := os.UserHomeDir(); err == nil {
			p = filepath.Join(home, rest)
		}
	} else if !filepath.IsAbs(raw) && cwd != "" {
		p = filepath.Join(cwd, raw)
	}
	if !filepath.IsAbs(p) {
		return "https://" + raw
	}
	if fi, err := os.Stat(p); err == nil && fi.Mode().IsRegular() {
		return FileURL(p)
	}
	return "https://" + raw
}

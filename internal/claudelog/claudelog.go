// Package claudelog reads what a Claude Code session is doing from its
// session log: the title Claude gave it, the recap Claude shows when you come
// back, and the last prompt and reply.
package claudelog

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"
	"unicode/utf8"
)

// Summary is what a session log says about its session. Fields the log
// doesn't have yet are empty.
type Summary struct {
	Title      string    // latest "ai-title" aiTitle
	Recap      string    // latest system/away_summary content
	RecapAt    time.Time // its timestamp
	RecapStale bool      // a "last-prompt" entry comes after the latest recap
	LastPrompt string    // latest "last-prompt" lastPrompt, cut to 600 runes
	LastReply  string    // latest non-sidechain assistant text, cut to 600 runes
	ReplyAt    time.Time // its timestamp
}

const (
	chunkSize  = 256 << 10
	firstBack  = 32 << 20 // how far back Cache reads a log it hasn't seen
	maxForward = 8 << 20  // the most Cache reads of what was added; past it, it starts over
	maxRunes   = 600
)

// file is an open log. Reads go through openFile so tests can count them.
type file interface {
	io.ReaderAt
	io.Closer
	Stat() (os.FileInfo, error)
}

var openFile = func(name string) (file, error) {
	f, err := os.Open(name)
	if err != nil {
		return nil, err
	}
	return f, nil
}

// ConfigDir is Claude Code's config directory: $CLAUDE_CONFIG_DIR, else ~/.claude.
func ConfigDir() string {
	if d := os.Getenv("CLAUDE_CONFIG_DIR"); d != "" {
		return d
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".claude")
}

// sessionID keeps IDs to the characters a Claude session ID uses, so one can't
// name a file outside the projects.
var sessionID = regexp.MustCompile(`^[0-9a-fA-F-]{8,64}$`)

// Path finds the log of session id, configDir/projects/<project>/<id>.jsonl,
// in whichever project folder holds it rather than rebuilding Claude's folder
// names. It returns "" when there's no log: Claude writes it once the session
// has a message. If two folders have one, the newer wins.
func Path(configDir, id string) (string, error) {
	if !sessionID.MatchString(id) {
		return "", fmt.Errorf("not a Claude session ID: %q", id)
	}
	projects := filepath.Join(configDir, "projects")
	dirs, err := os.ReadDir(projects)
	if errors.Is(err, fs.ErrNotExist) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	var path string
	var newest time.Time
	for _, d := range dirs {
		p := filepath.Join(projects, d.Name(), id+".jsonl")
		fi, err := os.Stat(p)
		if err != nil || !fi.Mode().IsRegular() {
			continue
		}
		if path == "" || fi.ModTime().After(newest) {
			path, newest = p, fi.ModTime()
		}
	}
	return path, nil
}

// cwdBytes is how much of a log's start Cwd reads.
const cwdBytes = 1 << 20

// Cwd is the folder the session at path ran in, as its first lines that
// name one say, or "" when they don't. claude --resume finds a session
// only in that folder.
func Cwd(path string) (string, error) {
	f, err := openFile(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	b := make([]byte, cwdBytes)
	n, err := f.ReadAt(b, 0)
	if err != nil && !errors.Is(err, io.EOF) {
		return "", err
	}
	lines := bytes.Split(b[:n], []byte("\n"))
	if n == len(b) {
		lines = lines[:len(lines)-1] // cut short
	}
	for _, line := range lines {
		if !bytes.Contains(line, []byte(`"cwd"`)) {
			continue
		}
		var e struct {
			Cwd string `json:"cwd"`
		}
		if json.Unmarshal(line, &e) == nil && e.Cwd != "" {
			return e.Cwd, nil
		}
	}
	return "", nil
}

// Read reads the Summary of the log at path. Logs can run to hundreds of MB,
// so it reads back from the end in 256 KiB chunks and stops once it has every
// field, or after maxBack bytes. A last line without its newline is still
// being written and is skipped.
func Read(path string, maxBack int64) (Summary, error) {
	f, err := openFile(path)
	if err != nil {
		return Summary{}, err
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		return Summary{}, err
	}
	s, _, err := scan(f, fi.Size(), maxBack)
	return s, err
}

// scan reads f back from size, at most maxBack bytes. end is where the last
// complete line ends, which is where reading on later starts.
func scan(f file, size, maxBack int64) (s Summary, end int64, err error) {
	end = -1
	pos := size
	// rest is the part after pos of a line whose start isn't read yet.
	var rest [][]byte
	for pos > 0 && size-pos < maxBack {
		n := min(chunkSize, pos, maxBack-(size-pos))
		buf := make([]byte, n)
		if err := readAt(f, buf, pos-n); err != nil {
			return Summary{}, 0, err
		}
		pos -= n
		for {
			i := bytes.LastIndexByte(buf, '\n')
			if i < 0 {
				break
			}
			line := join(buf[i+1:], rest)
			rest = nil
			if end < 0 {
				// The bytes after the last newline are a line still being written.
				end = pos + int64(i) + 1
			} else {
				s.older(line)
				if s.complete() {
					return s, end, nil
				}
			}
			buf = buf[:i]
		}
		rest = append([][]byte{buf}, rest...)
	}
	if pos == 0 && end >= 0 {
		s.older(join(nil, rest)) // the log's first line
	}
	if end < 0 {
		// No newline within reach. Reading on from here starts mid-line, and
		// that piece won't decode.
		end = pos
	}
	return s, end, nil
}

// join puts a line's first bytes before the rest of it.
func join(first []byte, rest [][]byte) []byte {
	if len(rest) == 0 {
		return first
	}
	return bytes.Join(append([][]byte{first}, rest...), nil)
}

// readAt fills p from off, or fails.
func readAt(f file, p []byte, off int64) error {
	n, err := f.ReadAt(p, off)
	if n == len(p) {
		return nil
	}
	if err == io.EOF {
		err = io.ErrUnexpectedEOF
	}
	return err
}

// kind is the Summary field a log line sets.
type kind int

const (
	other kind = iota
	title
	prompt
	recap
	reply
)

// markers are bytes each kind of line holds, so other lines are skipped
// without decoding them.
var markers = [...][]byte{
	title:  []byte(`"ai-title"`),
	prompt: []byte(`"last-prompt"`),
	recap:  []byte(`"away_summary"`),
	reply:  []byte(`"assistant"`),
}

type kinds [len(markers)]bool

var allKinds = kinds{title: true, prompt: true, recap: true, reply: true}

func (s *Summary) missing() kinds {
	return kinds{title: s.Title == "", prompt: s.LastPrompt == "", recap: s.Recap == "", reply: s.LastReply == ""}
}

func (s *Summary) complete() bool {
	return s.Title != "" && s.LastPrompt != "" && s.Recap != "" && s.LastReply != ""
}

// older adds a line that comes before every line seen so far, so it only
// fills fields that are still empty.
func (s *Summary) older(line []byte) {
	k, text, at := parse(line, s.missing())
	switch k {
	case title:
		s.Title = text
	case prompt:
		s.LastPrompt = text
	case recap:
		// A prompt already seen comes later in the log.
		s.Recap, s.RecapAt, s.RecapStale = text, at, s.LastPrompt != ""
	case reply:
		s.LastReply, s.ReplyAt = text, at
	}
}

// newer adds a line that comes after every line seen so far.
func (s *Summary) newer(line []byte) {
	k, text, at := parse(line, allKinds)
	switch k {
	case title:
		s.Title = text
	case prompt:
		s.LastPrompt, s.RecapStale = text, s.Recap != ""
	case recap:
		s.Recap, s.RecapAt, s.RecapStale = text, at, false
	case reply:
		s.LastReply, s.ReplyAt = text, at
	}
}

// forward adds the complete lines in b, which comes after every line seen so
// far, and returns how many bytes they take.
func (s *Summary) forward(b []byte) int64 {
	var n int64
	for {
		i := bytes.IndexByte(b, '\n')
		if i < 0 {
			return n
		}
		s.newer(b[:i])
		b = b[i+1:]
		n += int64(i) + 1
	}
}

// parse reads the field a log line sets, if its kind is in want. Lines that
// don't decode, and empty values, set nothing.
func parse(line []byte, want kinds) (kind, string, time.Time) {
	wanted := false
	for k, m := range markers {
		if want[k] && bytes.Contains(line, m) {
			wanted = true
			break
		}
	}
	if !wanted {
		return other, "", time.Time{}
	}
	var e struct {
		Type        string          `json:"type"`
		Subtype     string          `json:"subtype"`
		AITitle     string          `json:"aiTitle"`
		LastPrompt  string          `json:"lastPrompt"`
		Content     json.RawMessage `json:"content"`
		Timestamp   string          `json:"timestamp"`
		IsSidechain bool            `json:"isSidechain"`
		Message     struct {
			Content json.RawMessage `json:"content"`
		} `json:"message"`
	}
	if json.Unmarshal(line, &e) != nil {
		return other, "", time.Time{}
	}
	k, text, at := other, "", time.Time{}
	switch {
	case e.Type == "ai-title":
		k, text = title, e.AITitle
	case e.Type == "last-prompt":
		k, text = prompt, cut(e.LastPrompt)
	case e.Type == "system" && e.Subtype == "away_summary":
		k = recap
		_ = json.Unmarshal(e.Content, &text)
		at, _ = time.Parse(time.RFC3339Nano, e.Timestamp)
	case e.Type == "assistant" && !e.IsSidechain:
		k, text = reply, cut(replyText(e.Message.Content))
		at, _ = time.Parse(time.RFC3339Nano, e.Timestamp)
	}
	text = strings.TrimSpace(text)
	if !want[k] || text == "" {
		return other, "", time.Time{}
	}
	return k, text, at
}

// replyText is the text of an assistant message, without its tool calls and
// thinking.
func replyText(content json.RawMessage) string {
	var parts []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if json.Unmarshal(content, &parts) != nil {
		var s string
		_ = json.Unmarshal(content, &s)
		return s
	}
	var texts []string
	for _, p := range parts {
		if t := strings.TrimSpace(p.Text); p.Type == "text" && t != "" {
			texts = append(texts, t)
		}
	}
	return strings.Join(texts, "\n\n")
}

// cut shortens s to maxRunes, ending in "…" when it was longer.
func cut(s string) string {
	s = strings.TrimSpace(s)
	if utf8.RuneCountInString(s) <= maxRunes {
		return s
	}
	return string([]rune(s)[:maxRunes-1]) + "…"
}

// Cache keeps each session's Summary, and when a log grows it reads only what
// was added. It's safe for concurrent use.
type Cache struct {
	dir   string
	mu    sync.Mutex
	paths map[string]string    // session ID → log path
	logs  map[string]*logState // log path → what's been read of it
}

// logState is what Cache has read of one log.
type logState struct {
	fi   os.FileInfo // to notice the file was replaced
	size int64       // its size when read
	// end is where its last complete line ends. A line still being written
	// after it is read again next time.
	end int64
	sum Summary
}

// NewCache reads logs under configDir, Claude's config directory.
func NewCache(configDir string) *Cache {
	return &Cache{dir: configDir, paths: map[string]string{}, logs: map[string]*logState{}}
}

// Get returns the Summary of session id's log. ok is false when there's no
// log yet.
func (c *Cache) Get(id string) (Summary, bool, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if path, found := c.paths[id]; found {
		s, err := c.read(path)
		if !errors.Is(err, fs.ErrNotExist) {
			return s, err == nil, err
		}
		// The log moved or went away: look for it again.
		delete(c.paths, id)
		delete(c.logs, path)
	}
	path, err := Path(c.dir, id)
	if err != nil || path == "" {
		return Summary{}, false, err
	}
	s, err := c.read(path)
	if errors.Is(err, fs.ErrNotExist) {
		return Summary{}, false, nil
	}
	if err != nil {
		return Summary{}, false, err
	}
	c.paths[id] = path
	return s, true, nil
}

// read brings the Summary of the log at path up to date. It reads the log in
// full the first time, or when the file shrank or was replaced, and otherwise
// only what was added.
func (c *Cache) read(path string) (Summary, error) {
	f, err := openFile(path)
	if err != nil {
		return Summary{}, err
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		return Summary{}, err
	}
	size := fi.Size()
	l := c.logs[path]
	if l != nil && os.SameFile(l.fi, fi) && size >= l.size {
		if size == l.size {
			return l.sum, nil
		}
		if size-l.end <= maxForward {
			buf := make([]byte, size-l.end)
			if err := readAt(f, buf, l.end); err != nil {
				return Summary{}, err
			}
			sum := l.sum
			end := l.end + sum.forward(buf)
			c.logs[path] = &logState{fi: fi, size: size, end: end, sum: sum}
			return sum, nil
		}
	}
	sum, end, err := scan(f, size, firstBack)
	if err != nil {
		return Summary{}, err
	}
	c.logs[path] = &logState{fi: fi, size: size, end: end, sum: sum}
	return sum, nil
}

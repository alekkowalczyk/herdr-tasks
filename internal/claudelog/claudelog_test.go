package claudelog

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
	"unicode/utf8"
)

// sid is an invented Claude session ID.
const sid = "0b5e1c2a-7d3f-4e8a-9c61-2f4d8a1b3c5e"

func entry(v map[string]any) string {
	b, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	return string(b)
}

func aiTitle(s string) string {
	return entry(map[string]any{"type": "ai-title", "aiTitle": s})
}

func lastPrompt(s string) string {
	return entry(map[string]any{"type": "last-prompt", "lastPrompt": s})
}

func awaySummary(s, at string) string {
	return entry(map[string]any{"type": "system", "subtype": "away_summary", "content": s, "timestamp": at})
}

func assistant(sidechain bool, parts ...map[string]any) string {
	return entry(map[string]any{"type": "assistant", "isSidechain": sidechain, "message": map[string]any{"content": parts}})
}

func answer(s string) string { return assistant(false, map[string]any{"type": "text", "text": s}) }

// answerAt is a reply from the session at the time at.
func answerAt(s, at string) string {
	return entry(map[string]any{"type": "assistant", "timestamp": at, "message": map[string]any{"content": []map[string]any{{"type": "text", "text": s}}}})
}

func user(s string) string {
	return entry(map[string]any{"type": "user", "message": map[string]any{"role": "user", "content": s}})
}

// padding is user lines of about n bytes in all.
func padding(n int) string {
	var b strings.Builder
	for b.Len() < n {
		b.WriteString(user(strings.Repeat("x", 1000)) + "\n")
	}
	return b.String()
}

// logText is lines as a log holds them, each ending in a newline.
func logText(lines ...string) string {
	var b strings.Builder
	for _, l := range lines {
		b.WriteString(l)
		if !strings.HasSuffix(l, "\n") {
			b.WriteString("\n")
		}
	}
	return b.String()
}

func write(t *testing.T, path, text string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(text), 0o600); err != nil {
		t.Fatal(err)
	}
}

func appendTo(t *testing.T, path, text string) {
	t.Helper()
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if _, err := f.WriteString(text); err != nil {
		t.Fatal(err)
	}
}

// newLog makes a Claude config dir and returns it with the path of session
// sid's log in one of its projects. The log isn't written yet.
func newLog(t *testing.T) (dir, path string) {
	dir = t.TempDir()
	return dir, filepath.Join(dir, "projects", "-Users-acme-app", sid+".jsonl")
}

func readText(t *testing.T, text string, maxBack int64) Summary {
	t.Helper()
	path := filepath.Join(t.TempDir(), sid+".jsonl")
	write(t, path, text)
	s, err := Read(path, maxBack)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

type countingFile struct {
	*os.File
	n *int64
}

func (f countingFile) ReadAt(p []byte, off int64) (int, error) {
	n, err := f.File.ReadAt(p, off)
	*f.n += int64(n)
	return n, err
}

// countReads counts the bytes read from logs until the test ends.
func countReads(t *testing.T) *int64 {
	n := new(int64)
	old := openFile
	openFile = func(name string) (file, error) {
		f, err := os.Open(name)
		if err != nil {
			return nil, err
		}
		return countingFile{f, n}, nil
	}
	t.Cleanup(func() { openFile = old })
	return n
}

func TestReadFindsLatestOfEach(t *testing.T) {
	s := readText(t, logText(
		aiTitle("Look around"),
		lastPrompt("what does this repo do"),
		awaySummary("You were looking around.", "2026-10-29T05:00:00Z"),
		answer("It's a task board."),
		aiTitle("Fix the login"),
		lastPrompt("fix the login bug"),
		answerAt("Fixed: the token was read before refresh.", "2026-10-29T06:50:00.5Z"),
		// Neither a subagent's reply nor a tool call is the session's reply.
		assistant(true, map[string]any{"type": "text", "text": "subagent notes"}),
		entry(map[string]any{"type": "assistant", "timestamp": "2026-10-29T06:55:00Z", "message": map[string]any{"content": []map[string]any{{"type": "tool_use", "name": "Bash"}}}}),
		user("tool output"),
		awaySummary("You're fixing the login. Next: run the tests.", "2026-10-29T06:56:07.008Z"),
	), 32<<20)
	want := Summary{
		Title:      "Fix the login",
		Recap:      "You're fixing the login. Next: run the tests.",
		RecapAt:    time.Date(2026, 10, 29, 6, 56, 7, 8e6, time.UTC),
		LastPrompt: "fix the login bug",
		LastReply:  "Fixed: the token was read before refresh.",
		ReplyAt:    time.Date(2026, 10, 29, 6, 50, 0, 5e8, time.UTC),
	}
	if !s.RecapAt.Equal(want.RecapAt) {
		t.Errorf("RecapAt = %v, want %v", s.RecapAt, want.RecapAt)
	}
	if !s.ReplyAt.Equal(want.ReplyAt) {
		t.Errorf("ReplyAt = %v, want %v", s.ReplyAt, want.ReplyAt)
	}
	s.RecapAt, s.ReplyAt = want.RecapAt, want.ReplyAt
	if s != want {
		t.Errorf("Read = %+v\nwant %+v", s, want)
	}
}

func TestRecapStaleWhenAPromptFollows(t *testing.T) {
	s := readText(t, logText(awaySummary("You're fixing the login.", "2026-10-29T06:56:07Z"), lastPrompt("now the tests")), 32<<20)
	if !s.RecapStale || s.Recap == "" || s.LastPrompt != "now the tests" {
		t.Errorf("prompt after the recap: Read = %+v, want a stale recap", s)
	}
	s = readText(t, logText(lastPrompt("fix the login"), awaySummary("You're fixing the login.", "2026-10-29T06:56:07Z")), 32<<20)
	if s.RecapStale {
		t.Errorf("prompt before the recap: Read = %+v, want a fresh recap", s)
	}
}

func TestReadCutsLongTexts(t *testing.T) {
	long := strings.Repeat("é", 700)
	s := readText(t, logText(lastPrompt(long), answer(long)), 32<<20)
	for name, got := range map[string]string{"LastPrompt": s.LastPrompt, "LastReply": s.LastReply} {
		if utf8.RuneCountInString(got) != 600 || !strings.HasPrefix(long, strings.TrimSuffix(got, "…")) || !strings.HasSuffix(got, "…") {
			t.Errorf("%s is %d runes, want 600 ending in …", name, utf8.RuneCountInString(got))
		}
	}
}

func TestReadStopsAtCap(t *testing.T) {
	text := logText(aiTitle("Fix the login")) + padding(3<<20)
	n := countReads(t)
	if s := readText(t, text, 1<<20); s.Title != "" {
		t.Errorf("with a 1 MiB cap, Title = %q, want none", s.Title)
	}
	if *n > 1<<20 {
		t.Errorf("read %d bytes, want at most 1 MiB", *n)
	}
	if s := readText(t, text, 4<<20); s.Title != "Fix the login" {
		t.Errorf("with a 4 MiB cap, Title = %q", s.Title)
	}
}

func TestReadStopsOnceItHasEveryField(t *testing.T) {
	text := padding(3<<20) + logText(aiTitle("Fix the login"), lastPrompt("fix the login"),
		answer("Fixed."), awaySummary("You're fixing the login.", "2026-10-29T06:56:07Z"))
	n := countReads(t)
	if s := readText(t, text, 32<<20); !s.complete() {
		t.Errorf("Read = %+v, want every field", s)
	}
	if *n > 256<<10 {
		t.Errorf("read %d bytes, want one chunk", *n)
	}
}

func TestReadJoinsLinesAcrossChunks(t *testing.T) {
	// The reply spans several 256 KiB chunks, and the title is the first line.
	s := readText(t, logText(aiTitle("Fix the login"), answer("Start of a long reply. "+strings.Repeat("a", 700<<10))), 32<<20)
	if s.Title != "Fix the login" || !strings.HasPrefix(s.LastReply, "Start of a long reply. aaa") {
		t.Errorf("Read = %+v", s)
	}
}

func TestReadSkipsTruncatedLastLine(t *testing.T) {
	half := lastPrompt("still being written")
	s := readText(t, logText(lastPrompt("fix the login"))+half[:len(half)/2], 32<<20)
	if s.LastPrompt != "fix the login" {
		t.Errorf("LastPrompt = %q, want the last complete one", s.LastPrompt)
	}
	// A whole file of one unfinished line has nothing yet.
	if s := readText(t, half[:len(half)/2], 32<<20); s != (Summary{}) {
		t.Errorf("Read = %+v, want nothing", s)
	}
}

func TestReadSkipsLinesItCantRead(t *testing.T) {
	s := readText(t, logText(
		lastPrompt("fix the login"),
		`{"type":"last-prompt","lastPrompt":`,
		`not json`,
		`{"type":"last-prompt","lastPrompt":42}`,
		`{"type":"last-prompt"}`,
	), 32<<20)
	if s.LastPrompt != "fix the login" {
		t.Errorf("LastPrompt = %q", s.LastPrompt)
	}
}

func TestConfigDir(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("CLAUDE_CONFIG_DIR", "")
	if got := ConfigDir(); got != filepath.Join(home, ".claude") {
		t.Errorf("ConfigDir = %q", got)
	}
	t.Setenv("CLAUDE_CONFIG_DIR", "/opt/acme/claude")
	if got := ConfigDir(); got != "/opt/acme/claude" {
		t.Errorf("with CLAUDE_CONFIG_DIR, ConfigDir = %q", got)
	}
}

func TestPathFindsTheLogInAnyProject(t *testing.T) {
	dir, path := newLog(t)
	write(t, filepath.Join(dir, "projects", "-Users-acme-other", "1d2c3b4a-0000-4000-8000-000000000001.jsonl"), "")
	write(t, path, logText(lastPrompt("fix the login")))
	if got, err := Path(dir, sid); err != nil || got != path {
		t.Errorf("Path = %q, %v, want %q", got, err, path)
	}
}

func TestPathMissingIsNotAnError(t *testing.T) {
	dir := t.TempDir()
	if got, err := Path(dir, sid); got != "" || err != nil {
		t.Errorf("without projects, Path = %q, %v", got, err)
	}
	write(t, filepath.Join(dir, "projects", "-Users-acme-app", "1d2c3b4a-0000-4000-8000-000000000001.jsonl"), "")
	if got, err := Path(dir, sid); got != "" || err != nil {
		t.Errorf("with another session's log, Path = %q, %v", got, err)
	}
}

func TestPathRejectsGlobCharacters(t *testing.T) {
	dir, path := newLog(t)
	write(t, path, logText(lastPrompt("fix the login")))
	for _, id := range []string{"*", "0b5e1c2a-*", "0b5e1c2a-[0-9]*", "0b5e1c2a-7d3f-????", "../../etc/passwd", "0b5e1c2a/../x", "abc", ""} {
		if got, err := Path(dir, id); err == nil || got != "" {
			t.Errorf("Path(%q) = %q, %v, want an error", id, got, err)
		}
	}
}

func TestCacheReadsOnlyTheAppendedBytes(t *testing.T) {
	dir, path := newLog(t)
	write(t, path, logText(aiTitle("Fix the login"), lastPrompt("fix the login"))+padding(2<<20))
	n := countReads(t)
	c := NewCache(dir)
	if s, ok, err := c.Get(sid); err != nil || !ok || s.LastPrompt != "fix the login" {
		t.Fatalf("Get = %+v, %v, %v", s, ok, err)
	}
	*n = 0
	appendTo(t, path, logText(lastPrompt("now the tests"))+padding(1<<10))
	s, ok, err := c.Get(sid)
	if err != nil || !ok || s.LastPrompt != "now the tests" || s.Title != "Fix the login" {
		t.Errorf("after the append, Get = %+v, %v, %v", s, ok, err)
	}
	if *n >= 64<<10 {
		t.Errorf("after a 1 KiB append, read %d bytes", *n)
	}
	// An unchanged log isn't read again.
	*n = 0
	if s2, _, _ := c.Get(sid); s2 != s || *n != 0 {
		t.Errorf("unchanged, Get = %+v and read %d bytes", s2, *n)
	}
}

func TestCacheMergesNewerEntries(t *testing.T) {
	dir, path := newLog(t)
	write(t, path, logText(aiTitle("Fix the login"), awaySummary("You're fixing the login.", "2026-10-29T06:56:07Z"), answer("Fixed.")))
	c := NewCache(dir)
	if s, _, _ := c.Get(sid); s.RecapStale || s.LastPrompt != "" {
		t.Fatalf("Get = %+v", s)
	}
	appendTo(t, path, logText(lastPrompt("now the tests"), assistant(false, map[string]any{"type": "tool_use", "name": "Bash"})))
	s, _, _ := c.Get(sid)
	if !s.RecapStale || s.LastPrompt != "now the tests" || s.LastReply != "Fixed." || s.Title != "Fix the login" {
		t.Errorf("after a prompt, Get = %+v", s)
	}
	appendTo(t, path, logText(awaySummary("You're running the tests.", "2026-10-29T07:10:00Z"), aiTitle("Run the tests")))
	s, _, _ = c.Get(sid)
	if s.RecapStale || s.Recap != "You're running the tests." || s.Title != "Run the tests" || s.LastPrompt != "now the tests" {
		t.Errorf("after a new recap, Get = %+v", s)
	}
	appendTo(t, path, logText(answerAt("The tests pass.", "2026-10-29T07:12:00Z")))
	s, _, _ = c.Get(sid)
	if s.LastReply != "The tests pass." || !s.ReplyAt.Equal(time.Date(2026, 10, 29, 7, 12, 0, 0, time.UTC)) {
		t.Errorf("after a reply, Get = %+v", s)
	}
}

func TestCacheFinishesAPartialLine(t *testing.T) {
	dir, path := newLog(t)
	next := lastPrompt("now the tests")
	write(t, path, logText(lastPrompt("fix the login"))+next[:10])
	c := NewCache(dir)
	if s, _, _ := c.Get(sid); s.LastPrompt != "fix the login" {
		t.Fatalf("with half a line, LastPrompt = %q", s.LastPrompt)
	}
	appendTo(t, path, next[10:20])
	if s, _, _ := c.Get(sid); s.LastPrompt != "fix the login" {
		t.Fatalf("with more of the line, LastPrompt = %q", s.LastPrompt)
	}
	appendTo(t, path, next[20:]+"\n")
	if s, _, _ := c.Get(sid); s.LastPrompt != "now the tests" {
		t.Errorf("with the whole line, LastPrompt = %q", s.LastPrompt)
	}
}

func TestCacheRereadsAfterTruncation(t *testing.T) {
	dir, path := newLog(t)
	write(t, path, logText(lastPrompt("fix the login"))+padding(10<<10))
	c := NewCache(dir)
	if s, _, _ := c.Get(sid); s.LastPrompt != "fix the login" {
		t.Fatalf("LastPrompt = %q", s.LastPrompt)
	}
	// Written in place, so the file is the same one, only shorter.
	write(t, path, logText(aiTitle("Start over"), lastPrompt("start over")))
	if s, _, _ := c.Get(sid); s.Title != "Start over" || s.LastPrompt != "start over" {
		t.Errorf("after truncation, Get = %+v", s)
	}
}

func TestCacheRereadsAReplacedFile(t *testing.T) {
	dir, path := newLog(t)
	write(t, path, logText(lastPrompt("fix the login"))+padding(10<<10))
	c := NewCache(dir)
	if s, _, _ := c.Get(sid); s.LastPrompt != "fix the login" {
		t.Fatalf("LastPrompt = %q", s.LastPrompt)
	}
	// A bigger file renamed over the log: reading on from the old end would
	// miss its new entries.
	tmp := path + ".tmp"
	write(t, tmp, logText(aiTitle("Replaced"), lastPrompt("start over"))+padding(20<<10))
	if err := os.Rename(tmp, path); err != nil {
		t.Fatal(err)
	}
	if s, _, _ := c.Get(sid); s.Title != "Replaced" || s.LastPrompt != "start over" {
		t.Errorf("after replacing, Get = %+v", s)
	}
}

func TestCacheFollowsAMovedLog(t *testing.T) {
	dir, path := newLog(t)
	write(t, path, logText(lastPrompt("fix the login")))
	c := NewCache(dir)
	if _, ok, err := c.Get(sid); !ok || err != nil {
		t.Fatalf("Get = %v, %v", ok, err)
	}
	moved := filepath.Join(dir, "projects", "-Users-acme-app-worktree", sid+".jsonl")
	if err := os.MkdirAll(filepath.Dir(moved), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(path, moved); err != nil {
		t.Fatal(err)
	}
	if s, ok, err := c.Get(sid); !ok || err != nil || s.LastPrompt != "fix the login" {
		t.Errorf("after the move, Get = %+v, %v, %v", s, ok, err)
	}
	if err := os.Remove(moved); err != nil {
		t.Fatal(err)
	}
	if s, ok, err := c.Get(sid); ok || err != nil || s != (Summary{}) {
		t.Errorf("after removing it, Get = %+v, %v, %v, want no log", s, ok, err)
	}
}

func TestCacheWithoutALog(t *testing.T) {
	c := NewCache(t.TempDir())
	if _, ok, err := c.Get(sid); ok || err != nil {
		t.Errorf("Get = %v, %v, want no log and no error", ok, err)
	}
	if _, ok, err := c.Get("*"); ok || err == nil {
		t.Errorf("Get(*) = %v, %v, want an error", ok, err)
	}
}

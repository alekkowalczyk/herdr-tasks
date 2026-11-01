package claudehook

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"slices"
	"strings"
	"syscall"
	"time"
)

// SettingsPath is Claude Code's user settings file.
func SettingsPath() string {
	if d := os.Getenv("CLAUDE_CONFIG_DIR"); d != "" {
		return filepath.Join(d, "settings.json")
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".claude", "settings.json")
}

// Command is the hook's command line for the herdr-tasks binary bin. Claude
// Code runs it with sh, so bin is double-quoted with \ " ` $ escaped; only a
// leading $HOME/ is left to expand.
func Command(bin string) string {
	prefix := ""
	if rest, ok := strings.CutPrefix(bin, "$HOME/"); ok {
		prefix, bin = "$HOME/", rest
	}
	return `"` + prefix + strings.NewReplacer(`\`, `\\`, `"`, `\"`, "`", "\\`", "$", `\$`).Replace(bin) + `" claude-hook`
}

// ourCommand matches the command of a hook herdr-tasks added. Handlers have
// no id field, so the command is the only mark: one shell word, bare or
// quoted, naming a file called herdr-tasks, then claude-hook.
var ourCommand = regexp.MustCompile(`^\s*(?:"(?:(?:[^"\\]|\\.)*/)?herdr-tasks"|'(?:[^']*/)?herdr-tasks'|(?:[^\s"'\\]*/)?herdr-tasks)\s+claude-hook\s*$`)

// A ParseError means the settings aren't strict JSON, or their hooks aren't
// shaped the way Claude Code reads them. They are left alone.
type ParseError struct{ Err error }

func (e *ParseError) Error() string { return e.Err.Error() }
func (e *ParseError) Unwrap() error { return e.Err }

type handler struct {
	Type    string `json:"type"`
	Command string `json:"command"`
	Timeout int    `json:"timeout"`
	Async   bool   `json:"async"`
}

type group struct {
	Matcher string    `json:"matcher,omitempty"`
	Hooks   []handler `json:"hooks"`
}

// events are the hook events Apply adds our hook to: tool calls, your
// prompts, and Claude's replies.
var events = []string{"PostToolUse", "UserPromptSubmit", "Stop"}

// Apply adds our hook to Claude Code settings, or brings an old one up to
// date. PostToolUse runs it for the tools Matcher picks; UserPromptSubmit and
// Stop have no matcher. command is the hook's command line, from Command.
// Only the "hooks" value is rewritten; it returns src unchanged when the hook
// is already there.
func Apply(src []byte, command string) ([]byte, error) {
	hs := []handler{{"command", command, 10, true}}
	return editHooks(src, events, func(event string, groups []json.RawMessage) ([]json.RawMessage, bool) {
		want := marshal(group{Hooks: hs})
		if event == "PostToolUse" {
			want = marshal(group{Matcher, hs})
		}
		if i := slices.IndexFunc(groups, func(g json.RawMessage) bool { return sameJSON(g, want) }); i >= 0 {
			if _, more := without(slices.Delete(slices.Clone(groups), i, i+1), command); !more {
				return groups, false
			}
		}
		groups, _ = without(groups, command)
		return append(groups, want), true
	})
}

// Remove takes our hook out of every event in Claude Code settings, with
// groups and events it leaves empty, and "hooks" when it ends up empty.
func Remove(src []byte) ([]byte, error) {
	return editHooks(src, nil, func(_ string, groups []json.RawMessage) ([]json.RawMessage, bool) {
		return without(groups, "")
	})
}

// without takes our handlers out of an event's groups, dropping groups that
// held only ours. A handler is ours when its command looks like ours or is
// command. It reports whether it took anything out.
func without(groups []json.RawMessage, command string) ([]json.RawMessage, bool) {
	var keep []json.RawMessage
	took := false
	for _, g := range groups {
		fields, _, _, err := parseObject(g)
		hi := find(fields, "hooks")
		var hs []json.RawMessage
		if err != nil || hi < 0 || json.Unmarshal(fields[hi].val, &hs) != nil {
			keep = append(keep, g)
			continue
		}
		rest := slices.DeleteFunc(slices.Clone(hs), func(h json.RawMessage) bool {
			var c struct {
				Command string `json:"command"`
			}
			return json.Unmarshal(h, &c) == nil && (ourCommand.MatchString(c.Command) || command != "" && c.Command == command)
		})
		switch {
		case len(rest) == len(hs):
			keep = append(keep, g)
		case len(rest) > 0:
			fields[hi].val = list(rest)
			keep = append(keep, render(fields))
			took = true
		default:
			took = true
		}
	}
	return keep, took
}

// editHooks runs fn on the groups of each of names in src (nil for an event
// that isn't there), or of every event when names is nil, and splices the
// result back in place of the "hooks" value. Events left with no groups are
// taken out. fn reports whether it changed anything.
func editHooks(src []byte, names []string, fn func(event string, groups []json.RawMessage) ([]json.RawMessage, bool)) ([]byte, error) {
	doc := src
	if len(bytes.TrimSpace(doc)) == 0 {
		doc = []byte("{}\n")
	}
	top, open, close, err := parseObject(doc)
	if err != nil {
		return nil, &ParseError{err}
	}
	hi := find(top, "hooks")
	var hooks []field
	if hi >= 0 {
		if hooks, _, _, err = parseObject(top[hi].val); err != nil {
			return nil, &ParseError{fmt.Errorf("hooks: %w", err)}
		}
	}
	for _, h := range hooks {
		var groups []json.RawMessage
		if json.Unmarshal(h.val, &groups) != nil {
			return nil, &ParseError{fmt.Errorf("hooks.%s isn't a list", h.key)}
		}
	}
	if names == nil {
		for _, h := range hooks {
			names = append(names, h.key)
		}
	}
	changed := false
	for _, name := range names {
		i := find(hooks, name)
		var groups []json.RawMessage
		if i >= 0 {
			_ = json.Unmarshal(hooks[i].val, &groups) // checked above
		}
		groups, did := fn(name, groups)
		if !did {
			continue
		}
		changed = true
		switch {
		case len(groups) > 0 && i >= 0:
			hooks[i].val = list(groups)
		case len(groups) > 0:
			hooks = append(hooks, field{key: name, val: list(groups)})
		case i >= 0:
			hooks = slices.Delete(hooks, i, i+1)
		}
	}
	if !changed {
		return src, nil
	}

	switch {
	case hi >= 0 && len(hooks) > 0:
		return concat(doc[:top[hi].start], indent(render(hooks)), doc[top[hi].end:]), nil
	case hi >= 0:
		// Take the key out, with the comma that separates it.
		from, to := open, close
		switch {
		case hi > 0:
			from, to = top[hi-1].end, top[hi].end
		case len(top) > 1:
			to = top[0].end + int64(bytes.IndexByte(doc[top[0].end:], ',')) + 1
		}
		return concat(doc[:from], doc[to:]), nil
	case len(hooks) == 0:
		return src, nil
	case len(top) == 0:
		return concat(doc[:open], []byte("\n  \"hooks\": "), indent(render(hooks)), []byte("\n"), doc[close:]), nil
	default:
		// After the last key.
		at := top[len(top)-1].end
		return concat(doc[:at], []byte(",\n  \"hooks\": "), indent(render(hooks)), doc[at:]), nil
	}
}

// field is one member of a JSON object, with its value's place in the source.
type field struct {
	key        string
	val        json.RawMessage
	start, end int64
}

// parseObject reads a JSON object's members in order. open is the offset
// just after its "{", and close the offset of its "}".
func parseObject(src []byte) (fields []field, open, close int64, err error) {
	dec := json.NewDecoder(bytes.NewReader(src))
	tok, err := dec.Token()
	if err != nil {
		return nil, 0, 0, err
	}
	if d, ok := tok.(json.Delim); !ok || d != '{' {
		return nil, 0, 0, errors.New("not a JSON object")
	}
	open = dec.InputOffset()
	for dec.More() {
		tok, err := dec.Token()
		if err != nil {
			return nil, 0, 0, err
		}
		key, _ := tok.(string)
		var val json.RawMessage
		if err := dec.Decode(&val); err != nil {
			return nil, 0, 0, err
		}
		end := dec.InputOffset()
		fields = append(fields, field{key: key, val: val, start: end - int64(len(val)), end: end})
	}
	if _, err := dec.Token(); err != nil {
		return nil, 0, 0, err
	}
	close = dec.InputOffset() - 1
	if _, err := dec.Token(); err != io.EOF {
		return nil, 0, 0, errors.New("more after the closing }")
	}
	return fields, open, close, nil
}

func find(fields []field, key string) int {
	return slices.IndexFunc(fields, func(f field) bool { return f.key == key })
}

// render writes an object back as compact JSON, keeping each value's bytes.
func render(fields []field) []byte {
	b := []byte{'{'}
	for i, f := range fields {
		if i > 0 {
			b = append(b, ',')
		}
		b = append(append(append(b, marshal(f.key)...), ':'), f.val...)
	}
	return append(b, '}')
}

// list writes values as a JSON array, keeping their bytes.
func list(vals []json.RawMessage) []byte {
	b := []byte{'['}
	for i, v := range vals {
		if i > 0 {
			b = append(b, ',')
		}
		b = append(b, v...)
	}
	return append(b, ']')
}

// indent lays out a value of a top-level key the way Claude Code writes its
// settings: two spaces per level.
func indent(v []byte) []byte {
	var b bytes.Buffer
	if err := json.Indent(&b, v, "  ", "  "); err != nil {
		return v
	}
	return b.Bytes()
}

// marshal is json.Marshal without HTML escaping, so & < > stay as they are.
func marshal(v any) []byte {
	var b bytes.Buffer
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	_ = enc.Encode(v)
	return bytes.TrimSuffix(b.Bytes(), []byte("\n"))
}

func sameJSON(a, b []byte) bool {
	var x, y any
	return json.Unmarshal(a, &x) == nil && json.Unmarshal(b, &y) == nil && reflect.DeepEqual(x, y)
}

func concat(parts ...[]byte) []byte {
	var out []byte
	for _, p := range parts {
		out = append(out, p...)
	}
	return out
}

// beforeReplace runs just before the edited file takes the original's place,
// after the last read of it. Tests use it to change the file underneath.
var beforeReplace = func() {}

// settle waits a moment for an editor that has emptied the settings file to
// finish saving it. Tests replace it.
var settle = func() { time.Sleep(50 * time.Millisecond) }

// justEmptied is how recently a file that is still empty after settle must
// have been changed for a save to be probably under way.
const justEmptied = 2 * time.Second

// swap exchanges the files at two paths atomically. Tests replace it.
var swap = swapFiles

// errNoSwap means the filesystem can't exchange two files atomically.
var errNoSwap = errors.New("this filesystem can't swap files")

// WriteSettings edits the settings file without losing a change made while
// it works, as far as it can tell. It follows symlinks (editing the target),
// serializes our own writers with a flock on <lockDir>/claude-settings.lock,
// writes a unique temp file in the target's directory, keeps the file mode,
// and makes a one-time backup <target>.before-herdr-tasks. It swaps the new
// file in atomically and, when what it displaced isn't what it read, swaps
// that back and refuses (error); when the file is saved again meanwhile, the
// newest version stays in place and the one before it is kept next to it.
// It creates a file only if none appeared meanwhile, and refuses a file
// that is still empty a moment after it was emptied, as a save is probably
// under way. This is best effort: a writer that keeps the file open across
// the whole edit and writes in place can't be caught. It returns
// changed=false and writes nothing when edit returns the same bytes, or when
// the file doesn't exist and create is false.
func WriteSettings(path, lockDir string, create bool, edit func([]byte) ([]byte, error)) (changed bool, err error) {
	target, err := filepath.EvalSymlinks(path)
	if errors.Is(err, os.ErrNotExist) {
		if !create {
			return false, nil
		}
		if _, err := os.Lstat(path); err == nil {
			return false, fmt.Errorf("%s links to a file that doesn't exist, so the Claude links hook wasn't changed", path)
		}
		target = path
	} else if err != nil {
		return false, err
	}

	if err := os.MkdirAll(lockDir, 0o755); err != nil {
		return false, err
	}
	lock, err := os.OpenFile(filepath.Join(lockDir, "claude-settings.lock"), os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		return false, err
	}
	defer lock.Close() // closing it lets go of the lock
	if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX); err != nil {
		return false, err
	}

	mode := os.FileMode(0o644)
	fi, err := os.Stat(target)
	existed := err == nil
	switch {
	case existed:
		mode = fi.Mode().Perm()
	case !errors.Is(err, os.ErrNotExist):
		return false, err
	case !create:
		return false, nil
	}
	conflict := fmt.Errorf("%s changed while herdr-tasks was editing it, so the Claude links hook wasn't changed; try again", target)
	var before []byte
	if existed {
		if before, err = os.ReadFile(target); err != nil {
			return false, err
		}
		if len(before) == 0 {
			// Usually an editor that has emptied the file to save it.
			settle()
			if fi, err = os.Stat(target); err != nil {
				return false, err
			}
			if before, err = os.ReadFile(target); err != nil {
				return false, err
			}
		}
	}
	after, err := edit(before)
	if err != nil || bytes.Equal(after, before) {
		return false, err
	}
	// Still empty, and emptied just now: a save that's slow to finish would
	// write into the file the swap displaces, which looks like what was read.
	if existed && len(before) == 0 && time.Since(fi.ModTime()).Abs() < justEmptied {
		return false, conflict
	}

	dir := filepath.Dir(target)
	if existed {
		if err := backup(target+".before-herdr-tasks", before, mode); err != nil {
			return false, err
		}
	} else if err := os.MkdirAll(dir, 0o755); err != nil {
		return false, err
	}
	tmp, err := os.CreateTemp(dir, "."+filepath.Base(target)+".herdr-tasks-*")
	if err != nil {
		return false, err
	}
	keep := false
	defer func() {
		if !keep {
			os.Remove(tmp.Name()) // the old file once swapped, gone once renamed
		}
	}()
	_, err = tmp.Write(after)
	if err == nil {
		err = tmp.Chmod(mode)
	}
	if cerr := tmp.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return false, err
	}

	beforeReplace()
	if !existed {
		// A hard link makes the file only if none appeared meanwhile.
		err := os.Link(tmp.Name(), target)
		if errors.Is(err, os.ErrExist) {
			return false, conflict
		}
		if err == nil {
			return true, nil
		}
		// No hard links here: check, then rename.
		if _, err := os.Lstat(target); !errors.Is(err, os.ErrNotExist) {
			return false, conflict
		}
		return rename(tmp.Name(), target)
	}

	err = swap(tmp.Name(), target)
	if errors.Is(err, errNoSwap) {
		now, err := os.ReadFile(target)
		if err != nil || !bytes.Equal(now, before) {
			return false, conflict
		}
		return rename(tmp.Name(), target)
	}
	if err != nil {
		return false, err
	}
	// The temp file now holds what the target held at the swap.
	if now, err := os.ReadFile(tmp.Name()); err == nil && bytes.Equal(now, before) {
		return true, nil
	}
	// Someone else wrote it: put their version back. What that displaces is
	// deleted only while it's ours: someone may have saved again meanwhile.
	twice := func(newerInPlace bool) error {
		if newerInPlace {
			return fmt.Errorf("%s changed twice while herdr-tasks was editing it, so the Claude links hook wasn't changed; the newer version is in place, and the earlier one is in %s", target, tmp.Name())
		}
		return fmt.Errorf("%s changed twice while herdr-tasks was editing it, so the Claude links hook wasn't changed; the earlier version is in place, and the newer one is in %s", target, tmp.Name())
	}
	if swap(tmp.Name(), target) != nil {
		now, err := os.ReadFile(target)
		switch {
		case err == nil && !bytes.Equal(now, after):
			keep = true
			return false, twice(true)
		case err == nil && os.Rename(tmp.Name(), target) == nil:
			return false, conflict
		}
		keep = true
		return false, fmt.Errorf("%s changed while herdr-tasks was editing it, and its new version couldn't be put back; it's in %s", target, tmp.Name())
	}
	// The temp file now holds what the target held at the swap back: ours,
	// unless it was saved again. Then the newest version goes back in place.
	now, err := os.ReadFile(tmp.Name())
	switch {
	case err == nil && bytes.Equal(now, after):
		return false, conflict
	case err == nil && swap(tmp.Name(), target) == nil:
		keep = true
		return false, twice(true)
	}
	keep = true
	return false, twice(false)
}

// rename moves the temp file into place, for WriteSettings's results.
func rename(tmp, target string) (changed bool, err error) {
	if err := os.Rename(tmp, target); err != nil {
		return false, err
	}
	return true, nil
}

// backup saves the original settings once; later edits keep the first copy.
func backup(path string, b []byte, mode os.FileMode) error {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, mode)
	if errors.Is(err, os.ErrExist) {
		return nil
	}
	if err != nil {
		return err
	}
	_, err = f.Write(b)
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		os.Remove(path)
	}
	return err
}

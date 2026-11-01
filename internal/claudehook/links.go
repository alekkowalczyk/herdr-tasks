package claudehook

import (
	"bytes"
	"encoding/json"
	"errors"
	"html"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strings"
	"time"
	"unicode"

	"github.com/alekkowalczyk/herdr-tasks/internal/slack"
	"github.com/alekkowalczyk/herdr-tasks/internal/task"
)

// Link is a link a hook call found. Label is "" when the page has no title.
// Source is the local file an artifact was published from.
type Link struct{ URL, Label, Source string }

// MaxLinks caps the links one hook call finds, so a pasted log can't flood a
// task.
const MaxLinks = 20

// Links stops after maxProbes filesystem calls, or once budget has passed,
// and returns what it found, so the hook has time left to save it.
var (
	maxProbes = 200
	budget    = 2 * time.Second
)

const (
	// maxRead is the most Links reads of a file: enough for its <title>.
	maxRead = 64 << 10
	// headLen is how much of a page must show it's a whole page, and not a
	// template.
	headLen = 4 << 10
	// maxToken is the longest URL or path Links looks at.
	maxToken = 4 << 10
)

// The filesystem calls Links makes, one probe each. Tests count them.
var (
	statFile  = os.Stat
	readStart = func(path string) ([]byte, error) {
		f, err := os.Open(path)
		if err != nil {
			return nil, err
		}
		defer f.Close()
		return io.ReadAll(io.LimitReader(f, maxRead))
	}
	// readChannels reads the Slack channels' names, also one probe.
	readChannels = slack.Channels
)

var (
	slackSend   = regexp.MustCompile(`^mcp__.*[Ss]lack.*__slack_send_message$`)
	slackSearch = regexp.MustCompile(`^mcp__.*[Ss]lack.*__slack_search_(public|public_and_private)$`)
	navigate    = regexp.MustCompile(`^mcp__.*__(browser_)?navigate$`)
)

// Links returns the links in a hook call, in the order found, one per
// task.LinkKey, at most MaxLinks, within its probe and time budget.
//   - A prompt you wrote gives Slack links, Linear issues and pages.
//   - A reply gives pages only: Slack digests reply with lists of permalinks.
//   - A tool call gives the artifact it published, the page it wrote, the
//     Slack message it sent, or the local page it opened.
//
// A Slack link is labelled with its channel's name when the state folder
// dir has seen it ("" has seen none); a message Claude sent also says who
// sent it and its first words.
func Links(p Payload, dir string) []Link {
	f := &finder{
		cwd:      p.Cwd,
		dir:      dir,
		deadline: time.Now().Add(budget),
		seen:     map[string]bool{},
		tried:    map[string]bool{},
	}
	switch p.HookEventName {
	case "UserPromptSubmit":
		// Loop wake-ups and system prompts aren't yours.
		if p.Source == "user" || p.Source == "sdk" || p.Source == "" {
			f.text(p.Prompt, true)
		}
	case "Stop":
		f.text(p.LastAssistantMessage, false)
	case "PostToolUse", "":
		f.tool(p)
	}
	return f.links
}

// finder collects the links of one hook call.
type finder struct {
	cwd      string
	dir      string // the state folder, for Slack channels' names
	deadline time.Time
	probes   int
	out      bool              // out of probes or time
	seen     map[string]bool   // keys of the links found
	tried    map[string]bool   // paths of the files looked at
	channels map[string]string // Slack channels' names; nil until read
	links    []Link
}

// done reports whether Links should stop looking.
func (f *finder) done() bool { return f.out || len(f.links) >= MaxLinks }

// add keeps l unless its URL isn't safe or its page was found already.
func (f *finder) add(l Link) bool {
	if f.done() || !task.SafeURL(l.URL) {
		return false
	}
	k := task.LinkKey(l.URL)
	if f.seen[k] {
		return false
	}
	f.seen[k] = true
	f.links = append(f.links, l)
	return true
}

// probe reports whether one more filesystem call fits the budget, and
// counts it.
func (f *finder) probe() bool {
	if f.probes >= maxProbes || time.Now().After(f.deadline) {
		f.out = true
		return false
	}
	f.probes++
	return true
}

// errOut is a probe's error once the budget is spent.
var errOut = errors.New("out of probes or time")

// within runs the filesystem call fn as one probe, and gives up on it at the
// deadline. A call on a hung mount can block far longer than the budget; the
// hook's process exits soon after, so the call is left to finish on its own.
func within[T any](f *finder, fn func() (T, error)) (T, error) {
	var zero T
	if !f.probe() {
		return zero, errOut
	}
	type result struct {
		v   T
		err error
	}
	// Buffered, so a call that finishes late doesn't block forever.
	ch := make(chan result, 1)
	go func() {
		v, err := fn()
		ch <- result{v, err}
	}()
	timer := time.NewTimer(time.Until(f.deadline))
	defer timer.Stop()
	select {
	case r := <-ch:
		return r.v, r.err
	case <-timer.C:
		// A hung mount would hang the next probes too.
		f.out = true
		return zero, errOut
	}
}

// channel is the name of the Slack channel id, "#billing", or "" when it
// isn't known. The names are read once, as one probe.
func (f *finder) channel(id string) string {
	if f.dir == "" || id == "" {
		return ""
	}
	if f.channels == nil {
		read := readChannels
		names, err := within(f, func() (map[string]string, error) { return read(f.dir) })
		if err != nil || names == nil {
			names = map[string]string{}
		}
		f.channels = names
	}
	return task.CleanLabel(f.channels[id])
}

// addSlack adds a Slack link, then labels it: a read of the channels' names
// that runs out the budget keeps the link.
func (f *finder) addSlack(u string, label func() string) {
	if f.add(Link{URL: u}) {
		f.links[len(f.links)-1].Label = label()
	}
}

// regular reports whether path is a regular file, following symlinks.
func (f *finder) regular(path string) bool {
	stat := statFile // read here: a call left running mustn't see a later one
	fi, err := within(f, func() (os.FileInfo, error) { return stat(path) })
	return err == nil && fi.Mode().IsRegular()
}

// read returns the start of the file at path.
func (f *finder) read(path string) ([]byte, bool) {
	read := readStart
	b, err := within(f, func() ([]byte, error) { return read(path) })
	return b, err == nil
}

// abs makes a path absolute: ~/ is the home directory, and a relative path is
// under the session's directory. It's "" when it can't be.
func (f *finder) abs(p string) string {
	switch {
	case p == "":
		return ""
	case strings.HasPrefix(p, "~/"):
		home, err := os.UserHomeDir()
		if err != nil || !filepath.IsAbs(home) {
			return ""
		}
		return filepath.Join(home, p[2:])
	case filepath.IsAbs(p):
		return filepath.Clean(p)
	case filepath.IsAbs(f.cwd):
		return filepath.Join(f.cwd, p)
	}
	return ""
}

// text adds the links in a prompt you wrote (yours) or in a reply. A path in
// a reply is an implicit page: replies name the files Claude edited, and
// those are ordinary coding unless they pass a Write's checks.
func (f *finder) text(s string, yours bool) {
	covered := 0
	for _, c := range candidates(s) {
		if f.done() || time.Now().After(f.deadline) {
			return
		}
		switch {
		case c.url:
			f.url(c.text, yours)
		case c.at < covered:
			// Part of a path with spaces that was found whole.
		case f.page(f.abs(c.text), !yours, nil):
			covered = c.end
		}
	}
}

// url adds a URL from text when it's a page, or from your prompt also when
// it's a Slack link or a Linear issue. Other web links don't count.
func (f *finder) url(raw string, yours bool) {
	switch task.KindOf(raw) {
	case task.KindSlack:
		if u, ok := slackURL(raw); ok && yours {
			f.addSlack(u, func() string { return f.channel(slackChannel(u)) })
		}
	case task.KindLinear:
		if yours {
			f.add(Link{URL: linearURL(raw)})
		}
	case task.KindArtifact:
		f.add(Link{URL: artifactURL(raw)})
	case task.KindHosted:
		if strings.HasPrefix(strings.ToLower(raw), "https://") && !secret(raw) {
			f.add(Link{URL: raw})
		}
	case task.KindLocal:
		if !secret(raw) {
			f.add(Link{URL: raw})
		}
	case task.KindFile:
		path, _ := task.FilePath(raw)
		f.page(path, false, nil)
	}
}

// tool adds what a tool call published, wrote, sent or opened.
func (f *finder) tool(p Payload) {
	switch name := p.ToolName; {
	case name == "Artifact":
		f.artifact(p.ToolInput, p.ToolResponse)
	case name == "Write" || name == "Edit" || name == "MultiEdit":
		// A subagent's files are its working material.
		if p.AgentID != "" {
			return
		}
		var in struct {
			FilePath string `json:"file_path"`
			Content  string `json:"content"`
		}
		if json.Unmarshal(p.ToolInput, &in) != nil {
			return
		}
		var written []byte // nil reads the file
		if name == "Write" {
			if in.Content == "" {
				return
			}
			written = []byte(in.Content)
		}
		f.page(f.abs(in.FilePath), true, written)
	case slackSend.MatchString(name):
		f.slackSent(p.ToolInput, p.ToolResponse)
	case navigate.MatchString(name):
		var in struct {
			URL string `json:"url"`
		}
		_ = json.Unmarshal(p.ToolInput, &in)
		if in.URL != "" && !strings.Contains(in.URL, "://") {
			// Chrome's navigate takes "localhost:4100/x" as the address bar does.
			in.URL = "http://" + in.URL
		}
		switch task.KindOf(in.URL) {
		case task.KindLocal:
			if !secret(in.URL) {
				f.add(Link{URL: in.URL})
			}
		case task.KindFile:
			path, _ := task.FilePath(in.URL)
			f.page(path, false, nil)
		}
	}
}

// artifactLink is an artifact's URL in a publish call's result.
var artifactLink = regexp.MustCompile(`https://claude\.ai/(?:code/)?artifact/[A-Za-z0-9_-]+`)

// artifact adds the artifact a publish call made, labeled with the title of
// the file it was published from, else the title it was given.
func (f *finder) artifact(input, response json.RawMessage) {
	var in struct {
		Action   string `json:"action"`
		Asset    bool   `json:"asset"`
		FilePath string `json:"file_path"`
		Title    string `json:"title"`
	}
	// Reads, listings and asset uploads publish no page.
	if json.Unmarshal(input, &in) != nil || in.Action != "" && in.Action != "publish" || in.Asset {
		return
	}
	u := artifactLink.Find(response)
	if u == nil {
		return
	}
	l := Link{URL: string(u)}
	if src := f.abs(in.FilePath); src != "" && task.SafeURL(task.FileURL(src)) {
		l.Source = src
		if htmlFile(src) && f.regular(src) {
			if b, ok := f.read(src); ok {
				l.Label = title(b)
			}
		}
	}
	if l.Label == "" {
		l.Label = task.CleanLabel(in.Title)
	}
	f.add(l)
}

// slackSent adds the message a Slack send call posted, labelled with its
// channel's name, "you", and its first words. Its result is MCP content
// whose text is JSON with the message's link. Its input has the channel's
// ID, and the text as message, or else text; the channel's ID in the link
// stands in for a missing one.
func (f *finder) slackSent(input, response json.RawMessage) {
	var content []struct {
		Text string `json:"text"`
	}
	if json.Unmarshal(response, &content) != nil {
		return
	}
	// Fields that don't decode stay empty.
	var in struct {
		ChannelID string `json:"channel_id"`
		Message   string `json:"message"`
		Text      string `json:"text"`
	}
	_ = json.Unmarshal(input, &in)
	text := in.Message
	if text == "" {
		text = in.Text
	}
	for _, c := range content {
		var res struct {
			MessageLink string `json:"message_link"`
		}
		if json.Unmarshal([]byte(c.Text), &res) != nil {
			continue
		}
		if u, ok := slackURL(res.MessageLink); ok {
			f.addSlack(u, func() string {
				id := in.ChannelID
				if id == "" {
					id = slackChannel(u)
				}
				return slack.LinkLabel(f.channel(id), "you", text)
			})
		}
	}
}

// SearchLabel is the label a Slack search gives a message's link, and Place,
// the label with only where it is, "#billing", that it may take the place
// of: a link from your prompt gets that.
type SearchLabel struct{ Label, Place string }

// SearchLabels returns the labels a Slack search's results give the
// messages they found, by SlackMessage, and the names of their channels, as
// slack.LearnChannels saves them for later links. A search adds no links.
// Another call, or a result that isn't one, gives nothing.
func SearchLabels(p Payload) (labels map[string]SearchLabel, channels map[string]string) {
	if p.HookEventName != "" && p.HookEventName != "PostToolUse" || !slackSearch.MatchString(p.ToolName) {
		return nil, nil
	}
	// MCP content whose text is a JSON object holding the results' markdown.
	var content []struct {
		Text string `json:"text"`
	}
	if json.Unmarshal(p.ToolResponse, &content) != nil {
		return nil, nil
	}
	var text strings.Builder
	for _, c := range content {
		text.WriteString(c.Text)
	}
	var body struct {
		Results string `json:"results"`
	}
	if json.Unmarshal([]byte(text.String()), &body) != nil {
		return nil, nil
	}
	msgs, _, err := slack.ParseResults(body.Results)
	if err != nil || len(msgs) == 0 {
		return nil, nil
	}
	labels = map[string]SearchLabel{}
	for _, m := range msgs {
		if key, ok := messageKey("/archives/" + m.Channel + "/p" + strings.Replace(m.TS, ".", "", 1)); ok {
			labels[key] = SearchLabel{slack.MessageLabel(m), slack.PlaceLabel(m)}
		}
	}
	return labels, slack.ChannelNames(msgs)
}

// page adds the HTML page at path, which must be absolute, and reports
// whether it did. Implicit pages, which Claude wrote or edited or named by
// path in a reply, must also not look like source or a template. Explicit
// pages are paths in your prompt, file:// URLs, and pages Claude navigated
// to. written is what Claude wrote; nil reads the file.
func (f *finder) page(path string, implicit bool, written []byte) bool {
	if !filepath.IsAbs(path) || !htmlFile(path) {
		return false
	}
	path = filepath.Clean(path)
	if f.tried[path] || implicit && sourceLike(path) {
		return false
	}
	f.tried[path] = true
	if !f.regular(path) {
		return false
	}
	b := written
	if b == nil {
		var ok bool
		if b, ok = f.read(path); !ok {
			return false
		}
	}
	head := bytes.ToLower(b[:min(len(b), headLen)])
	if !bytes.Contains(head, []byte("<!doctype html")) && !bytes.Contains(head, []byte("<html")) {
		return false
	}
	if implicit && slices.ContainsFunc(templateMarks, func(m string) bool { return bytes.Contains(head, []byte(m)) }) {
		return false
	}
	return f.add(Link{URL: task.FileURL(path), Label: title(b)})
}

// htmlFile reports whether path ends in .html or .htm, in any case.
func htmlFile(path string) bool {
	ext := strings.ToLower(filepath.Ext(path))
	return ext == ".html" || ext == ".htm"
}

// sourceDirs hold source, build output or dependencies: a page Claude writes
// there is ordinary coding.
var sourceDirs = map[string]bool{
	"src": true, "templates": true, "template": true, "static": true, "node_modules": true, "vendor": true,
	"dist": true, "build": true, "public": true, "wwwroot": true, ".git": true, ".next": true, "target": true,
}

// templateMarks are template syntax: Django and Jinja, Go and Handlebars,
// ERB and EJS, PHP, and Thymeleaf.
var templateMarks = []string{"{%", "{{", "<%", "<?php", " th:"}

// sourceLike reports whether path is named like a template, or sits in a
// source, build or dependency folder.
func sourceLike(path string) bool {
	base := strings.ToLower(filepath.Base(path))
	if strings.Contains(base, "template") || strings.Contains(base, ".tpl.") {
		return true
	}
	return slices.ContainsFunc(strings.Split(filepath.Dir(path), string(filepath.Separator)), func(dir string) bool {
		return sourceDirs[strings.ToLower(dir)]
	})
}

var titleTag = regexp.MustCompile(`(?is)<title(?:\s[^>]*)?>(.*?)</title\s*>`)

// title is the first <title> in a page's first 64 KiB, as clean text.
func title(b []byte) string {
	m := titleTag.FindSubmatch(b[:min(len(b), maxRead)])
	if m == nil {
		return ""
	}
	return task.CleanLabel(html.UnescapeString(string(m[1])))
}

// slackHost and slackPath are a Slack workspace, and a channel or a message
// in it: /archives/<ID>[/p<16 digits>].
var (
	slackHost = regexp.MustCompile(`^([a-z0-9-]+\.)+slack\.com$`)
	slackPath = regexp.MustCompile(`^/archives/([A-Za-z0-9]+)(/p\d{16})?/?$`)
)

// SlackMessage names the Slack message a link opens, "C0123ABCD/p<16
// digits>", in any workspace, whatever its query. It's false for a channel
// and for other links.
func SlackMessage(raw string) (string, bool) {
	if task.KindOf(raw) != task.KindSlack {
		return "", false
	}
	u, _ := url.Parse(raw) // KindOf parsed it already.
	return messageKey(u.EscapedPath())
}

// messageKey is SlackMessage of a link's path.
func messageKey(path string) (string, bool) {
	m := slackPath.FindStringSubmatch(path)
	if m == nil || m[2] == "" {
		return "", false
	}
	return m[1] + m[2], true
}

// slackChannel is the channel's ID in a link slackURL made.
func slackChannel(u string) string {
	p, err := url.Parse(u)
	if err != nil {
		return ""
	}
	if m := slackPath.FindStringSubmatch(p.EscapedPath()); m != nil {
		return m[1]
	}
	return ""
}

// slackURL is a Slack link to a channel or a message, with only the query
// parameters that open a thread reply: thread_ts and cid.
func slackURL(raw string) (string, bool) {
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "https" || u.User != nil {
		return "", false
	}
	host := strings.ToLower(u.Host)
	if !slackHost.MatchString(host) || !slackPath.MatchString(u.EscapedPath()) {
		return "", false
	}
	s := "https://" + host + strings.TrimSuffix(u.EscapedPath(), "/")
	if q := keepParams(u.RawQuery, "thread_ts", "cid"); q != "" {
		s += "?" + q
	}
	return s, true
}

// artifactURL is an artifact link with only its share key, sk, and no user
// name or password.
func artifactURL(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return raw
	}
	u.User = nil
	u.RawQuery = keepParams(u.RawQuery, "sk")
	u.Fragment, u.RawFragment = "", ""
	return u.String()
}

// linearURL is a Linear issue link with only its issue and slug: no user
// name or password, query or fragment.
func linearURL(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return raw
	}
	u.User = nil
	u.RawQuery, u.ForceQuery = "", false
	u.Fragment, u.RawFragment = "", ""
	return u.String()
}

// keepParams is a raw query with only the named parameters, in their order.
func keepParams(query string, names ...string) string {
	var kept []string
	for _, kv := range strings.Split(query, "&") {
		if name, _, _ := strings.Cut(kv, "="); slices.Contains(names, name) {
			kept = append(kept, kv)
		}
	}
	return strings.Join(kept, "&")
}

// secretWords in a parameter's name mean it may carry a credential.
var secretWords = []string{"token", "code", "key", "secret", "pass", "auth", "sig", "session", "jwt"}

// secret reports whether a local or hosted link may carry a credential: a
// user name or password, a token-like parameter in its query or fragment, or
// an OAuth callback path or hash route. A link that doesn't parse counts as
// one.
func secret(raw string) bool {
	u, err := url.Parse(raw)
	if err != nil || u.User != nil {
		return true
	}
	if callback(u.Path) || secretParams(u.RawQuery) {
		return true
	}
	// Split escaped, so an encoded ? & or = isn't a separator.
	frag := u.EscapedFragment()
	// #/route?a=1 is a route, then parameters. A router reads the route
	// percent-decoded, as u.Path is.
	if route, params, ok := strings.Cut(frag, "?"); ok && !strings.Contains(route, "=") {
		return callback(unescapePath(route)) || secretParams(params)
	}
	// #access_token=… is all parameters, as it holds an "=".
	if strings.Contains(u.Fragment, "=") {
		return secretParams(frag)
	}
	return callback(u.Fragment)
}

// unescapePath is p percent-decoded, or p itself when it isn't valid.
func unescapePath(p string) string {
	if s, err := url.PathUnescape(p); err == nil {
		return s
	}
	return p
}

// callback reports whether a path or hash route looks like an OAuth
// callback.
func callback(p string) bool {
	p = strings.ToLower(p)
	return strings.Contains(p, "/callback") || strings.Contains(p, "/oauth")
}

// secretParams reports whether a query, or a fragment, has a parameter whose
// percent-decoded name holds a secretWord.
func secretParams(s string) bool {
	for _, kv := range strings.FieldsFunc(s, func(r rune) bool { return r == '&' || r == ';' || r == '?' }) {
		name, _, _ := strings.Cut(kv, "=")
		if n, err := url.QueryUnescape(name); err == nil {
			name = n
		}
		name = strings.ToLower(name)
		if slices.ContainsFunc(secretWords, func(w string) bool { return strings.Contains(name, w) }) {
			return true
		}
	}
	return false
}

// A candidate is a URL, or a path to an .html file, at s[at:end] in text.
type candidate struct {
	at, end int
	text    string
	url     bool
}

// webURL is a web or file URL in text, with any punctuation after it, which
// trim takes off.
var webURL = regexp.MustCompile("(?i)\\b(?:https?|file)://[^\\s\\p{Z}<>\"`]+")

// candidates returns the URLs and .html paths in s, in the order they appear.
func candidates(s string) []candidate {
	var urls []candidate
	for _, m := range webURL.FindAllStringIndex(s, -1) {
		if m[1]-m[0] <= maxToken {
			urls = append(urls, candidate{m[0], m[1], trim(s[m[0]:m[1]]), true})
		}
	}
	cs := slices.Clone(urls)
	for _, c := range paths(s) {
		// A path inside a URL is part of the URL. URLs don't overlap, so only
		// the last one starting before c ends can hold it.
		i := sort.Search(len(urls), func(i int) bool { return urls[i].at >= c.end })
		if i == 0 || urls[i-1].end <= c.at {
			cs = append(cs, c)
		}
	}
	slices.SortStableFunc(cs, func(a, b candidate) int { return a.at - b.at })
	return cs
}

// paths returns the path-shaped .html tokens in s. Tokens are split on
// whitespace and brackets; a quoted, backticked or Markdown link span with
// spaces in it is also tried whole, before the tokens in it.
func paths(s string) []candidate {
	var cs []candidate
	keep := func(at, end int, spaced bool) {
		if end-at > maxToken {
			return
		}
		tok := strings.TrimSpace(s[at:end])
		if !spaced {
			tok = strings.TrimLeft(tok, "(*'")
		} else if words := strings.Fields(tok); len(words) == 0 || !strings.Contains(words[0], "/") {
			// "open ./a.html" is a command, not a path with a space.
			return
		}
		// Absolute, ~/…, ./… and ../… paths all contain a "/" too.
		if tok = trim(tok); strings.Contains(tok, "/") && htmlFile(tok) {
			cs = append(cs, candidate{at, end, tok, false})
		}
	}
	for i := 0; i < len(s); i++ {
		end := -1
		switch c := s[i]; {
		case c == '`' || c == '"' || c == '\'':
			if j := closing(s, i+1, c); j >= 0 {
				end = j
			}
		case c == ']' && i+1 < len(s) && s[i+1] == '(':
			if j := closing(s, i+2, ')'); j >= 0 {
				i, end = i+1, j
			}
		}
		if end > i+1 && strings.ContainsFunc(s[i+1:end], unicode.IsSpace) {
			at := i + 1
			if s[at] == '<' && s[end-1] == '>' {
				// A Markdown link destination may be wrapped in <>.
				at, end = at+1, end-1
			}
			keep(at, end, true)
		}
	}
	start := -1
	for i, r := range s + " " {
		if unicode.IsSpace(r) || strings.ContainsRune("`\"<>[]{}|", r) {
			if start >= 0 {
				keep(start, i, false)
			}
			start = -1
		} else if start < 0 {
			start = i
		}
	}
	return cs
}

// closing is the index of the c that closes a span starting at s[from], on
// the same line and within maxToken bytes, or -1. A ) closes only once the
// parentheses in the span are balanced, and before the next ], so each
// Markdown link is scanned once.
func closing(s string, from int, c byte) int {
	depth := 0
	for i := from; i < len(s) && i-from <= maxToken; i++ {
		switch s[i] {
		case '\n':
			return -1
		case ']':
			if c == ')' {
				return -1
			}
		case '(':
			depth++
		case c:
			if c != ')' || depth == 0 {
				return i
			}
			depth--
		}
	}
	return -1
}

// trim takes trailing punctuation off a URL or path found in text. A ) or ]
// goes only when the match has more of them than ( or [, so http://[::1] and
// a(1).html keep theirs. An _ always stays.
func trim(s string) string {
	for s != "" {
		switch c := s[len(s)-1]; {
		case strings.IndexByte(".,;:!?'\"*`", c) >= 0:
		case c == ')' && strings.Count(s, ")") > strings.Count(s, "("):
		case c == ']' && strings.Count(s, "]") > strings.Count(s, "["):
		default:
			return s
		}
		s = s[:len(s)-1]
	}
	return s
}

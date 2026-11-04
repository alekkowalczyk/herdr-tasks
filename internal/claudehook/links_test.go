package claudehook

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/alekkowalczyk/herdr-tasks/internal/slack"
	"github.com/alekkowalczyk/herdr-tasks/internal/task"
)

const (
	aSlackMsg  = "https://acme.slack.com/archives/C0123ABCD/p1700000000123456"
	anArtifact = "https://claude.ai/code/artifact/00000000-0000-4000-8000-000000000001"
)

// fullPage is a whole HTML page titled title.
func fullPage(title string) string {
	return "<!doctype html>\n<html><head><title>" + title + "</title></head><body><p>Hi</p></body></html>\n"
}

// writeFile writes body to path, making its directory, and returns path.
func writeFile(t *testing.T, path, body string) string {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func prompt(cwd, text string) Payload {
	return Payload{HookEventName: "UserPromptSubmit", Source: "user", Cwd: cwd, Prompt: text}
}

func reply(cwd, text string) Payload {
	return Payload{HookEventName: "Stop", Cwd: cwd, LastAssistantMessage: text}
}

func toolCall(t *testing.T, name, cwd string, input, response any) Payload {
	t.Helper()
	in, err := json.Marshal(input)
	if err != nil {
		t.Fatal(err)
	}
	out, err := json.Marshal(response)
	if err != nil {
		t.Fatal(err)
	}
	return Payload{HookEventName: "PostToolUse", Cwd: cwd, ToolName: name, ToolInput: in, ToolResponse: out}
}

func checkLinks(t *testing.T, name string, got, want []Link) {
	t.Helper()
	if len(got) == 0 && len(want) == 0 {
		return
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("%s: Links =\n%swant\n%s", name, show(got), show(want))
	}
}

func show(ls []Link) string {
	if len(ls) == 0 {
		return "  nothing\n"
	}
	var b strings.Builder
	for _, l := range ls {
		fmt.Fprintf(&b, "  %+v\n", l)
	}
	return b.String()
}

// countProbes counts Links' filesystem calls, each taking delay longer.
// Atomic, as a call Links gave up on may still be running.
func countProbes(t *testing.T, delay time.Duration) *atomic.Int32 {
	n := new(atomic.Int32)
	stat, read := statFile, readStart
	t.Cleanup(func() { statFile, readStart = stat, read })
	statFile = func(p string) (os.FileInfo, error) {
		n.Add(1)
		time.Sleep(delay)
		return stat(p)
	}
	readStart = func(p string) ([]byte, error) {
		n.Add(1)
		time.Sleep(delay)
		return read(p)
	}
	return n
}

func TestLinksFromAPrompt(t *testing.T) {
	checkLinks(t, "prompt.json", Links(load(t, "links/prompt.json"), ""), []Link{
		{URL: aSlackMsg + "?thread_ts=1700000000.000001&cid=C0123ABCD"},
		{URL: "https://acme.enterprise.slack.com/archives/C0123ABCD"},
		{URL: "https://linear.app/acme/issue/ABC-12/fix-login"},
		{URL: "http://localhost:4100/showcase/"},
		{URL: "http://[::1]"},
		{URL: anArtifact + "?sk=abc"},
		{URL: "https://acme.github.io/demo/"},
		{URL: "https://github.com/acme/x/pull/1"},
	})
}

func TestPromptFromALoopIsSkipped(t *testing.T) {
	p := load(t, "links/prompt_loop_wakeup.json")
	checkLinks(t, "loop_wakeup", Links(p, ""), nil)
	p.Source = "system"
	checkLinks(t, "system", Links(p, ""), nil)
	p.Source = "sdk"
	checkLinks(t, "sdk", Links(p, ""), []Link{{URL: "http://localhost:4100/status"}, {URL: aSlackMsg}})
}

func TestReplyCountsPagesOnly(t *testing.T) {
	dir := t.TempDir()
	report := writeFile(t, filepath.Join(dir, "report.html"), fullPage("Acme report"))
	plan := writeFile(t, filepath.Join(dir, "My Pages", "plan.html"), fullPage("Acme plan"))
	docs := writeFile(t, filepath.Join(dir, "docs", "x.html"), fullPage("Acme docs"))
	var b strings.Builder
	b.WriteString("Done. The threads I read:\n")
	for i := range 5 {
		fmt.Fprintf(&b, "- https://acme.slack.com/archives/C0123ABCD/p170000000000000%d\n", i)
	}
	fmt.Fprintf(&b, "The issue is https://linear.app/acme/issue/ABC-12.\n")
	fmt.Fprintf(&b, "The report is at %s, the plan at `%s`, and docs/x.html has the rest.", report, plan)
	checkLinks(t, "reply", Links(reply(dir, b.String()), ""), []Link{
		{URL: task.FileURL(report), Label: "Acme report"},
		{URL: task.FileURL(plan), Label: "Acme plan"},
		{URL: task.FileURL(docs), Label: "Acme docs"},
	})
}

func TestHomePathInAReply(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	page := writeFile(t, filepath.Join(home, "pages", "x.html"), fullPage("Acme x"))
	checkLinks(t, "~/pages/x.html", Links(reply("", "It's at ~/pages/x.html."), ""), []Link{{URL: task.FileURL(page), Label: "Acme x"}})
}

func TestBareFilenameInProseIsSkipped(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "index.html"), fullPage("Acme home"))
	for _, p := range []Payload{
		reply(dir, "I updated index.html."),
		reply(dir, "I updated `index.html` and (index.html)."),
		prompt(dir, "Open index.html please"),
	} {
		checkLinks(t, p.Prompt+p.LastAssistantMessage, Links(p, ""), nil)
	}
}

// Claude's replies name the files it edited, so a path in a reply gets the
// same checks as a Write: source, templates and dependencies stay out.
func TestReplyNamingSourceFilesIsSkipped(t *testing.T) {
	dir := t.TempDir()
	src := writeFile(t, filepath.Join(dir, "src", "index.html"), fullPage("Acme app"))
	writeFile(t, filepath.Join(dir, "templates", "base.html"), "<!doctype html>\n<html>{% block body %}{% endblock %}</html>\n")
	writeFile(t, filepath.Join(dir, "node_modules", "x", "index.html"), fullPage("X"))
	for _, text := range []string{
		"I updated `src/index.html` and `templates/base.html`. Also see node_modules/x/index.html.",
		"I updated src/index.html and templates/base.html, and node_modules/x/index.html too.",
		"I updated " + src + ".",
	} {
		checkLinks(t, text, Links(reply(dir, text), ""), nil)
	}
	// A file:// URL in a reply is a page Claude points you at.
	checkLinks(t, "file:// URL", Links(reply(dir, "Open "+task.FileURL(src)+" to see it."), ""), []Link{{URL: task.FileURL(src), Label: "Acme app"}})
}

func TestLocalLinkWithASecretIsSkipped(t *testing.T) {
	for _, u := range []string{
		"http://localhost:4100/?token=abc",
		"http://localhost:4100/done?code=abc",
		"http://localhost:4100/?Access%5FToken=abc",
		"http://localhost:4100/#access_token=x",
		"http://localhost:4100/#/done?api_key=x",
		"http://u:p@localhost:4100/",
		"http://localhost:4100/oauth/callback?state=x",
		"https://acme.github.io/demo/?session=abc",
		"http://localhost:4100/#/oauth/callback?state=x",
		"http://localhost:4100/#/oauth/callback",
		"http://localhost:4100/#/a?b=1?token=x",
		"http://localhost:4100/#access_token=x?y",
		"http://localhost:4100/#access_token%3Dx",
		// A router reads a hash route percent-decoded.
		"http://localhost:4100/#/%63allback?state=x",
		"http://localhost:4100/#%2Fcallback",
		"http://localhost:4100/#/%6Fauth?state=x",
	} {
		checkLinks(t, u, Links(prompt("", "Open "+u+" now"), ""), nil)
	}
	// A hash route is a path, and only the parameters after its "?" are
	// checked for secret words.
	for _, kept := range []string{
		"http://localhost:4100/#/showcase",
		"http://localhost:4100/#/authors?page=2",
		"http://localhost:4100/#/password-reset?step=1",
		"http://localhost:4100/#/decode?x=1",
		"http://localhost:4100/#/design?tab=1",
	} {
		checkLinks(t, kept, Links(prompt("", "Open "+kept+" now"), ""), []Link{{URL: kept}})
	}
}

// A Linear link keeps only its issue and slug, and an artifact link only its
// share key: neither keeps a user name, a password, or another parameter.
// A pull request Claude names in a reply, or you in a prompt, is linked once,
// whatever page of it the link opens.
func TestPullRequests(t *testing.T) {
	pr := "https://github.com/acme/web/pull/12"
	text := "Opened " + pr + ", see the diff at " + pr + "/files and https://github.com/acme/web/issues/3."
	checkLinks(t, "reply", Links(reply("", text), ""), []Link{{URL: pr}})
	checkLinks(t, "prompt", Links(prompt("", text), ""), []Link{{URL: pr}})
	for raw, want := range map[string]string{
		pr + "#issuecomment-1":                                  pr,
		"https://u:p@github.com/acme/web/pull/12?token=x#a":     pr,
		"https://www.github.com/acme/web/pull/12/commits/abc12": pr,
		"http://github.com/acme/web/pull/12":                    pr,
	} {
		checkLinks(t, raw, Links(reply("", "See "+raw+" now"), ""), []Link{{URL: want}})
	}
}

// A reply naming more than maxReplyPRs pull requests lists them, as a
// summary of open PRs does, and adds none of them. Its pages still count.
func TestReplyListingPullRequests(t *testing.T) {
	var b strings.Builder
	var want []Link
	for i := range maxReplyPRs + 1 {
		fmt.Fprintf(&b, "- https://github.com/acme/web/pull/%d\n", i+1)
		if i < maxReplyPRs {
			want = append(want, Link{URL: fmt.Sprintf("https://github.com/acme/web/pull/%d", i+1)})
		}
	}
	b.WriteString("Preview: http://localhost:4100/preview\n")
	checkLinks(t, "a list", Links(reply("", b.String()), ""), []Link{{URL: "http://localhost:4100/preview"}})
	checkLinks(t, "the same in a prompt", Links(prompt("", b.String()), ""), append(want, Link{URL: "https://github.com/acme/web/pull/6"}, Link{URL: "http://localhost:4100/preview"}))
	// The same PR named again isn't another.
	few := strings.Repeat("https://github.com/acme/web/pull/1 ", maxReplyPRs+1)
	checkLinks(t, "one PR", Links(reply("", few), ""), []Link{{URL: "https://github.com/acme/web/pull/1"}})
}

func TestLinearAndArtifactLinksDropCredentials(t *testing.T) {
	for raw, want := range map[string]string{
		"https://linear.app/acme/issue/ABC-12?token=secret":                      "https://linear.app/acme/issue/ABC-12",
		"https://u:p@linear.app/acme/issue/ABC-12":                               "https://linear.app/acme/issue/ABC-12",
		"https://linear.app/acme/issue/ABC-12/fix-login#comment-1":               "https://linear.app/acme/issue/ABC-12/fix-login",
		"https://u:p@claude.ai/artifact/x?sk=a":                                  "https://claude.ai/artifact/x?sk=a",
		"https://u:p@claude.ai/artifact/x?sk=a&token=x#access_token=y":           "https://claude.ai/artifact/x?sk=a",
		"https://u@claude.ai/code/artifact/00000000-0000-4000-8000-000000000001": anArtifact,
	} {
		checkLinks(t, raw, Links(prompt("", "See "+raw+" now"), ""), []Link{{URL: want}})
	}
}

func TestLinksCapsOneCall(t *testing.T) {
	var text strings.Builder
	var want []Link
	for i := range 30 {
		u := fmt.Sprintf("http://localhost:4100/page-%d", i)
		fmt.Fprintf(&text, "%s\n", u)
		if i < MaxLinks {
			want = append(want, Link{URL: u})
		}
	}
	checkLinks(t, "30 links", Links(prompt("", text.String()), ""), want)
}

func TestProbeBudget(t *testing.T) {
	probes, b := maxProbes, budget
	t.Cleanup(func() { maxProbes, budget = probes, b })
	maxProbes = 200

	dir := t.TempDir()
	var text strings.Builder
	for i := range 500 {
		fmt.Fprintf(&text, "%s\n", filepath.Join(dir, fmt.Sprintf("missing-%d.html", i)))
	}
	text.WriteString(writeFile(t, filepath.Join(dir, "real.html"), fullPage("Acme")))

	n := countProbes(t, 0)
	start := time.Now()
	got := Links(reply(dir, text.String()), "")
	if took := time.Since(start); took > budget {
		t.Errorf("Links took %s, want at most %s", took, budget)
	}
	if got := n.Load(); int(got) > maxProbes {
		t.Errorf("Links made %d probes, want at most %d", got, maxProbes)
	}
	checkLinks(t, "after the probes ran out", got, nil)

	// Slow probes run out the time first.
	budget = 50 * time.Millisecond
	n = countProbes(t, 20*time.Millisecond)
	start = time.Now()
	Links(reply(dir, text.String()), "")
	if took := time.Since(start); took > time.Second {
		t.Errorf("Links took %s with a %s budget", took, budget)
	}
	if got := n.Load(); got > 5 {
		t.Errorf("Links made %d slow probes in %s", got, budget)
	}
}

// A probe that hangs, as on a dead network mount, doesn't hold Links past its
// budget, and what it found before is kept.
func TestHungProbeKeepsWhatWasFound(t *testing.T) {
	b, stat := budget, statFile
	release := make(chan struct{})
	t.Cleanup(func() { close(release); budget, statFile = b, stat })
	budget = 100 * time.Millisecond
	var n atomic.Int32
	statFile = func(string) (os.FileInfo, error) {
		n.Add(1)
		select {
		case <-release:
		case <-time.After(3 * time.Second):
		}
		return nil, os.ErrNotExist
	}

	dir := t.TempDir()
	start := time.Now()
	got := Links(prompt(dir, aSlackMsg+" "+dir+"/hung.html "+dir+"/other.html"), "")
	if took := time.Since(start); took > time.Second {
		t.Errorf("Links took %s with a %s budget", took, budget)
	}
	checkLinks(t, "a hung probe", got, []Link{{URL: aSlackMsg}})
	if got := n.Load(); got != 1 {
		t.Errorf("Links made %d probes, want none after the hung one", got)
	}
}

func TestSlackSendMessage(t *testing.T) {
	checkLinks(t, "slack_send_message.json", Links(load(t, "links/slack_send_message.json"), ""),
		[]Link{{URL: aSlackMsg, Label: "you: The fix for ABC-12 is deployed."}})
}

// knowsBilling is a state folder where #billing's name was seen.
func knowsBilling(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	if err := slack.LearnChannels(dir, map[string]string{"C0123ABCD": "#billing"}); err != nil {
		t.Fatal(err)
	}
	return dir
}

// sent is a slack_send_message call with input in, that posted the message
// at link.
func sent(t *testing.T, in map[string]any, link string) Payload {
	t.Helper()
	return toolCall(t, "mcp__plugin_slack_slack__slack_send_message", "", in, []map[string]string{{"type": "text",
		"text": `{"ok":true,"channel":"C0123ABCD","ts":"1793454800.000200","message_link":"` + link + `"}`}})
}

func TestSentMessageLabel(t *testing.T) {
	dir := knowsBilling(t)
	const link = "https://acme.slack.com/archives/C0123ABCD/p1793454800000200"
	p := sent(t, map[string]any{"channel_id": "C0123ABCD", "message": "on it, fix ships Friday"}, link)
	checkLinks(t, "message", Links(p, dir), []Link{{URL: link, Label: "#billing · you: on it, fix ships Friday"}})

	// text when there's no message; a channel whose name isn't known.
	const dmLink = "https://acme.slack.com/archives/D0RUI0001/p1793454800000200"
	p = sent(t, map[string]any{"channel_id": "D0RUI0001", "text": "thanks, merging now"}, dmLink)
	checkLinks(t, "text", Links(p, dir), []Link{{URL: dmLink, Label: "you: thanks, merging now"}})

	// Nothing known but who sent it; the channel comes from the link.
	checkLinks(t, "no input", Links(sent(t, map[string]any{}, link), dir), []Link{{URL: link, Label: "#billing · you"}})
	checkLinks(t, "no names", Links(sent(t, map[string]any{}, dmLink), ""), []Link{{URL: dmLink, Label: "you"}})
}

// Channels' names are read once a call, as one probe, and only for a Slack
// link. A read that hangs doesn't hold Links past its budget, and the link
// is kept without its name.
func TestChannelNamesAreOneProbe(t *testing.T) {
	dir := knowsBilling(t)
	read, b := readChannels, budget
	t.Cleanup(func() { readChannels, budget = read, b })
	var n atomic.Int32
	readChannels = func(dir string) (map[string]string, error) {
		n.Add(1)
		return read(dir)
	}
	const other = "https://acme.slack.com/archives/C0OPS0001/p1793454800000200"
	got := Links(prompt("", "See "+aSlackMsg+", "+other+" and https://acme.slack.com/archives/C0123ABCD"), dir)
	if n.Load() != 1 || len(got) != 3 || got[2].Label != "#billing" {
		t.Errorf("read the names %d times, found %+v; want once, and all three", n.Load(), got)
	}
	n.Store(0)
	Links(prompt("", "Open http://localhost:4100/showcase"), dir)
	if n.Load() != 0 {
		t.Errorf("read the names %d times for a call with no Slack link", n.Load())
	}

	budget = 100 * time.Millisecond
	release := make(chan struct{})
	defer close(release)
	readChannels = func(string) (map[string]string, error) {
		<-release
		return nil, nil
	}
	start := time.Now()
	got = Links(prompt("", "See "+aSlackMsg), dir)
	if took := time.Since(start); took > time.Second {
		t.Errorf("Links took %s with a %s budget", took, budget)
	}
	checkLinks(t, "a hung read", got, []Link{{URL: aSlackMsg}})
}

func TestPromptSlackLinkGetsChannelName(t *testing.T) {
	dir := knowsBilling(t)
	const other = "https://acme.slack.com/archives/C0OPS0001/p1793454800000200"
	p := prompt("", "See "+aSlackMsg+"?thread_ts=1700000000.000001&cid=C0123ABCD, "+other+" and https://acme.enterprise.slack.com/archives/C0123ABCD")
	checkLinks(t, "known", Links(p, dir), []Link{
		{URL: aSlackMsg + "?thread_ts=1700000000.000001&cid=C0123ABCD", Label: "#billing"},
		{URL: other},
		{URL: "https://acme.enterprise.slack.com/archives/C0123ABCD", Label: "#billing"},
	})
	checkLinks(t, "none seen", Links(p, t.TempDir()), []Link{
		{URL: aSlackMsg + "?thread_ts=1700000000.000001&cid=C0123ABCD"},
		{URL: other},
		{URL: "https://acme.enterprise.slack.com/archives/C0123ABCD"},
	})
}

func TestSlackReadThreadGivesNothing(t *testing.T) {
	checkLinks(t, "slack_read_thread.json", Links(load(t, "links/slack_read_thread.json"), ""), nil)
}

func TestArtifactPublish(t *testing.T) {
	dir := t.TempDir()
	src := writeFile(t, filepath.Join(dir, "plan.html"), fullPage("Acme plan"))
	published := "Published " + anArtifact + " (private). This session watches it."
	p := toolCall(t, "Artifact", dir, map[string]any{"file_path": "plan.html", "title": "Plan", "icon": "doc"}, published)
	checkLinks(t, "publish", Links(p, ""), []Link{{URL: anArtifact, Label: "Acme plan", Source: src}})

	// Without a <title>, the input's title.
	notes := writeFile(t, filepath.Join(dir, "notes.md"), "# Notes\n")
	p = toolCall(t, "Artifact", dir, map[string]any{"action": "publish", "file_path": notes, "title": "Acme notes"}, published)
	checkLinks(t, "markdown", Links(p, ""), []Link{{URL: anArtifact, Label: "Acme notes", Source: notes}})

	// Uploading an asset publishes nothing.
	p = toolCall(t, "Artifact", dir, map[string]any{"url": anArtifact, "file_path": src, "asset": true}, published)
	checkLinks(t, "asset", Links(p, ""), nil)
}

func TestArtifactReadGivesNothing(t *testing.T) {
	checkLinks(t, "artifact_read.json", Links(load(t, "links/artifact_read.json"), ""), nil)
}

func TestWritePage(t *testing.T) {
	dir := t.TempDir()
	body := fullPage("Acme report")
	path := writeFile(t, filepath.Join(dir, "report.html"), body)
	p := toolCall(t, "Write", dir, map[string]string{"file_path": path, "content": body}, map[string]string{"type": "create", "filePath": path})
	checkLinks(t, "write", Links(p, ""), []Link{{URL: task.FileURL(path), Label: "Acme report"}})
}

func TestImplicitPageNoise(t *testing.T) {
	dir := t.TempDir()
	write := func(rel, body, agent string) Payload {
		path := writeFile(t, filepath.Join(dir, rel), body)
		p := toolCall(t, "Write", dir, map[string]string{"file_path": path, "content": body}, map[string]string{"type": "create"})
		p.AgentID = agent
		return p
	}
	for name, p := range map[string]Payload{
		"a subagent's page":    write("sub.html", fullPage("Sub"), "a1b2c3d4"),
		"a fragment":           write("part.html", "<section><h2>Hi</h2></section>\n", ""),
		"a template name":      write("page.template.html", fullPage("Page"), ""),
		"a .tpl. name":         write("page.tpl.html", fullPage("Page"), ""),
		"a template folder":    write("templates/base.html", "<!doctype html>\n<html>{% block body %}{% endblock %}</html>\n", ""),
		"template syntax":      write("layout.html", "<!doctype html>\n<html><title>{{ .Title }}</title></html>\n", ""),
		"a source folder":      write("src/index.html", fullPage("Acme app"), ""),
		"a dependency":         write("node_modules/x/index.html", fullPage("X"), ""),
		"a build folder":       write("site/dist/index.html", fullPage("Dist"), ""),
		"a Thymeleaf template": write("mail.html", "<!doctype html>\n<html><p th:text=\"${name}\">x</p></html>\n", ""),
	} {
		checkLinks(t, name, Links(p, ""), nil)
	}

	src := filepath.Join(dir, "src", "index.html")
	nav := toolCall(t, "mcp__claude-in-chrome__navigate", dir, map[string]string{"url": task.FileURL(src)}, []any{})
	checkLinks(t, "navigate to src/index.html", Links(nav, ""), []Link{{URL: task.FileURL(src), Label: "Acme app"}})
}

func TestEditReadsTheFile(t *testing.T) {
	dir := t.TempDir()
	full := writeFile(t, filepath.Join(dir, "page.html"), fullPage("Acme page"))
	part := writeFile(t, filepath.Join(dir, "part.html"), "<section>Hello</section>\n")
	edit := func(tool, path string) Payload {
		return toolCall(t, tool, dir, map[string]any{"file_path": path, "old_string": "Hi", "new_string": "Hello"}, map[string]any{"filePath": path})
	}
	want := []Link{{URL: task.FileURL(full), Label: "Acme page"}}
	checkLinks(t, "Edit of a page", Links(edit("Edit", full), ""), want)
	checkLinks(t, "MultiEdit of a page", Links(edit("MultiEdit", full), ""), want)
	checkLinks(t, "Edit of a fragment", Links(edit("Edit", part), ""), nil)
}

func TestNavigateLocalAndFile(t *testing.T) {
	checkLinks(t, "navigate_local.json", Links(load(t, "links/navigate_local.json"), ""), []Link{{URL: "http://localhost:4100/showcase"}})
	checkLinks(t, "navigate_web.json", Links(load(t, "links/navigate_web.json"), ""), nil)

	dir := t.TempDir()
	page := writeFile(t, filepath.Join(dir, "demo.html"), fullPage("Acme demo"))
	p := toolCall(t, "mcp__playwright__browser_navigate", dir, map[string]string{"url": task.FileURL(page)}, []any{})
	checkLinks(t, "a file page", Links(p, ""), []Link{{URL: task.FileURL(page), Label: "Acme demo"}})

	notes := writeFile(t, filepath.Join(dir, "notes.txt"), "<html>")
	p = toolCall(t, "mcp__playwright__browser_navigate", dir, map[string]string{"url": task.FileURL(notes)}, []any{})
	checkLinks(t, "a text file", Links(p, ""), nil)

	// Chrome's navigate takes a URL without its scheme, as the address bar does.
	p = toolCall(t, "mcp__claude-in-chrome__navigate", dir, map[string]string{"url": "localhost:4100/showcase"}, []any{})
	checkLinks(t, "localhost without a scheme", Links(p, ""), []Link{{URL: "http://localhost:4100/showcase"}})
	for _, url := range []string{"back", "forward", "acme.github.io/demo", "localhost:4100/callback?code=x"} {
		p = toolCall(t, "mcp__claude-in-chrome__navigate", dir, map[string]string{"url": url}, []any{})
		checkLinks(t, url, Links(p, ""), nil)
	}
}

func TestTitleIsCleaned(t *testing.T) {
	dir := t.TempDir()
	body := "<!doctype html>\n<html><head><title>a &amp;\x1b[31m b</title></head></html>\n"
	path := writeFile(t, filepath.Join(dir, "t.html"), body)
	p := toolCall(t, "Write", dir, map[string]string{"file_path": path, "content": body}, map[string]string{"type": "create"})
	checkLinks(t, "title", Links(p, ""), []Link{{URL: task.FileURL(path), Label: "a &[31m b"}})
}

func TestTrimming(t *testing.T) {
	dir := t.TempDir()
	paren := writeFile(t, filepath.Join(dir, "a(1).html"), fullPage("Acme paren"))
	text := "See [demo](http://localhost:4100/x_), (http://localhost:4100/y) and " + paren + ". Or 'http://localhost:4100/z', or http://localhost:4100/w(1)."
	checkLinks(t, "trimming", Links(prompt(dir, text), ""), []Link{
		{URL: "http://localhost:4100/x_"},
		{URL: "http://localhost:4100/y"},
		{URL: task.FileURL(paren), Label: "Acme paren"},
		{URL: "http://localhost:4100/z"},
		{URL: "http://localhost:4100/w(1)"},
	})
}

func TestFindIgnoresOtherEvents(t *testing.T) {
	p := load(t, "get_issue.json")
	for _, ev := range []string{"Stop", "UserPromptSubmit", "PostToolUseFailure"} {
		p.HookEventName = ev
		if got, found := Find(p); found {
			t.Errorf("Find on %s = %+v, want nothing", ev, got)
		}
	}
	stop := load(t, "links/stop_linear.json")
	if got, found := Find(stop); found {
		t.Errorf("Find on stop_linear.json = %+v, want nothing", got)
	}
	checkLinks(t, "stop_linear.json", Links(stop, ""), nil)
}

func TestSlackMessage(t *testing.T) {
	for raw, want := range map[string]string{
		aSlackMsg: "C0123ABCD/p1700000000123456",
		aSlackMsg + "?thread_ts=1700000000.000001&cid=C0123ABCD":                  "C0123ABCD/p1700000000123456",
		"https://acme.enterprise.slack.com/archives/C0123ABCD/p1700000000123456/": "C0123ABCD/p1700000000123456",
		"https://acme.slack.com/archives/C0123ABCD":                               "",
		"https://acme.slack.com/archives/C0123ABCD/p17000000001234567":            "",
		"https://acme.slack.com/archives/C0123ABCD/p1700000000123456/x":           "",
		"http://acme.slack.com/archives/C0123ABCD/p1700000000123456":              "",
		"https://linear.app/acme/issue/ABC-12":                                    "",
	} {
		got, ok := SlackMessage(raw)
		if got != want || ok != (want != "") {
			t.Errorf("SlackMessage(%q) = %q, %v; want %q", raw, got, ok, want)
		}
	}
}

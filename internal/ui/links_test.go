package ui

import (
	"fmt"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/charmbracelet/x/ansi"

	"github.com/alekkowalczyk/herdr-tasks/internal/herdr"
	"github.com/alekkowalczyk/herdr-tasks/internal/task"
)

const (
	linearIssue = "https://linear.app/acme/issue/ABC-12"
	slackMsg    = "https://acme.slack.com/archives/C0123ABCD/p1700000000123456"
	slackChan   = "https://acme.enterprise.slack.com/archives/C0123ABCD"
	hostedPage  = "https://acme.github.io/demo/"
	otherLink   = "https://github.com/acme/x/pull/1"
)

func links(urls ...string) []task.Link {
	var out []task.Link
	for _, u := range urls {
		out = append(out, task.Link{URL: u})
	}
	return out
}

// plain is a row as you'd read it: no escape sequences, single spaces.
func plain(row string) string {
	return strings.Join(strings.Fields(ansi.Strip(row)), " ")
}

// sections is the section name each row starts with.
func sections(rows []string) []string {
	var out []string
	for _, r := range rows {
		if f := strings.Fields(ansi.Strip(r)); len(f) > 0 {
			out = append(out, f[0])
		}
	}
	return out
}

func TestLinkRowsGroupsBySection(t *testing.T) {
	rows := linkRows(links(hostedPage, linearIssue, otherLink, slackChan), 52, false)
	if got, want := sections(rows), []string{"Linear", "Slack", "Pages", "Links"}; !slices.Equal(got, want) {
		t.Fatalf("sections = %q, want %q", got, want)
	}
	want := []string{"Linear ABC-12", "Slack channel", "Pages acme.github.io/demo", "Links PR #1"}
	for i, r := range rows {
		if got := plain(r); got != want[i] {
			t.Errorf("row %d = %q, want %q", i, got, want[i])
		}
	}
	// Only sections with links get a row.
	rows = linkRows(links(otherLink, hostedPage), 52, false)
	if got, want := sections(rows), []string{"Pages", "Links"}; !slices.Equal(got, want) {
		t.Errorf("sections = %q, want %q", got, want)
	}
}

func TestLinkRowsFitWidth(t *testing.T) {
	dir := t.TempDir()
	var ls []task.Link
	for i := 1; i <= 8; i++ {
		ls = append(ls, task.Link{URL: task.FileURL(filepath.Join(dir, fmt.Sprintf("page-%d.html", i)))})
	}
	rows := linkRows(ls, 40, false)
	if len(rows) != 1 {
		t.Fatalf("got %d rows, want 1: %q", len(rows), rows)
	}
	row := rows[0]
	if w := ansi.StringWidth(row); w > 40 {
		t.Errorf("row is %d cells wide, want at most 40: %q", w, row)
	}
	shown := strings.Count(row, "\x1b]8;;") / 2
	if shown == 0 || shown == 8 {
		t.Fatalf("%d of 8 pages shown, want some but not all: %q", shown, row)
	}
	if want := " +" + strconv.Itoa(8-shown); !strings.HasSuffix(ansi.Strip(row), want) {
		t.Errorf("row = %q, want it to end in %q", ansi.Strip(row), want)
	}

	// No width is too narrow, and a long label is cut rather than hidden.
	ls = append(ls, task.Link{URL: hostedPage, Label: strings.Repeat("long title ", 7)})
	for w := 10; w <= 60; w++ {
		for _, r := range linkRows(ls, w, true) {
			if got := ansi.StringWidth(r); got > w {
				t.Errorf("w=%d: row is %d cells wide: %q", w, got, r)
			}
		}
	}
	rows = linkRows([]task.Link{{URL: hostedPage, Label: strings.Repeat("long title ", 7)}}, 30, false)
	if got := plain(rows[0]); !strings.HasPrefix(got, "Pages long title") || !strings.HasSuffix(got, "…") {
		t.Errorf("row = %q, want the label cut to fit", got)
	}
}

func TestLinkRowsNumbered(t *testing.T) {
	rows := linkRows(links(hostedPage, linearIssue, slackChan, hostedPage+"other"), 80, true)
	want := []string{"Linear 2 ABC-12", "Slack 3 channel", "Pages 1 acme.github.io/demo · 4 acme.github.io/demo/other"}
	if len(rows) != len(want) {
		t.Fatalf("got %d rows, want %d: %q", len(rows), len(want), rows)
	}
	for i, r := range rows {
		if got := plain(r); got != want[i] {
			t.Errorf("row %d = %q, want %q", i, got, want[i])
		}
	}
}

func TestLinkRowsEmitOnlyTheirOwnSequences(t *testing.T) {
	ls := []task.Link{
		{URL: hostedPage, Label: "x\x1b[2Jy\x07"},
		{URL: "http://x/\x1b]8;;http://evil\x1b\\"},
		{URL: "http://x/\x07"},
		{URL: "http://localhost:4100/%1b]8;;http://evil"},
	}
	out := strings.Join(linkRows(ls, 52, false), "\n")
	for _, bad := range []string{"\x1b[2J", "\x07", "http://evil"} {
		if strings.Contains(out, bad) {
			t.Errorf("output has %q: %q", bad, out)
		}
	}
	safe := 1
	if n := strings.Count(out, "\x1b]8;;"); n != 2*safe {
		t.Errorf("output has %d OSC 8 sequences, want %d: %q", n, 2*safe, out)
	}
}

// ownSequences are the escape sequences the UI writes: OSC 8 links, and
// lipgloss's colors when it uses them.
var ownSequences = regexp.MustCompile(`\x1b]8;;[^\x1b\x07]*\x1b\\|\x1b\[[0-9;]*m`)

func TestDetailLinksEmitOnlyTheirOwnSequences(t *testing.T) {
	// A hand-edited state file: nothing on the way in cleaned these.
	tk := &task.Task{TabID: "w1:t1", Stage: task.Active, Since: time.Now(), Links: []task.Link{
		{URL: hostedPage, Label: "Demo"},
		{URL: "https://evil.example/\x1b]52;c;aGVsbG8=\x07\x1b[2J", By: "claude\x1b]52;c;aGVsbG8=\x07"},
		{URL: "https://evil.example/\u009b2J", Label: "x\x1b[2J", By: "a\x9bb"},
	}}
	m := &model{rowAt: map[int]string{}, linkAt: map[int]task.Link{}, sel: tk.TabID, rows: []row{
		{tab: herdr.Tab{ID: tk.TabID}, task: tk, ws: herdr.Workspace{Label: "Acme"}, pos: 1, group: task.YourTurn},
	}}
	out := strings.Join(m.detailLines(80, 40), "\n")
	for _, bad := range []string{"\x1b]52", "\x1b[2J", "\x07", "\u009b", "\x9b"} {
		if strings.Contains(out, bad) {
			t.Errorf("output has %q: %q", bad, out)
		}
	}
	if rest := ownSequences.ReplaceAllString(out, ""); strings.Contains(rest, "\x1b") {
		t.Errorf("output has an escape sequence it didn't make: %q", rest)
	}
	if n := strings.Count(out, "\x1b]8;;"); n != 2 {
		t.Errorf("output has %d OSC 8 sequences, want 2, for the one safe link: %q", n, out)
	}
	if !strings.Contains(ansi.Strip(out), "unsafe link") {
		t.Errorf("output doesn't say a link is unsafe: %q", ansi.Strip(out))
	}
}

func TestLinkItemSlackTime(t *testing.T) {
	local := time.Local
	time.Local = time.FixedZone("UTC+3", 3*60*60)
	t.Cleanup(func() { time.Local = local })

	// p1700000000… is 2023-11-14 22:13:20 UTC.
	if got := linkItem(task.Link{URL: slackMsg}); got != "Nov 15 01:13" {
		t.Errorf("Slack message = %q, want %q", got, "Nov 15 01:13")
	}
	if got := linkItem(task.Link{URL: slackChan}); got != "channel" {
		t.Errorf("Slack channel = %q, want %q", got, "channel")
	}
}

func TestLinkItem(t *testing.T) {
	cases := []struct {
		link task.Link
		want string
	}{
		{task.Link{URL: linearIssue + "/fix-it", Label: "Fix the login"}, "ABC-12"},
		{task.Link{URL: hostedPage, Label: "Demo\n  page"}, "Demo page"},
		{task.Link{URL: hostedPage}, "acme.github.io/demo"},
		{task.Link{URL: task.FileURL("/tmp/acme/plan.html")}, "plan.html"},
		{task.Link{URL: otherLink}, "PR #1"},
		{task.Link{URL: "http://x/\x1b]8;;http://evil"}, "unsafe link"},
		{task.Link{URL: "http://x/\x07", Label: "Mine"}, "Mine"},
	}
	for _, c := range cases {
		if got := linkItem(c.link); got != c.want {
			t.Errorf("linkItem(%q) = %q, want %q", c.link.URL, got, c.want)
		}
	}
}

func TestHyperlinkChecksTheURL(t *testing.T) {
	linked := []string{hostedPage, "http://localhost:4100/a;b", "HTTPS://acme.github.io/demo/", task.FileURL("/tmp/acme/plan.html")}
	for _, u := range linked {
		if got, want := hyperlink(u, "a\x1b[2Jb"), "\x1b]8;;"+u+"\x1b\\a[2Jb\x1b]8;;\x1b\\"; got != want {
			t.Errorf("hyperlink(%q) = %q, want %q", u, got, want)
		}
	}
	unsafe := []string{"http://a;b@localhost:4100/", "ht;tp://x/", "http://localhost:4100/\x1b", "http://localhost:4100/%9b", "http://x/\u009b",
		// Schemes a terminal would hand to some other app, and files on other hosts.
		"shortcuts://run-shortcut?name=Wipe", "vscode://file/etc/passwd", "mailto:a@b.c", "file://otherhost/tmp/x.html", "http:///no-host", "javascript://x/%0aalert(1)"}
	for _, u := range unsafe {
		if got := hyperlink(u, "a\x1b[2Jb"); got != "a[2Jb" {
			t.Errorf("hyperlink(%q) = %q, want the cleaned text alone", u, got)
		}
	}
}

func TestUnlinkRows(t *testing.T) {
	page := func(i int) task.Link {
		return task.Link{URL: task.FileURL(fmt.Sprintf("/tmp/acme/page-%d.html", i))}
	}
	pages := func(n int) []task.Link {
		var ls []task.Link
		for i := 1; i <= n; i++ {
			ls = append(ls, page(i))
		}
		return ls
	}

	// One column when it's enough.
	rows := unlinkRows(append(links(linearIssue, slackChan), page(3)), 80, 12)
	want := []string{"1 Linear ABC-12", "2 Slack channel", "3 Pages page-3.html"}
	if len(rows) != len(want) {
		t.Fatalf("got %d rows, want %d: %q", len(rows), len(want), rows)
	}
	for i, r := range rows {
		if got := plain(r); got != want[i] {
			t.Errorf("row %d = %q, want %q", i, got, want[i])
		}
	}

	// Columns, left to right, when it isn't.
	rows = unlinkRows(pages(20), 120, 12)
	if len(rows) != 10 {
		t.Fatalf("got %d rows, want 10 (two columns): %q", len(rows), rows)
	}
	if got := plain(rows[0]); got != "1 Pages page-1.html 11 Pages page-11.html" {
		t.Errorf("first row = %q", got)
	}

	// When even that isn't enough, the last cell says where the rest are.
	rows = unlinkRows(pages(30), 80, 12)
	if len(rows) != 12 {
		t.Fatalf("got %d rows, want 12: %q", len(rows), rows)
	}
	for _, r := range rows {
		if w := ansi.StringWidth(r); w > 80 {
			t.Errorf("row is %d cells wide: %q", w, r)
		}
	}
	if got := plain(rows[11]); !strings.HasSuffix(got, "+7 more: herdr-tasks link rm") {
		t.Errorf("last row = %q, want it to end in the +7 more cell", got)
	}
}

package task

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const (
	slackMsg  = "https://acme.slack.com/archives/C0123ABCD/p1700000000123456"
	artifact  = "https://claude.ai/code/artifact/00000000-0000-4000-8000-000000000001"
	localPage = "http://localhost:4100"
)

func TestKindOf(t *testing.T) {
	cases := map[string]Kind{
		"https://linear.app/acme/issue/ABC-12": KindLinear,
		slackMsg:                               KindSlack,
		"https://acme.enterprise.slack.com/archives/C0123ABCD":   KindSlack,
		"https://slack.com/intl/en-gb/":                          KindOther,
		"https://acme.slack.com/":                                KindOther,
		artifact:                                                 KindArtifact,
		"https://claude.ai/artifact/AbC123?sk=x":                 KindArtifact,
		"https://claude.ai/chat/123":                             KindOther,
		"https://acme.github.io/demo/":                           KindHosted,
		"https://acme.surge.sh":                                  KindHosted,
		"https://github.io/":                                     KindOther,
		"https://github.com/acme/x/pull/1":                       KindGitHubPR,
		"https://www.github.com/acme/x/pull/1/files#diff-1":      KindGitHubPR,
		"http://github.com/acme/x/pull/1":                        KindGitHubPR,
		"https://github.com/acme/x/pulls":                        KindOther,
		"https://github.com/acme/x/pull/new/branch":              KindOther,
		"https://github.com/acme/x/issues/1":                     KindOther,
		"https://gitlab.com/acme/x/pull/1":                       KindOther,
		"file:///tmp/a.html":                                     KindFile,
		"file://localhost/tmp/a.html":                            KindFile,
		"file://nas/share/a.html":                                KindOther,
		"http://localhost:4100/x":                                KindLocal,
		"https://127.0.0.1:8443":                                 KindLocal,
		"http://[::1]":                                           KindLocal,
		"not a url":                                              KindOther,
		"https://linear.app/acme/project/roadmap-0123456789abcd": KindOther,
	}
	for in, want := range cases {
		if got := KindOf(in); got != want {
			t.Errorf("KindOf(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestSections(t *testing.T) {
	cases := map[Kind]string{
		KindLinear: "Linear", KindGitHubPR: "GitHub", KindSlack: "Slack",
		KindArtifact: "Pages", KindHosted: "Pages", KindFile: "Pages", KindLocal: "Pages",
		KindOther: "Links",
	}
	for k, want := range cases {
		if got := k.Section(); got != want {
			t.Errorf("%s.Section() = %q, want %q", k, got, want)
		}
	}
}

func TestLinkKeySameForURLForms(t *testing.T) {
	same := [][2]string{
		{slackMsg, slackMsg + "?thread_ts=1700000000.000001&cid=C0123ABCD"},
		{artifact, artifact + "?sk=abc"},
		{localPage, localPage + "/"},
		{"file:///tmp/x/../a.html", "file:///tmp/a.html"},
		{"https://linear.app/acme/issue/ABC-12", "https://linear.app/acme/issue/abc-12/fix-it"},
		{"https://github.com/acme/x/pull/1", "https://github.com/acme/x/pull/1/"},
		{"https://github.com/acme/x/pull/1", "https://www.github.com/Acme/X/pull/1/files?w=1#diff-2"},
		{"https://github.com/acme/x/pull/1", "http://github.com/acme/x/pull/1#issuecomment-3"},
	}
	for _, c := range same {
		if a, b := LinkKey(c[0]), LinkKey(c[1]); a != b {
			t.Errorf("LinkKey(%q) = %q, LinkKey(%q) = %q, want the same", c[0], a, c[1], b)
		}
	}
	different := [][2]string{
		{localPage + "/#/a", localPage + "/#/b"},
		{localPage + "/a", localPage + "/b"},
		{localPage, "https://localhost:4100"},
		{"https://github.com/acme/x/pull/1", "https://github.com/acme/x/pull/2"},
		{"https://github.com/acme/x/pull/1", "https://github.com/acme/y/pull/1"},
	}
	for _, c := range different {
		if a, b := LinkKey(c[0]), LinkKey(c[1]); a == b {
			t.Errorf("LinkKey(%q) and LinkKey(%q) are both %q, want them different", c[0], c[1], a)
		}
	}
}

func TestGitHubPR(t *testing.T) {
	cases := map[string]struct {
		repo string
		n    int
	}{
		"https://github.com/acme/web/pull/12":                       {"acme/web", 12},
		"https://www.github.com/Acme/Web/pull/12/commits/abc?w=1#x": {"Acme/Web", 12},
		"https://github.com/acme/web/pull/0":                        {},
		"https://github.com/acme/web/pull/12x":                      {},
		"https://github.com/acme/web/issues/12":                     {},
		"https://github.com/acme/pull/12":                           {},
		"https://acme.github.io/web/pull/12":                        {},
		"ftp://github.com/acme/web/pull/12":                         {},
	}
	for in, want := range cases {
		repo, n, ok := GitHubPR(in)
		if repo != want.repo || n != want.n || ok != (want.n > 0) {
			t.Errorf("GitHubPR(%q) = %q, %d, %v, want %q, %d", in, repo, n, ok, want.repo, want.n)
		}
	}
	if got := GitHubPRURL("acme/web", 12); got != "https://github.com/acme/web/pull/12" {
		t.Errorf("GitHubPRURL = %q", got)
	}
}

func TestSlackTime(t *testing.T) {
	at, ok := SlackTime(slackMsg + "?thread_ts=1700000000.000001")
	if !ok || at.Unix() != 1700000000 || at.Nanosecond() != 123456000 {
		t.Errorf("SlackTime = %v, %v, want 1700000000.123456", at, ok)
	}
	if _, ok := SlackTime("https://acme.slack.com/archives/C0123ABCD"); ok {
		t.Error("a channel link has no time")
	}
}

func TestCleanLabel(t *testing.T) {
	cases := map[string]string{
		"a\x1b]0;x\x07b":         "a]0;xb",
		"line\none":              "line one",
		"\u009b31m":              "31m",
		"  a \t\x1b b  ":         "a b",
		"bad \xff\x9b byte":      "bad byte",
		strings.Repeat("é", 100): strings.Repeat("é", 79) + "…",
		strings.Repeat("é", 80):  strings.Repeat("é", 80),
	}
	for in, want := range cases {
		if got := CleanLabel(in); got != want {
			t.Errorf("CleanLabel(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestCleanText(t *testing.T) {
	cases := map[string]string{
		"a\x1b]0;x\x07b":                 "a]0;xb",
		"line\none\n\nthree":             "line\none\n\nthree",
		"\u009b31m":                      "31m",
		"  a \t\x1b b\r\n":               "  a   b\n",
		"bad \xff\x9b byte":              "bad  byte",
		strings.Repeat("é", 100):         strings.Repeat("é", 100),
		"x \x1b[2J y \x1b]52;c;aGk=\x07": "x [2J y ]52;c;aGk=",
	}
	for in, want := range cases {
		if got := CleanText(in); got != want {
			t.Errorf("CleanText(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestSafeURL(t *testing.T) {
	for _, u := range []string{localPage + "/a%20b", slackMsg, "file:///tmp/caf%C3%A9.html"} {
		if !SafeURL(u) {
			t.Errorf("SafeURL(%q) = false, want true", u)
		}
	}
	for _, u := range []string{localPage + "/\x1b]8;;x", localPage + "/%1b", localPage + "/%C2%9B", localPage + "/%7F", "http://x/\x07", "http://[::1"} {
		if SafeURL(u) {
			t.Errorf("SafeURL(%q) = true, want false", u)
		}
	}
}

func TestFilePath(t *testing.T) {
	cases := map[string]string{
		"file:///tmp/x/../a.html":        "/tmp/a.html",
		"file://localhost/tmp/a%20b.htm": "/tmp/a b.htm",
		"file://nas/share/a.html":        "",
		"file:a.html":                    "",
		"https://acme.github.io/a.html":  "",
	}
	for in, want := range cases {
		got, ok := FilePath(in)
		if got != want || ok != (want != "") {
			t.Errorf("FilePath(%q) = %q, %v, want %q", in, got, ok, want)
		}
	}
	if got := FileURL("/tmp/x/../a b.html"); got != "file:///tmp/a%20b.html" {
		t.Errorf("FileURL = %q", got)
	}
}

func TestLinkLabelForNewKinds(t *testing.T) {
	cases := map[string]string{
		slackMsg: "Slack message",
		"https://acme.enterprise.slack.com/archives/C0123ABCD": "Slack channel",
		artifact:                                  "Artifact",
		"file:///tmp/demo/plan.html":              "plan.html",
		localPage + "/showcase/?tab=2":            "localhost:4100/showcase",
		localPage + "/":                           "localhost:4100",
		localPage + "/#/showcase":                 "localhost:4100#/showcase",
		"http://[::1]":                            "[::1]",
		"https://acme.github.io/demo/":            "acme.github.io/demo",
		"file:///tmp/%1B%5B2J.html":               "[2J.html",
		"https://github.com/acme/x/pull/12/files": "PR #12",
	}
	for in, want := range cases {
		if got := LinkLabel(in); got != want {
			t.Errorf("LinkLabel(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestLinkInput(t *testing.T) {
	dir := t.TempDir()
	page := filepath.Join(dir, "docs", "plan.html")
	if err := os.MkdirAll(filepath.Dir(page), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(page, []byte("<!doctype html>"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", dir)
	cases := []struct{ raw, cwd, want string }{
		{page, "", FileURL(page)},
		{"docs/plan.html", dir, FileURL(page)},
		{"./docs/plan.html", dir, FileURL(page)},
		{"~/docs/plan.html", "", FileURL(page)},
		{"docs/plan.html", "", "https://docs/plan.html"},
		{"docs", dir, "https://docs"},
		{"example.com/x", dir, "https://example.com/x"},
		{"missing.html", dir, "https://missing.html"},
		{"http://localhost:4100", dir, "http://localhost:4100"},
	}
	for _, c := range cases {
		if got := LinkInput(c.raw, c.cwd); got != c.want {
			t.Errorf("LinkInput(%q, %q) = %q, want %q", c.raw, c.cwd, got, c.want)
		}
	}
}

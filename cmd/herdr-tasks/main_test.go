package main

import (
	"bytes"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/alekkowalczyk/herdr-tasks/internal/task"
)

// onTask returns a change func that edits tk, as a.change edits the target
// tab's task.
func onTask(tk *task.Task) func(func(*task.Task) error) error {
	return func(fn func(*task.Task) error) error { return fn(tk) }
}

func TestAgentLinkAddOfARemovedIssueSaysSo(t *testing.T) {
	tk := &task.Task{DismissedIssues: []string{"ABC-12"}}
	var out bytes.Buffer
	if err := linkAdd(onTask(tk), &out, "https://linear.app/acme/issue/ABC-12", "", "", "claude"); err != nil {
		t.Fatal(err)
	}
	if len(tk.Links) != 0 {
		t.Errorf("links = %+v, want none", tk.Links)
	}
	if s := out.String(); !strings.Contains(s, "ABC-12 was removed from this task") || !strings.Contains(s, "herdr-tasks link add https://linear.app/acme/issue/ABC-12") {
		t.Errorf("said %q, want a note that ABC-12 stays removed", s)
	}

	// You can add it back, and nothing is said.
	out.Reset()
	if err := linkAdd(onTask(tk), &out, "https://linear.app/acme/issue/ABC-12", "", "", ""); err != nil {
		t.Fatal(err)
	}
	if len(tk.Links) != 1 || out.Len() > 0 {
		t.Errorf("links = %+v, said %q; want the link and nothing said", tk.Links, out.String())
	}
}

func TestAgentLinkAddOfARemovedPageSaysSo(t *testing.T) {
	tk := &task.Task{}
	tk.AddLink("http://localhost:4100/showcase", "", "", time.Now())
	if _, err := tk.RemoveLink("1"); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	// Another form of the same page.
	if err := linkAdd(onTask(tk), &out, "http://localhost:4100/showcase/", "", "", "claude"); err != nil {
		t.Fatal(err)
	}
	if len(tk.Links) != 0 {
		t.Errorf("links = %+v, want none", tk.Links)
	}
	if s := out.String(); !strings.Contains(s, "localhost:4100/showcase was removed from this task") || !strings.Contains(s, "herdr-tasks link add http://localhost:4100/showcase/") {
		t.Errorf("said %q, want a note that the page stays removed", s)
	}
}

func TestLinkAddTakesALocalPath(t *testing.T) {
	dir := t.TempDir()
	page := filepath.Join(dir, "docs", "plan.html")
	if err := os.MkdirAll(filepath.Dir(page), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(page, []byte("<!doctype html><title>Acme plan</title>\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	tk := &task.Task{}
	var out bytes.Buffer
	// Relative to where link add runs, then as an absolute path, which is
	// the same link.
	for _, target := range []string{"docs/plan.html", page, "acme.github.io/demo/", "docs/missing.html"} {
		if err := linkAdd(onTask(tk), &out, target, dir, "", ""); err != nil {
			t.Fatal(err)
		}
	}
	var got []string
	for _, l := range tk.Links {
		got = append(got, l.URL)
	}
	want := []string{task.FileURL(page), "https://acme.github.io/demo/", "https://docs/missing.html"}
	if !slices.Equal(got, want) || out.Len() > 0 {
		t.Errorf("links = %q, said %q; want %q and nothing said", got, out.String(), want)
	}
}

func TestLinkAddOfALinkedIssueSaysNothing(t *testing.T) {
	tk := &task.Task{Links: []task.Link{{URL: "https://linear.app/acme/issue/ABC-12"}}}
	var out bytes.Buffer
	if err := linkAdd(onTask(tk), &out, "https://linear.app/acme/issue/ABC-12/fix-login", "", "", "claude"); err != nil {
		t.Fatal(err)
	}
	if len(tk.Links) != 1 || out.Len() > 0 {
		t.Errorf("links = %+v, said %q; want one link and nothing said", tk.Links, out.String())
	}
}

func TestShowPrintsAgentTextCleaned(t *testing.T) {
	tk := &task.Task{
		TabID: "w1:t1", Workspace: "Acme\x1b[2J", Stage: task.Paused, Since: time.Now(),
		Title:  "Checkout\x1b]0;owned\x07",
		Reason: "QA \x1b[2J\x07sign-off",
		Note:   "hi \x1b]0;owned\x07 there\nsecond\tline",
		Log:    []task.Entry{{Who: "claude\x1b]52;c;aGVsbG8=\x07", At: time.Now(), Text: "x \x1b[2J y\u009b"}},
		Links: []task.Link{
			{URL: "https://acme.github.io/demo/", Label: "Demo\x1b[2J", By: "claude\x1b]0;x\x07"},
			{URL: "https://evil.example/\x1b]52;c;aGVsbG8=\x07"},
			{URL: "https://evil.example/%1b]0;x", Label: "Plan"},
		},
	}
	var out bytes.Buffer
	printTask(&out, tk, tk.TabID, "", 1, time.Now())
	s := out.String()
	for _, bad := range []string{"\x1b", "\x07", "\u009b", "evil.example"} {
		if strings.Contains(s, bad) {
			t.Errorf("show printed %q:\n%q", bad, s)
		}
	}
	for _, want := range []string{
		"Checkout]0;owned  (Acme[2J · w1:t1)",
		"until QA [2Jsign-off",
		"  hi ]0;owned there\n  second line\n",
		"claude]52;c;aGVsbG8=",
		"x [2J y\n",
		"1. Demo[2J  https://acme.github.io/demo/  (via claude]0;x)\n",
		"2. unsafe link\n",
		"3. Plan  unsafe link\n",
	} {
		if !strings.Contains(s, want) {
			t.Errorf("show didn't print %q:\n%s", want, s)
		}
	}
}

func TestLinkAddRefusesWhatItCantSave(t *testing.T) {
	for _, target := range []string{"https://example.com/100%done", "https://example.com/a#100%", "https://example.com/%0A", "https://example.com/\x1b[2J", "foo bar"} {
		for _, by := range []string{"", "claude"} {
			tk := &task.Task{}
			var out bytes.Buffer
			err := linkAdd(onTask(tk), &out, target, "", "", by)
			if err == nil {
				t.Errorf("link add %q as %q: no error", target, by)
			} else if strings.ContainsAny(err.Error(), "\x1b\n") {
				t.Errorf("link add %q: the error has a control character: %q", target, err)
			}
			if len(tk.Links) != 0 {
				t.Errorf("link add %q as %q: links = %+v, want none", target, by, tk.Links)
			}
		}
	}
	// A % in the query is fine, as url.Parse leaves it alone.
	tk := &task.Task{}
	if err := linkAdd(onTask(tk), io.Discard, "https://example.com/?q=50%off", "", "", ""); err != nil || len(tk.Links) != 1 {
		t.Errorf("link add with a %% in the query = %v, links %+v; want it added", err, tk.Links)
	}
}

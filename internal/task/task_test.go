package task

import (
	"testing"
	"time"
)

func TestLinkLabel(t *testing.T) {
	cases := map[string]string{
		"https://github.com/acme/api/pull/431":           "PR #431",
		"https://github.com/acme/api/issues/12":          "issue #12",
		"https://github.com/acme/api":                    "acme/api",
		"https://linear.app/acme/issue/MWP-418/fix-it":   "MWP-418",
		"https://acme.atlassian.net/browse/OPS-7":        "OPS-7",
		"https://www.figma.com/file/abc/Checkout":        "Figma",
		"https://www.notion.so/acme/Onboarding-API":      "Notion page",
		"https://gitlab.com/acme/api/-/merge_requests/9": "MR !9",
		"https://example.com/some/page":                  "example.com",
		"not a url":                                      "not a url",
	}
	for in, want := range cases {
		if got := LinkLabel(in); got != want {
			t.Errorf("LinkLabel(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestGroupOf(t *testing.T) {
	cases := []struct {
		stage  Stage
		status string
		want   Group
	}{
		{Active, "working", Running},
		{Active, "done", YourTurn},
		{Active, "blocked", YourTurn},
		{Active, "idle", YourTurn},
		{Active, "unknown", YourTurn},
		{Paused, "working", GPaused},
		{Parked, "idle", GParked},
		{Done, "working", GDone},
	}
	for _, c := range cases {
		if got := GroupOf(c.stage, c.status); got != c.want {
			t.Errorf("GroupOf(%s, %s) = %s, want %s", c.stage, c.status, got, c.want)
		}
	}
}

func TestLabels(t *testing.T) {
	if got := TitleFromLabel("3", 3); got != "" {
		t.Errorf("a tab named by its position should stay unnamed, got %q", got)
	}
	if got := TitleFromLabel("3", 1); got != "3" {
		t.Errorf("a number that isn't the position is a real name, got %q", got)
	}
	if got := TitleFromLabel("‖ fix login", 2); got != "fix login" {
		t.Errorf("marker not stripped: %q", got)
	}
	if got := TitleFromLabel("● 2", 2); got != "" {
		t.Errorf("marked position label should stay unnamed, got %q", got)
	}
	if got := MarkedLabel(Parked, "", 4); got != "○ 4" {
		t.Errorf("MarkedLabel unnamed = %q", got)
	}
	if got := MarkedLabel(Done, "ship it", 1); got != "✓ ship it" {
		t.Errorf("MarkedLabel = %q", got)
	}
}

func TestSetStage(t *testing.T) {
	t0 := time.Date(2026, 10, 14, 10, 0, 0, 0, time.UTC)
	t1 := t0.Add(time.Hour)
	tk := &Task{Stage: Active, Since: t0}

	tk.SetStage(Paused, "PR #212 review", t1)
	if tk.Stage != Paused || tk.Reason != "PR #212 review" || !tk.Since.Equal(t1) {
		t.Fatalf("pause: %+v", tk)
	}
	tk.SetStage(Paused, "", t1.Add(time.Hour))
	if tk.Reason != "PR #212 review" || !tk.Since.Equal(t1) {
		t.Fatalf("pausing again without a reason should keep both: %+v", tk)
	}
	tk.SetStage(Active, "", t1.Add(2*time.Hour))
	if tk.Reason != "" {
		t.Fatalf("leaving Paused should drop the reason: %+v", tk)
	}
}

func TestLinks(t *testing.T) {
	tk := &Task{}
	now := time.Now()
	tk.AddLink("https://github.com/a/b/pull/1", "", "claude", now)
	tk.AddLink("https://github.com/a/b/pull/1", "", "", now)
	tk.AddLink("https://linear.app/a/issue/X-2", "", "", now)
	if len(tk.Links) != 2 {
		t.Fatalf("duplicate link added: %d links", len(tk.Links))
	}
	if _, err := tk.RemoveLink("3"); err == nil {
		t.Fatal("removing a missing link should fail")
	}
	if l, err := tk.RemoveLink("1"); err != nil || l.By != "claude" {
		t.Fatalf("RemoveLink(1) = %+v, %v", l, err)
	}
	if _, err := tk.RemoveLink("https://linear.app/a/issue/X-2"); err != nil || len(tk.Links) != 0 {
		t.Fatalf("RemoveLink by URL: %v, %d left", err, len(tk.Links))
	}
}

func TestAge(t *testing.T) {
	cases := map[time.Duration]string{
		10 * time.Second: "now",
		4 * time.Minute:  "4m",
		3 * time.Hour:    "3h",
		30 * time.Hour:   "30h",
		72 * time.Hour:   "3d",
	}
	for d, want := range cases {
		if got := Age(d); got != want {
			t.Errorf("Age(%v) = %q, want %q", d, got, want)
		}
	}
}

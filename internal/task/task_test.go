package task

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestLinkLabel(t *testing.T) {
	cases := map[string]string{
		"https://github.com/acme/api/pull/431":           "PR #431",
		"https://github.com/acme/api/issues/12":          "issue #12",
		"https://github.com/acme/api":                    "acme/api",
		"https://linear.app/acme/issue/ABC-418/fix-it":   "ABC-418",
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
		{Active, "blocked", Asking},
		{Active, "idle", YourTurn},
		{Active, "unknown", YourTurn},
		{Paused, "working", GPaused},
		{Review, "working", GReview},
		{Parked, "idle", GParked},
		{Done, "working", GDone},
		{Tool, "blocked", GTool},
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
	if got := MarkedLabel(Review, "", 2); got != "◐ 2" {
		t.Errorf("MarkedLabel review = %q", got)
	}
	if got := TitleFromLabel("◐ ship it", 1); got != "ship it" {
		t.Errorf("review marker not stripped: %q", got)
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
	tk.SetStage(Review, "approval + QA", t1.Add(90*time.Minute))
	if tk.Reason != "approval + QA" {
		t.Fatalf("review should take its own reason: %+v", tk)
	}
	tk.SetStage(Active, "", t1.Add(2*time.Hour))
	if tk.Reason != "" {
		t.Fatalf("going back to Active should drop the reason: %+v", tk)
	}
}

func TestGrouped(t *testing.T) {
	page1 := Link{URL: localPage + "/one"}
	linear := Link{URL: "https://linear.app/acme/issue/ABC-12"}
	slack := Link{URL: slackMsg, Label: "#billing · Dana Reed: can you check…"}
	page2 := Link{URL: artifact}
	other := Link{URL: "https://github.com/acme/web/issues/7"}
	pr := Link{URL: "https://github.com/acme/web/pull/7"}
	in := []Link{page1, linear, slack, page2, other, pr}
	got := Grouped(in)
	if want := []Link{linear, pr, slack, page1, page2, other}; !reflect.DeepEqual(got, want) {
		t.Errorf("Grouped = %+v, want %+v", got, want)
	}
	if want := []Link{page1, linear, slack, page2, other, pr}; !reflect.DeepEqual(in, want) {
		t.Errorf("Grouped changed its input: %+v", in)
	}
	got[0] = other
	if in[1] != linear {
		t.Error("Grouped's result shares its input's array")
	}
}

func TestRemoveLinkByGroupedNumber(t *testing.T) {
	page1 := Link{URL: localPage + "/one"}
	linear := Link{URL: "https://linear.app/acme/issue/ABC-12"}
	tk := &Task{Links: []Link{page1, linear}}
	l, err := tk.RemoveLink("1")
	if err != nil || l.URL != linear.URL {
		t.Fatalf("RemoveLink(1) = %+v, %v; want the Linear issue, shown first", l, err)
	}
	if !reflect.DeepEqual(tk.Links, []Link{page1}) {
		t.Errorf("links = %+v, want the page alone", tk.Links)
	}
	if _, err := tk.RemoveLink("2"); err == nil {
		t.Error("RemoveLink(2) of one link: no error")
	}
}

func TestLinks(t *testing.T) {
	tk := &Task{}
	now := time.Now()
	if _, added := tk.AddLink("https://github.com/a/b/pull/1", "", "claude", now); !added {
		t.Fatal("first link not added")
	}
	if l, added := tk.AddLink("https://github.com/a/b/pull/1", "", "", now); added || l.By != "claude" {
		t.Fatalf("adding it again should return the existing link: %+v, %v", l, added)
	}
	if _, added := tk.AddLink("https://linear.app/a/issue/X-2", "", "", now); !added {
		t.Fatal("second link not added")
	}
	if len(tk.Links) != 2 {
		t.Fatalf("duplicate link added: %d links", len(tk.Links))
	}
	if _, err := tk.RemoveLink("3"); err == nil {
		t.Fatal("removing a missing link should fail")
	}
	// Numbers count in Grouped order: the Linear issue is 1, the PR 2.
	if l, err := tk.RemoveLink("2"); err != nil || l.By != "claude" {
		t.Fatalf("RemoveLink(2) = %+v, %v", l, err)
	}
	if _, err := tk.RemoveLink("https://linear.app/a/issue/X-2"); err != nil || len(tk.Links) != 0 {
		t.Fatalf("RemoveLink by URL: %v, %d left", err, len(tk.Links))
	}
}

func TestLinearID(t *testing.T) {
	cases := map[string]string{
		"https://linear.app/acme/issue/ABC-12/fix-it":                 "ABC-12",
		"https://linear.app/acme/issue/abc-12":                        "ABC-12",
		"https://linear.app/acme/issue/ABC-12?noRedirect=1#comment-1": "ABC-12",
		"https://www.linear.app/acme/issue/ABC-12/":                   "ABC-12",
		"https://linear.app/acme/project/ABC-12":                      "",
		"https://github.com/acme/x/issues/12":                         "",
		"https://notlinear.app/acme/issue/ABC-12":                     "",
		"https://linear.app/acme/issue/fix-it":                        "",
		"https://linear.app/acme/issue/ABC-12/fix-it/more":            "",
		"linear.app/acme/issue/ABC-12":                                "",
		"ftp://linear.app/acme/issue/ABC-12":                          "",
	}
	for in, want := range cases {
		got, ok := LinearID(in)
		if got != want || ok != (want != "") {
			t.Errorf("LinearID(%q) = %q, %v, want %q", in, got, ok, want)
		}
	}
	if org, ok := LinearOrg("https://linear.app/acme/issue/ABC-12/fix-it"); !ok || org != "acme" {
		t.Errorf("LinearOrg = %q, %v, want acme", org, ok)
	}
	if _, ok := LinearOrg("https://linear.app/acme/project/ABC-12"); ok {
		t.Error("LinearOrg should only read issue URLs")
	}
	if got := LinearURL("acme", "abc-12"); got != "https://linear.app/acme/issue/ABC-12" {
		t.Errorf("LinearURL = %q", got)
	}
}

const issueURL = "https://linear.app/acme/issue/ABC-12"

func TestAutoLinkIsIdempotentAcrossURLForms(t *testing.T) {
	tk := &Task{}
	now := time.Now()
	if _, added := tk.AddLink(issueURL, "", "claude", now); !added {
		t.Fatal("first link not added")
	}
	l, added := tk.AddLink(issueURL+"/fix-it", "", "claude", now)
	if added || l.URL != issueURL {
		t.Fatalf("the same issue under another URL should return the existing link: %+v, %v", l, added)
	}
	if len(tk.Links) != 1 {
		t.Fatalf("%d links, want 1", len(tk.Links))
	}
}

func TestRemovedLinearLinkIsDismissed(t *testing.T) {
	tk := &Task{}
	now := time.Now()
	tk.AddLink(issueURL+"/fix-it", "", "claude", now)
	if l, err := tk.RemoveLink("abc-12"); err != nil || l.URL != issueURL+"/fix-it" {
		t.Fatalf("RemoveLink(abc-12) = %+v, %v", l, err)
	}
	if len(tk.Links) != 0 {
		t.Fatalf("%d links left", len(tk.Links))
	}
	if !reflect.DeepEqual(tk.DismissedIssues, []string{"ABC-12"}) {
		t.Fatalf("DismissedIssues = %q", tk.DismissedIssues)
	}
	// Added back by you, then removed again: dismissed again.
	tk.AddLink(issueURL, "", "", now)
	tk.RemoveLink(issueURL)
	if !reflect.DeepEqual(tk.DismissedIssues, []string{"ABC-12"}) {
		t.Fatalf("removing it again should dismiss it again: %q", tk.DismissedIssues)
	}
}

func TestDismissingAnAlreadyDismissedIssueKeepsOneEntry(t *testing.T) {
	// A hand-edited file can link an issue that's dismissed already.
	tk := &Task{
		Links:           []Link{{URL: issueURL + "/fix-it"}},
		DismissedIssues: []string{"ABC-12"},
	}
	if _, err := tk.RemoveLink("1"); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(tk.DismissedIssues, []string{"ABC-12"}) {
		t.Fatalf("dismissing twice should keep one entry: %q", tk.DismissedIssues)
	}
}

func TestDismissReportsWhetherItWasNew(t *testing.T) {
	art := "https://claude.ai/code/artifact/00000000-0000-4000-8000-000000000001"
	tk := &Task{Links: []Link{{URL: art, By: "claude"}}}
	if !tk.Dismiss(art+"?sk=abc") || !tk.Dismiss(issueURL) {
		t.Fatal("Dismiss of a new key = false, want true")
	}
	if tk.Dismiss(art) || tk.Dismiss(issueURL+"/fix-it") {
		t.Fatal("Dismiss of a dismissed key = true, want false")
	}
	if !reflect.DeepEqual(tk.DismissedLinks, []string{"artifact:00000000-0000-4000-8000-000000000001"}) || !reflect.DeepEqual(tk.DismissedIssues, []string{"ABC-12"}) {
		t.Fatalf("DismissedLinks = %q, DismissedIssues = %q", tk.DismissedLinks, tk.DismissedIssues)
	}
	if len(tk.Links) != 1 {
		t.Fatalf("Dismiss changed the links: %+v", tk.Links)
	}
}

func TestRemovingALinearLinkRemovesItsOtherURLs(t *testing.T) {
	pr := Link{URL: "https://github.com/acme/api/pull/431"}
	for _, which := range []string{"abc-12", "1"} {
		t.Run(which, func(t *testing.T) {
			// Before 0.6.0, AddLink deduped only on the exact URL.
			tk := &Task{Links: []Link{{URL: issueURL}, pr, {URL: issueURL + "/fix-it"}}}
			if l, err := tk.RemoveLink(which); err != nil || l.URL != issueURL {
				t.Fatalf("RemoveLink(%s) = %+v, %v", which, l, err)
			}
			if !reflect.DeepEqual(tk.Links, []Link{pr}) {
				t.Fatalf("links left: %+v, want only the PR", tk.Links)
			}
			if !reflect.DeepEqual(tk.DismissedIssues, []string{"ABC-12"}) {
				t.Fatalf("DismissedIssues = %q", tk.DismissedIssues)
			}
		})
	}
}

func TestAgentLinkAddRespectsDismissal(t *testing.T) {
	tk := &Task{}
	now := time.Now()
	tk.AddLink(issueURL, "", "claude", now)
	tk.RemoveLink("1")
	l, added := tk.AddLink(issueURL+"/fix-it", "", "claude", now)
	if added || l != (Link{}) || len(tk.Links) != 0 {
		t.Fatalf("an agent added back a dismissed issue: %+v, %v, %d links", l, added, len(tk.Links))
	}
	if len(tk.DismissedIssues) != 1 {
		t.Fatalf("DismissedIssues = %q", tk.DismissedIssues)
	}
}

func TestAddingItYourselfClearsTheDismissal(t *testing.T) {
	tk := &Task{}
	now := time.Now()
	tk.AddLink(issueURL, "", "claude", now)
	tk.RemoveLink("ABC-12")
	l, added := tk.AddLink(issueURL, "", "", now)
	if !added || l.URL != issueURL || len(tk.Links) != 1 {
		t.Fatalf("AddLink by you = %+v, %v, %d links", l, added, len(tk.Links))
	}
	if len(tk.DismissedIssues) != 0 {
		t.Fatalf("DismissedIssues = %q, want none", tk.DismissedIssues)
	}
}

func TestRemovingAnyLinkDismissesIt(t *testing.T) {
	pr := "https://github.com/acme/api/pull/431"
	tk := &Task{}
	tk.AddLink(pr, "", "claude", time.Now())
	if _, err := tk.RemoveLink("1"); err != nil {
		t.Fatal(err)
	}
	if len(tk.DismissedIssues) != 0 || !reflect.DeepEqual(tk.DismissedLinks, []string{"github-pr:acme/api#431"}) {
		t.Fatalf("DismissedIssues = %q, DismissedLinks = %q, want only the PR", tk.DismissedIssues, tk.DismissedLinks)
	}
	if !tk.Dismissed(pr, "") || !tk.Dismissed(pr+"/files", "") {
		t.Fatal("the removed PR isn't dismissed, under each of its pages")
	}
	if _, err := tk.RemoveLink("ABC-12"); err == nil {
		t.Fatal("removing an issue the task doesn't have should fail")
	}
}

func TestAgentCantReAddARemovedPage(t *testing.T) {
	tk := &Task{}
	now := time.Now()
	if _, added := tk.Add(Link{URL: "http://localhost:4100/showcase", By: "claude", Added: now}); !added {
		t.Fatal("first link not added")
	}
	if _, err := tk.RemoveLink("1"); err != nil {
		t.Fatal(err)
	}
	if l, added := tk.Add(Link{URL: "http://localhost:4100/showcase/", By: "claude", Added: now}); added || l != (Link{}) || len(tk.Links) != 0 {
		t.Fatalf("an agent added back a removed page: %+v, %v, %d links", l, added, len(tk.Links))
	}
	l, added := tk.Add(Link{URL: "http://localhost:4100/showcase/", Added: now})
	if !added || l.URL != "http://localhost:4100/showcase/" || len(tk.Links) != 1 {
		t.Fatalf("Add by you = %+v, %v, %d links", l, added, len(tk.Links))
	}
	if len(tk.DismissedLinks) != 0 {
		t.Fatalf("DismissedLinks = %q, want none", tk.DismissedLinks)
	}
}

func TestRemovingAnArtifactDismissesItsSource(t *testing.T) {
	art := "https://claude.ai/code/artifact/00000000-0000-4000-8000-000000000001"
	now := time.Now()
	tk := &Task{}
	tk.Add(Link{URL: art, Source: "/tmp/p.html", By: "claude", Added: now})
	if _, err := tk.RemoveLink(art + "?sk=abc"); err != nil {
		t.Fatal(err)
	}
	if !tk.Dismissed("file:///tmp/p.html", "") {
		t.Fatal("removing the artifact didn't dismiss its source file")
	}
	if _, added := tk.Add(Link{URL: "file:///tmp/p.html", By: "claude", Added: now}); added {
		t.Fatal("an agent added back the removed artifact's source file")
	}

	// The other way round: removing the file keeps off artifacts made from it.
	tk = &Task{}
	tk.Add(Link{URL: "file:///tmp/p.html", By: "claude", Added: now})
	if _, err := tk.RemoveLink("1"); err != nil {
		t.Fatal(err)
	}
	if !tk.Dismissed(art, "/tmp/p.html") {
		t.Fatal("an artifact made from a removed file isn't dismissed")
	}
	if tk.Dismissed(art, "") || tk.Dismissed(art, "/tmp/other.html") {
		t.Fatal("an artifact from another file is dismissed")
	}
	// Adding the artifact yourself brings its source back too.
	if _, added := tk.Add(Link{URL: art, Source: "/tmp/p.html", Added: now}); !added || len(tk.DismissedLinks) != 0 {
		t.Fatalf("Add by you: added %v, DismissedLinks = %q", added, tk.DismissedLinks)
	}
}

func TestRemovingYourFileTakesItsArtifact(t *testing.T) {
	const art = "https://claude.ai/code/artifact/00000000-0000-4000-8000-000000000001"
	now := time.Now()

	// You linked the file; Claude published it.
	tk := &Task{}
	tk.Add(Link{URL: "file:///tmp/x/p.html", Added: now})
	tk.Add(Link{URL: art, Source: "/tmp/x/p.html", By: "claude", Added: now})
	tk.Add(Link{URL: "https://claude.ai/artifact/other", Source: "/tmp/x/q.html", By: "claude", Added: now})
	if _, err := tk.RemoveLink("file:///tmp/x/p.html"); err != nil {
		t.Fatal(err)
	}
	if len(tk.Links) != 1 || tk.Links[0].URL != "https://claude.ai/artifact/other" {
		t.Fatalf("links = %+v, want only the artifact from another file", tk.Links)
	}
	// Kept off however it comes back: a reply names it without its file.
	if !tk.Dismissed(art, "") || !tk.Dismissed(art+"?sk=abc", "") || !tk.Dismissed(art, "/tmp/x/p.html") {
		t.Fatal("the artifact published from the removed file isn't dismissed")
	}
	if _, added := tk.Add(Link{URL: art + "?sk=abc", By: "claude", Added: now}); added {
		t.Fatal("an agent added back the artifact published from the removed file")
	}

	// An artifact you linked yourself stays, and isn't dismissed.
	tk = &Task{}
	tk.Add(Link{URL: "file:///tmp/x/p.html", Added: now})
	tk.Add(Link{URL: art, Source: "/tmp/x/p.html", Added: now})
	if _, err := tk.RemoveLink("1"); err != nil {
		t.Fatal(err)
	}
	if len(tk.Links) != 1 || tk.Links[0].URL != art || tk.Dismissed(art, "") {
		t.Fatalf("links = %+v, dismissed %q: want your artifact kept", tk.Links, tk.DismissedLinks)
	}
}

func TestAddRefusesControlCharacters(t *testing.T) {
	tk := &Task{}
	for _, u := range []string{"http://localhost:4100/\x1b]8;;x", "http://localhost:4100/%1b", "http://localhost:4100/%c2%9b"} {
		if l, added := tk.Add(Link{URL: u, Added: time.Now()}); added || l != (Link{}) {
			t.Errorf("Add(%q) = %+v, %v, want it refused", u, l, added)
		}
	}
	if len(tk.Links) != 0 {
		t.Fatalf("links = %+v, want none", tk.Links)
	}
}

func TestAddCleansTheLabel(t *testing.T) {
	tk := &Task{}
	l, added := tk.Add(Link{URL: "http://localhost:4100", Label: "Demo\x1b[2J\npage", Added: time.Now()})
	if !added || l.Label != "Demo[2J page" || tk.Links[0].Label != l.Label {
		t.Fatalf("Add = %+v, %v", l, added)
	}
}

func TestNameIsCleaned(t *testing.T) {
	cases := map[string]string{
		"ABC-12 checkout":           "ABC-12 checkout",
		"x\x1b]0;owned\x07\ny":      "x]0;owned y",
		"\x1b\x07":                  "tab 3",
		"":                          "tab 3",
		"Fix \x1b[2Jthe\tlogin\x9b": "Fix [2Jthe login",
	}
	for title, want := range cases {
		if got := (&Task{Title: title}).Name(3); got != want {
			t.Errorf("Name of %q = %q, want %q", title, got, want)
		}
	}
}

func TestGroupOrder(t *testing.T) {
	if Asking.Order() != 0 {
		t.Fatalf("Asking.Order() = %d, want 0", Asking.Order())
	}
	if GDone.Order() <= Running.Order() {
		t.Fatal("done should sort after running")
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

func TestToolTabs(t *testing.T) {
	if s, ok := ParseStage("tool"); !ok || s != Tool || !Done.IsTask() || Tool.IsTask() {
		t.Fatal("tool stage")
	}
	if Tool.Order() <= Done.Order() {
		t.Fatal("tools should sort after done")
	}
	tk := &Task{Stage: Active}
	if !tk.WantsStrip() {
		t.Fatal("tasks show a strip by default")
	}
	tk.Stage = Tool
	if tk.WantsStrip() {
		t.Fatal("tools have no strip by default")
	}
	tk.SetStrip(true)
	if !tk.WantsStrip() {
		t.Fatal("you can turn a tool's strip on")
	}
	tk.Stage = Active
	tk.SetStrip(false)
	if tk.WantsStrip() {
		t.Fatal("you can turn a task's strip off")
	}
}

func TestPriority(t *testing.T) {
	now := time.Now()
	tk := &Task{Stage: Active}
	if err := tk.SetPriority(true); err != nil || !tk.Priority {
		t.Fatalf("priority on: %v %+v", err, tk)
	}
	tk.SetStage(Parked, "", now)
	if !tk.Priority {
		t.Fatal("parking dropped the priority")
	}
	tk.SetStage(Done, "", now)
	if tk.Priority {
		t.Fatal("done kept the priority")
	}
	if err := tk.SetPriority(true); err == nil || tk.Priority {
		t.Fatalf("a done task took priority: %+v", tk)
	}
	tk.SetStage(Active, "", now)
	_ = tk.SetPriority(true)
	tk.SetStage(Tool, "", now)
	if tk.Priority {
		t.Fatal("a tool kept the priority")
	}
	if err := tk.SetPriority(false); err != nil {
		t.Fatalf("turning priority off should always work: %v", err)
	}
}

func TestTaskSlackRoundTrips(t *testing.T) {
	in := Task{TabID: "t1", Workspace: "Slack", Stage: Active, Slack: &SlackAsk{
		Key: "C0123ABCD/1793450000.000100", Channel: "C0123ABCD", Place: "#billing",
		Author: "Dana Reed", TS: "1793450000.000100", ThreadTS: "1793450000.000100",
		At:        time.Date(2026, 11, 2, 9, 0, 0, 0, time.UTC),
		Permalink: "https://acme.slack.com/archives/C0123ABCD/p1793450000000100",
		SessionID: "abc", Research: ResearchLocked,
	}}
	b, err := json.Marshal(in)
	if err != nil {
		t.Fatal(err)
	}
	var out Task
	if err := json.Unmarshal(b, &out); err != nil {
		t.Fatal(err)
	}
	if out.Slack == nil || *out.Slack != *in.Slack {
		t.Fatalf("round trip: %+v", out.Slack)
	}
	plain, _ := json.Marshal(Task{TabID: "t2"})
	if strings.Contains(string(plain), `"slack"`) {
		t.Fatalf("slack key in %s", plain)
	}
}

func TestSlackAskCanStartAndContinue(t *testing.T) {
	for r, want := range map[Research]bool{"": true, ResearchHeld: true, ResearchFailed: true, ResearchLocked: false, ResearchOpen: false} {
		if got := (&SlackAsk{Research: r}).CanStart(); got != want {
			t.Errorf("CanStart(%q) = %v", r, got)
		}
	}
	if (&SlackAsk{Research: ResearchLocked}).CanContinue() {
		t.Error("locked without a session can't continue")
	}
	if !(&SlackAsk{Research: ResearchLocked, SessionID: "abc"}).CanContinue() {
		t.Error("locked with a session can continue")
	}
	if (&SlackAsk{Research: ResearchOpen, SessionID: "abc"}).CanContinue() {
		t.Error("open can't continue")
	}
}

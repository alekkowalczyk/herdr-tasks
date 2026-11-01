package slack

import (
	"strings"
	"testing"
	"time"

	"github.com/alekkowalczyk/herdr-tasks/internal/reconcile"
)

func opts() reconcile.Options {
	return reconcile.Options{
		SlackWorkspace: "Slack", SlackDMs: true, SlackEvery: "5m",
		SlackHours: "Mon-Fri 08:00-18:00", SlackMaxNew: 4,
	}
}

func TestSettingsFrom(t *testing.T) {
	o := opts()
	o.SlackCwd = "~/acme-harness"
	o.SlackSkipChannels = []string{"random"}
	s, err := SettingsFrom(o, "/home/u")
	if err != nil {
		t.Fatal(err)
	}
	if s.Cwd != "/home/u/acme-harness" || s.Every != 5*time.Minute || s.MaxNew != 4 ||
		!s.DMs || s.Workspace != "Slack" || len(s.Skip) != 1 {
		t.Fatalf("settings = %+v", s)
	}
	o.SlackCwd = ""
	if s, _ = SettingsFrom(o, "/home/u"); s.Cwd != "/home/u" {
		t.Fatalf("empty cwd = %q, want home", s.Cwd)
	}
	o.SlackCwd = "/srv/harness"
	if s, _ = SettingsFrom(o, "/home/u"); s.Cwd != "/srv/harness" {
		t.Fatalf("absolute cwd = %q", s.Cwd)
	}
}

func TestSettingsFromRefuses(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(*reconcile.Options)
		want   string
	}{
		{"short every", func(o *reconcile.Options) { o.SlackEvery = "30s" }, "slack_every must be at least 1m"},
		{"bad every", func(o *reconcile.Options) { o.SlackEvery = "soon" }, "slack_every"},
		{"relative cwd", func(o *reconcile.Options) { o.SlackCwd = "relative/dir" }, "slack_cwd must be an absolute path or start with ~/"},
		{"negative max", func(o *reconcile.Options) { o.SlackMaxNew = -1 }, "slack_max_new"},
		{"backwards hours", func(o *reconcile.Options) { o.SlackHours = "Mon-Fri 18:00-08:00" }, "slack_hours"},
	}
	for _, c := range cases {
		o := opts()
		c.mutate(&o)
		_, err := SettingsFrom(o, "/home/u")
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: err = %v, want it to contain %q", c.name, err, c.want)
		}
	}
	o := opts()
	o.SlackMaxNew = 0
	if _, err := SettingsFrom(o, "/home/u"); err != nil {
		t.Errorf("slack_max_new 0 should be allowed: %v", err)
	}
}

func TestSettingsTopic(t *testing.T) {
	o := opts()
	if s, err := SettingsFrom(o, "/home/u"); err != nil || s.Topic != "" {
		t.Fatalf("no topic: %q, %v", s.Topic, err)
	}
	o.SlackTopic = "  billing  "
	if s, err := SettingsFrom(o, "/home/u"); err != nil || s.Topic != "billing" {
		t.Errorf("topic = %q, %v; want it trimmed", s.Topic, err)
	}
	o.SlackTopic = strings.Repeat("ø", 1000)
	if _, err := SettingsFrom(o, "/home/u"); err != nil {
		t.Errorf("1,000 characters: %v", err)
	}
	o.SlackTopic = strings.Repeat("ø", 1001)
	if _, err := SettingsFrom(o, "/home/u"); err == nil || err.Error() != "slack_topic is over 1,000 characters" {
		t.Errorf("1,001 characters: err = %v", err)
	}
}

package slack

import (
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/alekkowalczyk/herdr-tasks/internal/reconcile"
)

// Settings are the slack_* options, checked and ready to use.
type Settings struct {
	Session, Workspace, Cwd string
	DMs                     bool
	Skip                    []string
	Every                   time.Duration
	Hours                   Hours
	MaxNew                  int
	Topic                   string // what asks must be about to make a task; "" takes every ask
}

// maxTopic is the most runes slack_topic may have.
const maxTopic = 1000

// SettingsFrom checks the options. Each error names its setting. home stands
// in for "" and "~/" in slack_cwd.
func SettingsFrom(o reconcile.Options, home string) (Settings, error) {
	every, err := time.ParseDuration(o.SlackEvery)
	if err != nil {
		return Settings{}, fmt.Errorf("slack_every: %w", err)
	}
	if every < time.Minute {
		return Settings{}, fmt.Errorf("slack_every must be at least 1m, got %q", o.SlackEvery)
	}
	cwd := o.SlackCwd
	switch {
	case cwd == "":
		cwd = home
	case cwd == "~":
		cwd = home
	case strings.HasPrefix(cwd, "~/"):
		cwd = filepath.Join(home, cwd[2:])
	case !filepath.IsAbs(cwd):
		return Settings{}, fmt.Errorf("slack_cwd must be an absolute path or start with ~/, got %q", cwd)
	}
	if o.SlackMaxNew < 0 {
		return Settings{}, fmt.Errorf("slack_max_new must be 0 or more, got %d", o.SlackMaxNew)
	}
	hours, err := ParseHours(o.SlackHours)
	if err != nil {
		return Settings{}, fmt.Errorf("slack_hours: %w", err)
	}
	topic := strings.TrimSpace(o.SlackTopic)
	if utf8.RuneCountInString(topic) > maxTopic {
		return Settings{}, errors.New("slack_topic is over 1,000 characters")
	}
	return Settings{
		Session: o.SlackSession, Workspace: o.SlackWorkspace, Cwd: cwd,
		DMs: o.SlackDMs, Skip: o.SlackSkipChannels,
		Every: every, Hours: hours, MaxNew: o.SlackMaxNew, Topic: topic,
	}, nil
}

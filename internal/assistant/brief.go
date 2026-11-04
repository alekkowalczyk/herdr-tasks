package assistant

import (
	"fmt"
	"strings"
	"time"

	"github.com/alekkowalczyk/herdr-tasks/internal/herdr"
	"github.com/alekkowalczyk/herdr-tasks/internal/store"
	"github.com/alekkowalczyk/herdr-tasks/internal/task"
	"github.com/alekkowalczyk/herdr-tasks/internal/view"
)

// maxBrief is how many tasks the brief lists.
const maxBrief = 60

// Brief is what the assistant's hook gives Claude before each of your
// messages: the tasks as they are now, one line each, in the overview's
// order, without tools or hidden spaces.
//
//   - Login loop (w1:t2, Acme): asking, its agent waits for an answer · priority · 3h
func Brief(s *store.State, live *herdr.Live, now time.Time) string {
	var b strings.Builder
	fmt.Fprintf(&b, "The tasks in herdr now (%s). `herdr-tasks context` has their notes, links, and what their agents said.\n", now.Format("Mon Jan 2 15:04"))
	n := 0
	for _, r := range view.Rows(s, live) {
		if r.Group == task.GTool {
			continue
		}
		if n++; n > maxBrief {
			continue
		}
		t := r.Task
		parts := []string{state(r.Group, task.CleanLabel(t.Reason))}
		if t.Priority {
			parts = append(parts, "priority")
		}
		parts = append(parts, task.Age(now.Sub(t.Since)))
		fmt.Fprintf(&b, "- %s (%s, %s): %s\n", t.Name(r.Pos), r.Tab.ID, task.CleanLabel(r.Workspace.Label), strings.Join(parts, " · "))
	}
	switch {
	case n == 0:
		b.WriteString("There are none.\n")
	case n > maxBrief:
		fmt.Fprintf(&b, "And %d more, which `herdr-tasks context` lists.\n", n-maxBrief)
	}
	return b.String()
}

// state says where a task stands, with what it waits for.
func state(g task.Group, reason string) string {
	switch g {
	case task.Asking:
		return "asking, its agent waits for an answer"
	case task.YourTurn:
		return "your turn, its agent is done or there's none"
	case task.Running:
		return "working, its agent is running"
	case task.GReview:
		if reason != "" {
			return "in review, waiting for " + reason
		}
		return "in review"
	case task.GPaused:
		if reason != "" {
			return "paused until " + reason
		}
		return "paused"
	}
	return string(g)
}

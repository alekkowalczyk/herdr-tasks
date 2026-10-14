// Package strip manages the short pane along the bottom of each tab that shows
// the tab's task: stage, note, and links.
//
// A strip is an ordinary herdr pane running `herdr-tasks strip`. It's created
// with pane.split and started by typing the command into its shell, rather
// than as a plugin pane, so it can be set up before herdr has registered the
// plugin and restarted the same way after herdr restores it as a plain shell.
package strip

import (
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"slices"

	"github.com/alekkowalczyk/herdr-tasks/internal/herdr"
)

const (
	// Label marks a pane as a strip.
	Label = "Task"
	// Rows of content a strip shows.
	Rows = 6
	// ParkingLabel names the temporary tabs panes wait in while a strip is added.
	ParkingLabel = "herdr-tasks parking"
)

// Command is what a strip pane's shell runs. The leading space keeps it out
// of shell history.
func Command() string {
	home, _ := os.UserHomeDir()
	bin := filepath.Join(home, ".local", "bin", "herdr-tasks")
	if _, err := os.Stat(bin); err != nil {
		if exe, err := os.Executable(); err == nil {
			bin = exe
		}
	}
	return fmt.Sprintf(" exec '%s' strip", bin)
}

// Host adds strip handling to a herdr client, for reconciliation.
type Host struct{ *herdr.Client }

// Open adds a full-width strip along the bottom of a tab and returns its pane.
//
// herdr can only split a single pane, so in a tab with several panes the
// others are moved out to temporary tabs, the strip is split off the one
// left, and the others are moved back in their original arrangement. Moving
// a pane keeps its process running.
func (h Host) Open(tabID string) (string, error) {
	root, err := h.Layout(tabID)
	if err != nil {
		return "", err
	}
	leaves := root.Leaves()
	if len(leaves) == 0 {
		return "", fmt.Errorf("tab %s has no panes", tabID)
	}
	anchor := leaves[0]
	for _, p := range leaves[1:] {
		if err := h.ParkPane(p, ParkingLabel); err != nil {
			return "", fmt.Errorf("making room for the strip: %w", err)
		}
	}
	pane, err := h.SplitPane(anchor, "down", 0.85, nil)
	if err != nil {
		return "", err
	}
	if err := h.rebuild(tabID, root, anchor); err != nil {
		return pane, fmt.Errorf("putting panes back after adding the strip (look for tabs named %q): %w", ParkingLabel, err)
	}
	_ = h.RenamePane(pane, Label)
	if err := h.SendText(pane, Command()); err != nil {
		return pane, err
	}
	_ = h.Fit(pane)
	return pane, nil
}

// rebuild recreates the layout n inside the region anchor occupies. anchor
// must be n's first leaf.
func (h Host) rebuild(tabID string, n *herdr.Node, anchor string) error {
	if !n.IsSplit() {
		return nil
	}
	if err := h.place(tabID, n.Second, anchor, n.Direction, n.Ratio); err != nil {
		return err
	}
	return h.rebuild(tabID, n.First, anchor)
}

// place moves the panes of n back next to target, keeping n's arrangement.
func (h Host) place(tabID string, n *herdr.Node, target, split string, ratio float64) error {
	first := n.Leaves()[0]
	if err := h.MovePane(first, tabID, target, split, ratio); err != nil {
		return err
	}
	return h.rebuild(tabID, n, first)
}

// Close removes a strip pane.
func (h Host) Close(paneID string) error { return h.ClosePane(paneID) }

// Fit sizes a strip at the bottom of its tab so it shows Rows rows. It
// measures the pane's frame and visible rows together, so running it again
// changes nothing. It leaves a strip alone if you moved it away from the bottom.
func (h Host) Fit(paneID string) error {
	tabID, err := h.PaneTab(paneID)
	if err != nil {
		return err
	}
	root, err := h.Layout(tabID)
	if err != nil {
		return err
	}
	if !root.IsSplit() || root.Direction != "down" || root.Second.IsSplit() || root.Second.PaneID != paneID {
		return nil
	}
	area, rects, err := h.PaneRects(paneID)
	if err != nil {
		return err
	}
	shown, err := h.ViewportRows(paneID)
	if err != nil {
		return err
	}
	r, ok := rects[paneID]
	if !ok || area.Height <= 0 || shown <= 0 {
		return errors.New("strip pane not in its tab's layout")
	}
	if shown == Rows {
		return nil
	}
	frame := r.Height - shown // borders and title around the strip's text
	above := area.Height - (Rows + frame)
	// Aim a quarter row in, so herdr lands on the same row whether it
	// rounds or truncates.
	ratio := (float64(above) + 0.25) / float64(area.Height)
	ratio = math.Max(0.3, math.Min(0.97, ratio))
	if math.Abs(ratio-root.Ratio) < 0.001 {
		return nil
	}
	return h.SetRootRatio(tabID, ratio)
}

// Relaunch starts the strip again in a pane herdr restored after a restart,
// where it comes back as a plain shell.
func (h Host) Relaunch(paneID string) error {
	names, err := h.ForegroundNames(paneID)
	if err != nil {
		return err
	}
	if slices.Contains(names, "herdr-tasks") {
		return nil
	}
	return h.SendText(paneID, Command())
}

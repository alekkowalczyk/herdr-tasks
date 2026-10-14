// Package herdr talks to a running herdr server over its local socket.
package herdr

import (
	"bufio"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"slices"
	"time"
)

// Source identifies this plugin when it reports sidebar metadata.
const Source = "herdr-tasks"

// SocketPath resolves the herdr socket the same way the herdr CLI does:
// HERDR_SOCKET_PATH, then HERDR_SESSION, then the default session.
func SocketPath() string {
	if p := os.Getenv("HERDR_SOCKET_PATH"); p != "" {
		return p
	}
	home, _ := os.UserHomeDir()
	base := filepath.Join(home, ".config", "herdr")
	if s := os.Getenv("HERDR_SESSION"); s != "" {
		return filepath.Join(base, "sessions", s, "herdr.sock")
	}
	return filepath.Join(base, "herdr.sock")
}

// SessionKey names the herdr session a socket belongs to, so each session
// keeps its own task state.
func SessionKey(socketPath string) string {
	dir := filepath.Dir(socketPath)
	if filepath.Base(filepath.Dir(dir)) == "sessions" {
		return filepath.Base(dir)
	}
	return "default"
}

// RunningSessions returns a client for every herdr session of this user
// whose server answers, including the default session.
func RunningSessions() []*Client {
	home, _ := os.UserHomeDir()
	base := filepath.Join(home, ".config", "herdr")
	paths := []string{filepath.Join(base, "herdr.sock")}
	named, _ := filepath.Glob(filepath.Join(base, "sessions", "*", "herdr.sock"))
	paths = append(paths, named...)
	if p := os.Getenv("HERDR_SOCKET_PATH"); p != "" && !slices.Contains(paths, p) {
		paths = append(paths, p)
	}
	var out []*Client
	for _, p := range paths {
		c := &Client{SocketPath: p}
		if _, err := c.Workspaces(); err == nil {
			out = append(out, c)
		}
	}
	return out
}

// Error is an error response from the herdr server.
type Error struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

func (e *Error) Error() string { return fmt.Sprintf("herdr: %s: %s", e.Code, e.Message) }

// Client sends one request per connection to a herdr server.
type Client struct {
	SocketPath string
}

func New() *Client { return &Client{SocketPath: SocketPath()} }

// Call sends a request and decodes its result into out (which may be nil).
func (c *Client) Call(method string, params any, out any) error {
	conn, err := net.DialTimeout("unix", c.SocketPath, 2*time.Second)
	if err != nil {
		return fmt.Errorf("can't reach herdr at %s (is it running?): %w", c.SocketPath, err)
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(10 * time.Second))

	if params == nil {
		params = struct{}{}
	}
	req := map[string]any{"id": "herdr-tasks", "method": method, "params": params}
	if err := json.NewEncoder(conn).Encode(req); err != nil {
		return fmt.Errorf("herdr %s: %w", method, err)
	}
	line, err := bufio.NewReader(conn).ReadBytes('\n')
	if err != nil {
		return fmt.Errorf("herdr %s: %w", method, err)
	}
	var resp struct {
		Result json.RawMessage `json:"result"`
		Error  *Error          `json:"error"`
	}
	if err := json.Unmarshal(line, &resp); err != nil {
		return fmt.Errorf("herdr %s: bad response: %w", method, err)
	}
	if resp.Error != nil {
		return resp.Error
	}
	if out != nil && len(resp.Result) > 0 {
		return json.Unmarshal(resp.Result, out)
	}
	return nil
}

type Workspace struct {
	ID          string `json:"workspace_id"`
	Label       string `json:"label"`
	Number      int    `json:"number"`
	Focused     bool   `json:"focused"`
	ActiveTabID string `json:"active_tab_id"`
}

type Tab struct {
	ID          string `json:"tab_id"`
	WorkspaceID string `json:"workspace_id"`
	Number      int    `json:"number"`
	Label       string `json:"label"`
	Focused     bool   `json:"focused"`
	PaneCount   int    `json:"pane_count"`
	AgentStatus string `json:"agent_status"`
}

func (c *Client) Workspaces() ([]Workspace, error) {
	var r struct {
		Workspaces []Workspace `json:"workspaces"`
	}
	err := c.Call("workspace.list", nil, &r)
	return r.Workspaces, err
}

// Tabs lists every tab in every workspace, in display order.
func (c *Client) Tabs() ([]Tab, error) {
	var r struct {
		Tabs []Tab `json:"tabs"`
	}
	err := c.Call("tab.list", nil, &r)
	return r.Tabs, err
}

func (c *Client) RenameTab(id, label string) error {
	return c.Call("tab.rename", map[string]any{"tab_id": id, "label": label}, nil)
}

// MoveTab moves a tab to a final position in its workspace and returns the
// workspace's tabs in their new order.
func (c *Client) MoveTab(id string, from, to int) ([]Tab, error) {
	// herdr's insert_index counts positions before the tab is taken out.
	index := to
	if from < to {
		index = to + 1
	}
	var r struct {
		Tabs []Tab `json:"tabs"`
	}
	err := c.Call("tab.move", map[string]any{"tab_id": id, "insert_index": index}, &r)
	return r.Tabs, err
}

func (c *Client) FocusTab(id string) error {
	return c.Call("tab.focus", map[string]any{"tab_id": id}, nil)
}

func (c *Client) CloseTab(id string) error {
	return c.Call("tab.close", map[string]any{"tab_id": id}, nil)
}

// PaneTab returns the tab a pane currently lives in.
func (c *Client) PaneTab(paneID string) (string, error) {
	var r struct {
		Pane struct {
			TabID string `json:"tab_id"`
		} `json:"pane"`
	}
	if err := c.Call("pane.get", map[string]any{"pane_id": paneID}, &r); err != nil {
		return "", err
	}
	return r.Pane.TabID, nil
}

// ReportWorkspaceTokens sets sidebar tokens for a workspace. A nil value clears that token.
func (c *Client) ReportWorkspaceTokens(workspaceID string, tokens map[string]*string) error {
	return c.Call("workspace.report_metadata", map[string]any{
		"workspace_id": workspaceID,
		"source":       Source,
		"tokens":       tokens,
	}, nil)
}

func (c *Client) ReloadConfig() error { return c.Call("server.reload_config", nil, nil) }

func (c *Client) DisablePlugin(id string) error {
	return c.Call("plugin.disable", map[string]any{"plugin_id": id}, nil)
}

func (c *Client) OpenPluginPane(pluginID, entrypoint string) error {
	return c.Call("plugin.pane.open", map[string]any{
		"plugin_id":  pluginID,
		"entrypoint": entrypoint,
		"focus":      true,
	}, nil)
}

// Live is one read of herdr's workspaces and tabs.
type Live struct {
	Workspaces []Workspace
	Tabs       []Tab
}

func (c *Client) Live() (*Live, error) {
	ws, err := c.Workspaces()
	if err != nil {
		return nil, err
	}
	tabs, err := c.Tabs()
	if err != nil {
		return nil, err
	}
	return &Live{Workspaces: ws, Tabs: tabs}, nil
}

func (l *Live) Workspace(id string) (Workspace, bool) {
	for _, w := range l.Workspaces {
		if w.ID == id {
			return w, true
		}
	}
	return Workspace{}, false
}

func (l *Live) Tab(id string) (Tab, bool) {
	for _, t := range l.Tabs {
		if t.ID == id {
			return t, true
		}
	}
	return Tab{}, false
}

// TabsIn returns a workspace's tabs in display order.
func (l *Live) TabsIn(workspaceID string) []Tab {
	var out []Tab
	for _, t := range l.Tabs {
		if t.WorkspaceID == workspaceID {
			out = append(out, t)
		}
	}
	return out
}

// Position is a tab's 1-based place in its workspace's tab bar, or 0 if unknown.
func (l *Live) Position(tabID string) int {
	t, ok := l.Tab(tabID)
	if !ok {
		return 0
	}
	for i, other := range l.TabsIn(t.WorkspaceID) {
		if other.ID == tabID {
			return i + 1
		}
	}
	return 0
}

// FocusedTab is the selected tab of the focused workspace.
func (l *Live) FocusedTab() (Tab, bool) {
	for _, w := range l.Workspaces {
		if w.Focused {
			if t, ok := l.Tab(w.ActiveTabID); ok {
				return t, true
			}
		}
	}
	for _, t := range l.Tabs {
		if t.Focused {
			return t, true
		}
	}
	return Tab{}, false
}

type Pane struct {
	ID          string `json:"pane_id"`
	TabID       string `json:"tab_id"`
	WorkspaceID string `json:"workspace_id"`
	Label       string `json:"label"`
	Focused     bool   `json:"focused"`
}

// Panes lists every pane in every workspace.
func (c *Client) Panes() ([]Pane, error) {
	var r struct {
		Panes []Pane `json:"panes"`
	}
	err := c.Call("pane.list", nil, &r)
	return r.Panes, err
}

// Node is one node of a tab's layout tree: a pane, or a split of two nodes.
type Node struct {
	Type      string  `json:"type"`
	PaneID    string  `json:"pane_id,omitempty"`
	Direction string  `json:"direction,omitempty"`
	Ratio     float64 `json:"ratio,omitempty"`
	First     *Node   `json:"first,omitempty"`
	Second    *Node   `json:"second,omitempty"`
}

func (n *Node) IsSplit() bool { return n.Type == "split" }

// Leaves are the panes under n, left to right and top to bottom.
func (n *Node) Leaves() []string {
	if n == nil {
		return nil
	}
	if !n.IsSplit() {
		return []string{n.PaneID}
	}
	return append(n.First.Leaves(), n.Second.Leaves()...)
}

func (c *Client) Layout(tabID string) (*Node, error) {
	var r struct {
		Layout struct {
			Root *Node `json:"root"`
		} `json:"layout"`
	}
	if err := c.Call("layout.export", map[string]any{"tab_id": tabID}, &r); err != nil {
		return nil, err
	}
	if r.Layout.Root == nil {
		return nil, fmt.Errorf("tab %s has no layout", tabID)
	}
	return r.Layout.Root, nil
}

type Rect struct {
	X, Y, Width, Height int
}

// PaneRects returns the tab area and each pane's rectangle for a pane's tab.
func (c *Client) PaneRects(paneID string) (Rect, map[string]Rect, error) {
	var r struct {
		Layout struct {
			Area  Rect `json:"area"`
			Panes []struct {
				PaneID string `json:"pane_id"`
				Rect   Rect   `json:"rect"`
			} `json:"panes"`
		} `json:"layout"`
	}
	if err := c.Call("pane.layout", map[string]any{"pane_id": paneID}, &r); err != nil {
		return Rect{}, nil, err
	}
	rects := map[string]Rect{}
	for _, p := range r.Layout.Panes {
		rects[p.PaneID] = p.Rect
	}
	return r.Layout.Area, rects, nil
}

// SplitPane splits target and returns the new pane, which starts a shell.
func (c *Client) SplitPane(target, direction string, ratio float64, env map[string]string) (string, error) {
	var r struct {
		Pane struct {
			ID string `json:"pane_id"`
		} `json:"pane"`
	}
	params := map[string]any{"target_pane_id": target, "direction": direction, "ratio": ratio, "focus": false}
	if len(env) > 0 {
		params["env"] = env
	}
	err := c.Call("pane.split", params, &r)
	return r.Pane.ID, err
}

// ParkPane moves a pane out into a tab of its own.
func (c *Client) ParkPane(paneID, label string) error {
	return c.Call("pane.move", map[string]any{
		"pane_id":     paneID,
		"destination": map[string]any{"type": "new_tab", "label": label},
		"focus":       false,
	}, nil)
}

// MovePane moves a pane next to target in tabID.
func (c *Client) MovePane(paneID, tabID, target, split string, ratio float64) error {
	return c.Call("pane.move", map[string]any{
		"pane_id": paneID,
		"destination": map[string]any{
			"type": "tab", "tab_id": tabID, "target_pane_id": target, "split": split, "ratio": ratio,
		},
		"focus": false,
	}, nil)
}

// SetRootRatio sets the ratio of a tab's top-level split.
func (c *Client) SetRootRatio(tabID string, ratio float64) error {
	return c.Call("layout.set_split_ratio", map[string]any{"tab_id": tabID, "path": []bool{}, "ratio": ratio}, nil)
}

func (c *Client) RenamePane(paneID, label string) error {
	return c.Call("pane.rename", map[string]any{"pane_id": paneID, "label": label}, nil)
}

func (c *Client) ClosePane(paneID string) error {
	return c.Call("pane.close", map[string]any{"pane_id": paneID}, nil)
}

// SendText types text into a pane, followed by Enter.
func (c *Client) SendText(paneID, text string) error {
	return c.Call("pane.send_input", map[string]any{"pane_id": paneID, "text": text, "keys": []string{"enter"}}, nil)
}

// ForegroundNames lists the names of the processes in a pane's foreground.
func (c *Client) ForegroundNames(paneID string) ([]string, error) {
	var r struct {
		Info struct {
			Procs []struct {
				Name string `json:"name"`
			} `json:"foreground_processes"`
		} `json:"process_info"`
	}
	if err := c.Call("pane.process_info", map[string]any{"pane_id": paneID}, &r); err != nil {
		return nil, err
	}
	var names []string
	for _, p := range r.Info.Procs {
		names = append(names, p.Name)
	}
	return names, nil
}

func (c *Client) TabGet(id string) (Tab, error) {
	var r struct {
		Tab Tab `json:"tab"`
	}
	err := c.Call("tab.get", map[string]any{"tab_id": id}, &r)
	return r.Tab, err
}

// ViewportRows is how many rows of text a pane shows.
func (c *Client) ViewportRows(paneID string) (int, error) {
	var r struct {
		Pane struct {
			Scroll struct {
				Rows int `json:"viewport_rows"`
			} `json:"scroll"`
		} `json:"pane"`
	}
	err := c.Call("pane.get", map[string]any{"pane_id": paneID}, &r)
	return r.Pane.Scroll.Rows, err
}

// Package herdr talks to a running herdr server over its local socket.
package herdr

import (
	"bufio"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"path/filepath"
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

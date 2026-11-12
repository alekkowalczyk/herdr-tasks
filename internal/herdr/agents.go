package herdr

import "time"

// Created is a workspace or tab herdr just made, with its first pane.
type Created struct{ WorkspaceID, TabID, PaneID string }

type createdReply struct {
	Workspace struct {
		ID string `json:"workspace_id"`
	} `json:"workspace"`
	Tab struct {
		ID          string `json:"tab_id"`
		WorkspaceID string `json:"workspace_id"`
	} `json:"tab"`
	Root struct {
		ID string `json:"pane_id"`
	} `json:"root_pane"`
}

// CreateWorkspace makes a workspace without focusing it. herdr names it,
// and picks its folder, when label or cwd is "".
func (c *Client) CreateWorkspace(label, cwd string) (Created, error) {
	var r createdReply
	err := c.Call("workspace.create", created(map[string]any{"focus": false}, label, cwd), &r)
	return Created{WorkspaceID: r.Workspace.ID, TabID: r.Tab.ID, PaneID: r.Root.ID}, err
}

// CreateTab makes a tab in a workspace without focusing it, named and
// placed as CreateWorkspace's.
func (c *Client) CreateTab(workspaceID, label, cwd string) (Created, error) {
	var r createdReply
	err := c.Call("tab.create", created(map[string]any{"workspace_id": workspaceID, "focus": false}, label, cwd), &r)
	ws := r.Tab.WorkspaceID
	if ws == "" {
		ws = workspaceID
	}
	return Created{WorkspaceID: ws, TabID: r.Tab.ID, PaneID: r.Root.ID}, err
}

// created adds a label and a folder to params, leaving out either when "".
func created(params map[string]any, label, cwd string) map[string]any {
	if label != "" {
		params["label"] = label
	}
	if cwd != "" {
		params["cwd"] = cwd
	}
	return params
}

// StartAgent starts an agent in a pane at its shell prompt and returns once
// the agent is ready for input. herdr waits up to timeout for that, so the
// call's own deadline is longer.
func (c *Client) StartAgent(name, kind, paneID string, args []string, timeout time.Duration) error {
	return c.callFor(timeout+5*time.Second, "agent.start", map[string]any{
		"name": name, "kind": kind, "pane_id": paneID, "args": args,
		"timeout_ms": timeout.Milliseconds(),
	}, nil)
}

// Process is a process in a pane's foreground.
type Process struct {
	PID  int      `json:"pid"`
	Name string   `json:"name"`
	Argv []string `json:"argv"`
}

// Processes lists the processes in a pane's foreground.
func (c *Client) Processes(paneID string) ([]Process, error) {
	var r struct {
		Info struct {
			Procs []Process `json:"foreground_processes"`
		} `json:"process_info"`
	}
	err := c.Call("pane.process_info", map[string]any{"pane_id": paneID}, &r)
	return r.Info.Procs, err
}

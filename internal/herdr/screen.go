package herdr

// LayoutPane is one pane of a tab's layout, and where herdr draws it.
type LayoutPane struct {
	PaneID  string `json:"pane_id"`
	Focused bool   `json:"focused"`
	Rect    Rect   `json:"rect"`
}

// TabLayout is how herdr lays out a tab's panes. When Zoomed, the Focused
// pane fills Area, though Panes keep their unzoomed rects.
type TabLayout struct {
	TabID   string       `json:"tab_id"`
	Zoomed  bool         `json:"zoomed"`
	Focused string       `json:"focused_pane_id"`
	Area    Rect         `json:"area"`
	Panes   []LayoutPane `json:"panes"`
}

// TabLayout returns the layout of the tab a pane is in.
func (c *Client) TabLayout(paneID string) (*TabLayout, error) {
	var r struct {
		Layout TabLayout `json:"layout"`
	}
	if err := c.Call("pane.layout", map[string]any{"pane_id": paneID}, &r); err != nil {
		return nil, err
	}
	return &r.Layout, nil
}

// ReadScreen returns what a pane shows, as ANSI text: rows joined by \r\n,
// styled with SGR codes only, and trailing blank rows left out. If the pane
// is scrolled back, it's the rows scrolled to.
func (c *Client) ReadScreen(paneID string) (string, error) {
	var r struct {
		Read struct {
			Text string `json:"text"`
		} `json:"read"`
	}
	err := c.Call("pane.read", map[string]any{"pane_id": paneID, "source": "visible", "format": "ansi", "strip_ansi": false}, &r)
	return r.Read.Text, err
}

// TypeText writes text to a pane as it is, with no Enter. SendText adds one.
func (c *Client) TypeText(paneID, text string) error {
	return c.Call("pane.send_text", map[string]any{"pane_id": paneID, "text": text}, nil)
}

// SendKeys presses keys in a pane, by herdr's names for them, such as
// "enter" or "ctrl+c". herdr checks them all first, and presses none if one
// is wrong.
func (c *Client) SendKeys(paneID string, keys ...string) error {
	if keys == nil {
		keys = []string{}
	}
	return c.Call("pane.send_keys", map[string]any{"pane_id": paneID, "keys": keys}, nil)
}

// Paste pastes text into a pane: herdr wraps it in bracketed paste when the
// pane has that turned on.
func (c *Client) Paste(paneID, text string) error {
	return c.Call("pane.send_input", map[string]any{"pane_id": paneID, "text": text}, nil)
}

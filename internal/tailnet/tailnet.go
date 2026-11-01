// Package tailnet talks to the tailscale command: it reads this machine's
// tailnet name and owner, and the serve entries, and adds or removes one.
package tailnet

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"runtime"
	"strconv"
	"strings"
	"time"
)

// ErrNoCLI means there's no tailscale command to run.
var ErrNoCLI = errors.New("the tailscale command isn't installed")

// appPath is where the Mac app keeps its command, tried after PATH. It's a
// variable so tests can point it at a fake.
var appPath = func() string {
	if runtime.GOOS == "darwin" {
		return "/Applications/Tailscale.app/Contents/MacOS/Tailscale"
	}
	return ""
}()

const defaultTimeout = 10 * time.Second

// CLI runs the tailscale command.
type CLI struct {
	// Bin is the command to run. "" means tailscale on PATH, then the Mac
	// app's command.
	Bin string
	// Timeout bounds each call. 0 means 10 seconds.
	Timeout time.Duration
}

// Self is this machine as the tailnet knows it.
type Self struct {
	// DNSName is the machine's name on the tailnet, without the trailing dot.
	DNSName string
	// Owner is the login of the user the machine belongs to. It's "" for a
	// tagged machine, which has no user.
	Owner string
}

// Self reads this machine's name and owner from `status --json`.
func (c CLI) Self(ctx context.Context) (Self, error) {
	out, err := c.run(ctx, "status", "--json")
	if err != nil {
		return Self{}, err
	}
	var st struct {
		BackendState string
		Self         struct {
			DNSName string
			UserID  int64
			Tags    []string
		}
		User map[string]struct{ LoginName string }
	}
	if err := json.Unmarshal(out, &st); err != nil {
		return Self{}, fmt.Errorf("reading tailscale status: %w", err)
	}
	if st.BackendState != "Running" {
		return Self{}, fmt.Errorf("Tailscale isn't running (%s)", st.BackendState)
	}
	self := Self{DNSName: strings.TrimSuffix(st.Self.DNSName, ".")}
	if len(st.Self.Tags) == 0 {
		self.Owner = st.User[strconv.FormatInt(st.Self.UserID, 10)].LoginName
	}
	return self, nil
}

// Served reads `serve status --json`. It maps each HTTPS port to the target
// it proxies to, or "" when the port serves something else.
func (c CLI) Served(ctx context.Context) (map[int]string, error) {
	out, err := c.run(ctx, "serve", "status", "--json")
	if err != nil {
		return nil, err
	}
	var cfg struct {
		TCP map[string]struct{ HTTPS bool }
		Web map[string]struct {
			Handlers map[string]struct{ Proxy string }
		}
	}
	if len(bytes.TrimSpace(out)) > 0 {
		if err := json.Unmarshal(out, &cfg); err != nil {
			return nil, fmt.Errorf("reading tailscale serve status: %w", err)
		}
	}
	targets := map[int]string{}
	for key, tcp := range cfg.TCP {
		port, err := strconv.Atoi(key)
		if err != nil || !tcp.HTTPS {
			continue
		}
		targets[port] = ""
	}
	for hostPort, web := range cfg.Web {
		i := strings.LastIndex(hostPort, ":")
		if i < 0 {
			continue
		}
		port, err := strconv.Atoi(hostPort[i+1:])
		if _, ok := targets[port]; err != nil || !ok {
			continue
		}
		targets[port] = web.Handlers["/"].Proxy
	}
	return targets, nil
}

// Serve shares target on the tailnet over HTTPS at port, and keeps it shared
// after this command returns.
func (c CLI) Serve(ctx context.Context, port int, target string) error {
	_, err := c.run(ctx, "serve", "--bg", "--https="+strconv.Itoa(port), target)
	return err
}

// Off stops sharing the HTTPS port.
func (c CLI) Off(ctx context.Context, port int) error {
	_, err := c.run(ctx, "serve", "--https="+strconv.Itoa(port), "off")
	return err
}

// bin finds the command to run.
func (c CLI) bin() (string, error) {
	if c.Bin != "" {
		if _, err := os.Stat(c.Bin); err != nil {
			return "", ErrNoCLI
		}
		return c.Bin, nil
	}
	if p, err := exec.LookPath("tailscale"); err == nil {
		return p, nil
	}
	if appPath != "" {
		if _, err := os.Stat(appPath); err == nil {
			return appPath, nil
		}
	}
	return "", ErrNoCLI
}

// run runs the command with the timeout and returns its standard output. A
// failure carries the command's output.
func (c CLI) run(ctx context.Context, args ...string) ([]byte, error) {
	bin, err := c.bin()
	if err != nil {
		return nil, err
	}
	timeout := c.Timeout
	if timeout == 0 {
		timeout = defaultTimeout
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	cmd := exec.CommandContext(ctx, bin, args...)
	// Without this a child that keeps the pipes open would hold Wait past the
	// timeout.
	cmd.WaitDelay = time.Second
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		msg := strings.TrimSpace(stderr.String() + "\n" + stdout.String())
		if ctx.Err() != nil {
			msg = "timed out after " + timeout.String()
		}
		if strings.Contains(msg, "Access denied") || strings.Contains(msg, "operator") {
			msg += " (run: sudo tailscale set --operator=$USER)"
		}
		if msg == "" {
			msg = err.Error()
		}
		return nil, fmt.Errorf("tailscale %s: %s", strings.Join(args, " "), msg)
	}
	return stdout.Bytes(), nil
}

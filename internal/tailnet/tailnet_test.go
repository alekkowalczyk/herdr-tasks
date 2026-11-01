package tailnet

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

// fakeTailscale writes a script that logs its arguments, prints status for
// `status --json` and serve for `serve status --json`, and exits with exit.
func fakeTailscale(t *testing.T, status, serve string, exit int) (bin, log string) {
	t.Helper()
	dir := t.TempDir()
	log = filepath.Join(dir, "log")
	write := func(name, s string) string {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte(s), 0o600); err != nil {
			t.Fatal(err)
		}
		return p
	}
	statusFile := write("status.json", status)
	serveFile := write("serve.json", serve)
	script := "#!/bin/sh\n" +
		"echo \"$*\" >> '" + log + "'\n" +
		"case \"$*\" in\n" +
		"'status --json') cat '" + statusFile + "';;\n" +
		"'serve status --json') cat '" + serveFile + "';;\n" +
		"esac\n" +
		"exit " + string(rune('0'+exit)) + "\n"
	bin = filepath.Join(dir, "tailscale")
	if err := os.WriteFile(bin, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	return bin, log
}

const runningStatus = `{"BackendState":"Running","Self":{"DNSName":"devbox.tail0000.ts.net.","UserID":42%s},
"User":{"42":{"LoginName":"dana@acme.test"},"43":{"LoginName":"rui@acme.test"}}}`

func status(extra string) string { return strings.Replace(runningStatus, "%s", extra, 1) }

func TestSelfReadsNameAndOwner(t *testing.T) {
	bin, _ := fakeTailscale(t, status(""), "", 0)
	got, err := CLI{Bin: bin}.Self(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if want := (Self{"devbox.tail0000.ts.net", "dana@acme.test"}); got != want {
		t.Fatalf("got %+v, want %+v", got, want)
	}
}

func TestSelfTaggedHasNoOwner(t *testing.T) {
	bin, _ := fakeTailscale(t, status(`,"Tags":["tag:server"]`), "", 0)
	got, err := CLI{Bin: bin}.Self(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if got.Owner != "" || got.DNSName != "devbox.tail0000.ts.net" {
		t.Fatalf("got %+v", got)
	}
}

func TestSelfNotRunning(t *testing.T) {
	bin, _ := fakeTailscale(t, `{"BackendState":"Stopped"}`, "", 0)
	_, err := CLI{Bin: bin}.Self(context.Background())
	if err == nil || !strings.Contains(err.Error(), "Tailscale isn't running (Stopped)") {
		t.Fatalf("err = %v", err)
	}
}

func TestServedReadsTargets(t *testing.T) {
	serve := `{"TCP":{"443":{"HTTPS":true},"7391":{"HTTPS":true}},"Web":{"devbox.tail0000.ts.net:443":{"Handlers":{"/":{"Proxy":"http://127.0.0.1:8800"}}},"devbox.tail0000.ts.net:7391":{"Handlers":{"/":{"Proxy":"unix:/s/board.sock"}}}}}`
	bin, _ := fakeTailscale(t, "", serve, 0)
	got, err := CLI{Bin: bin}.Served(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	want := map[int]string{443: "http://127.0.0.1:8800", 7391: "unix:/s/board.sock"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v, want %v", got, want)
	}
}

func TestServedNonProxyPortsMapToEmpty(t *testing.T) {
	serve := `{"TCP":{"443":{"HTTPS":true},"8443":{"HTTPS":true},"9000":{"TCPForward":"127.0.0.1:1"}},"Web":{"devbox.tail0000.ts.net:443":{"Handlers":{"/":{"Path":"/tmp/x"}}}}}`
	bin, _ := fakeTailscale(t, "", serve, 0)
	got, err := CLI{Bin: bin}.Served(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	want := map[int]string{443: "", 8443: ""}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v, want %v", got, want)
	}
}

func TestServedNothingServed(t *testing.T) {
	bin, _ := fakeTailscale(t, "", `{}`, 0)
	got, err := CLI{Bin: bin}.Served(context.Background())
	if err != nil || len(got) != 0 {
		t.Fatalf("got %v, %v", got, err)
	}
}

func TestServeAndOffArgs(t *testing.T) {
	bin, log := fakeTailscale(t, "", "", 0)
	c := CLI{Bin: bin}
	if err := c.Serve(context.Background(), 7391, "unix:/s/board.sock"); err != nil {
		t.Fatal(err)
	}
	if err := c.Off(context.Background(), 7391); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(log)
	want := "serve --bg --https=7391 unix:/s/board.sock\nserve --https=7391 off\n"
	if string(b) != want {
		t.Fatalf("log = %q, want %q", b, want)
	}
}

func TestServeAccessDenied(t *testing.T) {
	dir := t.TempDir()
	bin := filepath.Join(dir, "tailscale")
	script := "#!/bin/sh\necho 'Access denied: serve config denied'\nexit 1\n"
	if err := os.WriteFile(bin, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	err := CLI{Bin: bin}.Serve(context.Background(), 7391, "unix:/s/board.sock")
	if err == nil || !strings.Contains(err.Error(), "tailscale set --operator") ||
		!strings.Contains(err.Error(), "Access denied: serve config denied") {
		t.Fatalf("err = %v", err)
	}
}

func TestCallTimesOut(t *testing.T) {
	dir := t.TempDir()
	bin := filepath.Join(dir, "tailscale")
	// The shell is killed but its sleep keeps the output pipes open, which
	// is what WaitDelay is for.
	if err := os.WriteFile(bin, []byte("#!/bin/sh\nsleep 5\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	err := CLI{Bin: bin, Timeout: 200 * time.Millisecond}.Off(context.Background(), 7391)
	if err == nil {
		t.Fatal("want an error")
	}
	if d := time.Since(start); d > 2*time.Second {
		t.Fatalf("took %v", d)
	}
	if !strings.Contains(err.Error(), "timed out") {
		t.Fatalf("err = %v", err)
	}
}

func TestCallCancelledIsNotATimeout(t *testing.T) {
	dir := t.TempDir()
	bin := filepath.Join(dir, "tailscale")
	if err := os.WriteFile(bin, []byte("#!/bin/sh\nsleep 5\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	time.AfterFunc(200*time.Millisecond, cancel)
	start := time.Now()
	err := CLI{Bin: bin}.Off(ctx, 7391)
	if !errors.Is(err, context.Canceled) || strings.Contains(err.Error(), "timed out") {
		t.Fatalf("err = %v", err)
	}
	if d := time.Since(start); d > 2*time.Second {
		t.Fatalf("took %v", d)
	}
}

func TestOperatorHintNotRepeated(t *testing.T) {
	bin := filepath.Join(t.TempDir(), "tailscale")
	script := "#!/bin/sh\necho 'Access denied: use sudo tailscale set --operator=$USER'\nexit 1\n"
	if err := os.WriteFile(bin, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	err := CLI{Bin: bin}.Off(context.Background(), 7391)
	if err == nil || strings.Count(err.Error(), "tailscale set --operator") != 1 {
		t.Fatalf("err = %v", err)
	}
}

func TestNoCLI(t *testing.T) {
	bin := filepath.Join(t.TempDir(), "missing")
	if _, err := (CLI{Bin: bin}).Served(context.Background()); !errors.Is(err, ErrNoCLI) {
		t.Fatalf("err = %v", err)
	}
}

// The empty Bin looks on PATH first, then at the app's path. Both are
// pointed at fakes so the real CLI is never reached.
func TestLookupOrder(t *testing.T) {
	pathDir := t.TempDir()
	app, _ := fakeTailscale(t, "", "", 0)
	old := appPath
	appPath = app
	t.Cleanup(func() { appPath = old })

	t.Setenv("PATH", pathDir)
	if got, err := (CLI{}).bin(); err != nil || got != app {
		t.Fatalf("fallback: got %q, %v", got, err)
	}

	onPath := filepath.Join(pathDir, "tailscale")
	if err := os.WriteFile(onPath, []byte("#!/bin/sh\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	if got, err := (CLI{}).bin(); err != nil || got != onPath {
		t.Fatalf("PATH: got %q, %v", got, err)
	}

	appPath = filepath.Join(pathDir, "nowhere")
	if err := os.Remove(onPath); err != nil {
		t.Fatal(err)
	}
	if _, err := (CLI{}).bin(); !errors.Is(err, ErrNoCLI) {
		t.Fatalf("none: err = %v", err)
	}
}

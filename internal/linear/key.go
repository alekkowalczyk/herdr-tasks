package linear

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"time"

	"github.com/alekkowalczyk/herdr-tasks/internal/store"
)

var (
	ErrNoKey    = errors.New("no Linear API key")
	ErrKeychain = errors.New("the macOS keychain isn't available")
)

// keyFormat is a personal API key. It also keeps what goes to `security -i`
// to characters that need no quoting.
var keyFormat = regexp.MustCompile(`^lin_api_[A-Za-z0-9]+$`)

// The key's keychain item.
const (
	service  = "herdr-tasks"
	account  = "linear-api-key"
	notFound = 44 // security's exit status when there's no such item
)

// useKeychain is whether the key lives in the macOS keychain. Elsewhere it's
// a file only the user can read.
var useKeychain = runtime.GOOS == "darwin"

// runSecurity runs /usr/bin/security with stdin and returns its stdout and
// exit status. err is for when it couldn't run at all. A locked keychain can
// show an unlock dialog, so it gives up after a minute rather than wait on it
// forever.
var runSecurity = func(stdin string, args ...string) (stdout string, code int, err error) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, "/usr/bin/security", args...)
	cmd.Stdin = strings.NewReader(stdin)
	var out bytes.Buffer
	cmd.Stdout = &out
	err = cmd.Run()
	var exit *exec.ExitError
	if errors.As(err, &exit) {
		return out.String(), exit.ExitCode(), nil
	}
	if err != nil {
		return "", -1, err
	}
	return out.String(), 0, nil
}

// keyFile is where the key is kept off macOS.
var keyFile = func() string { return filepath.Join(store.Dir(), "linear-api-key") }

// LoadKey reads the saved key, or returns ErrNoKey.
func LoadKey() (string, error) {
	if useKeychain {
		out, code, err := runSecurity("", "find-generic-password", "-s", service, "-a", account, "-w")
		if err == nil && code == notFound {
			return "", ErrNoKey
		}
		if err := keychainErr(code, err); err != nil {
			return "", err
		}
		return keyOrNone(out)
	}
	f, err := os.Open(keyFile())
	if errors.Is(err, fs.ErrNotExist) {
		return "", ErrNoKey
	}
	if err != nil {
		return "", err
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		return "", err
	}
	if fi.Mode().Perm()&0o077 != 0 {
		return "", fmt.Errorf("%s can be read by other users; run chmod 600 on it", keyFile())
	}
	b, err := io.ReadAll(io.LimitReader(f, 4096))
	if err != nil {
		return "", err
	}
	return keyOrNone(string(b))
}

func keyOrNone(s string) (string, error) {
	if s = strings.TrimSpace(s); s == "" {
		return "", ErrNoKey
	}
	return s, nil
}

// SaveKey saves a personal API key, replacing any saved one. Space around
// it is dropped.
func SaveKey(key string) error {
	key = strings.TrimSpace(key)
	if !keyFormat.MatchString(key) {
		return errors.New("that isn't a Linear API key; they start with lin_api_ and have only letters and digits")
	}
	if useKeychain {
		// With -i the command comes in on stdin, so the key stays out of
		// argv, where any user could see it.
		_, code, err := runSecurity("add-generic-password -U -s "+service+" -a "+account+" -w "+key+"\n", "-i")
		return keychainErr(code, err)
	}
	return writeKeyFile(keyFile(), key)
}

// writeKeyFile writes the key to a new 0600 temp file and renames it into place.
func writeKeyFile(path, key string) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	f, err := os.CreateTemp(dir, ".linear-api-key-*") // O_EXCL, mode 0600
	if err != nil {
		return err
	}
	_, err = f.WriteString(key + "\n")
	if err == nil {
		err = f.Sync()
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		err = os.Rename(f.Name(), path)
	}
	if err != nil {
		os.Remove(f.Name())
	}
	return err
}

// DeleteKey removes the saved key. It's not an error when there's none.
func DeleteKey() error {
	if useKeychain {
		_, code, err := runSecurity("", "delete-generic-password", "-s", service, "-a", account)
		if err == nil && code == notFound {
			return nil
		}
		return keychainErr(code, err)
	}
	if err := os.Remove(keyFile()); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	return nil
}

// keychainErr is ErrKeychain when security couldn't run or failed. A locked
// keychain, as over SSH, is one way it fails.
func keychainErr(code int, err error) error {
	if err != nil {
		return fmt.Errorf("%w: %v", ErrKeychain, err)
	}
	if code != 0 {
		return fmt.Errorf("%w (security exited with %d)", ErrKeychain, code)
	}
	return nil
}

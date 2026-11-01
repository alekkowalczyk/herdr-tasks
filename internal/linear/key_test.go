package linear

import (
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// securityCall is one run of /usr/bin/security.
type securityCall struct {
	stdin string
	args  []string
}

// fakeKeychain stands in for /usr/bin/security: each run gets the next exit
// code from codes (0 once they run out) and prints out.
func fakeKeychain(t *testing.T, out string, codes ...int) *[]securityCall {
	t.Helper()
	calls := &[]securityCall{}
	oldUse, oldRun := useKeychain, runSecurity
	t.Cleanup(func() { useKeychain, runSecurity = oldUse, oldRun })
	useKeychain = true
	runSecurity = func(stdin string, args ...string) (string, int, error) {
		*calls = append(*calls, securityCall{stdin, args})
		code := 0
		if len(codes) > 0 {
			code, codes = codes[0], codes[1:]
		}
		return out, code, nil
	}
	return calls
}

// fileKey keeps the key in a file under a temp dir, and fails any keychain use.
func fileKey(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "state", "linear-api-key")
	oldUse, oldRun, oldFile := useKeychain, runSecurity, keyFile
	t.Cleanup(func() { useKeychain, runSecurity, keyFile = oldUse, oldRun, oldFile })
	useKeychain = false
	keyFile = func() string { return path }
	runSecurity = func(string, ...string) (string, int, error) {
		t.Error("used the keychain")
		return "", 1, nil
	}
	return path
}

func TestSaveKeyRejectsBadFormat(t *testing.T) {
	for _, keychain := range []bool{true, false} {
		var calls *[]securityCall
		var path string
		if keychain {
			calls = fakeKeychain(t, "")
		} else {
			path = fileKey(t)
		}
		for _, key := range []string{"", "lin_api_", "lin_api_ab cd", "lin_api_ab\nquit", "lin_api_ab-cd", "lin_oauth_abc", "sk-abc", "lin_api_ab\"cd"} {
			err := SaveKey(key)
			if err == nil {
				t.Errorf("SaveKey(%q) = nil, want an error", key)
				continue
			}
			if key != "" && key != "lin_api_" && strings.Contains(err.Error(), key) {
				t.Errorf("the error shows the key: %v", err)
			}
		}
		if keychain && len(*calls) > 0 {
			t.Errorf("ran security %d times for bad keys", len(*calls))
		}
		if !keychain {
			if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
				t.Errorf("a bad key left a file: %v", err)
			}
		}
	}
}

func TestSaveKeyTrimsSpace(t *testing.T) {
	path := fileKey(t)
	if err := SaveKey("  lin_api_Abc123\n"); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(path); string(b) != "lin_api_Abc123\n" {
		t.Errorf("file holds %q", b)
	}
}

func TestFileKeyRoundTripAndMode(t *testing.T) {
	path := fileKey(t)
	if _, err := LoadKey(); !errors.Is(err, ErrNoKey) {
		t.Fatalf("LoadKey before saving: %v, want ErrNoKey", err)
	}
	if err := SaveKey("lin_api_Abc123"); err != nil {
		t.Fatal(err)
	}
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm() != 0o600 {
		t.Errorf("mode = %v, want 0600", fi.Mode().Perm())
	}
	if key, err := LoadKey(); err != nil || key != "lin_api_Abc123" {
		t.Errorf("LoadKey = %q, %v", key, err)
	}

	// Saving again replaces the key and leaves no temp files behind.
	if err := SaveKey("lin_api_Def456"); err != nil {
		t.Fatal(err)
	}
	if key, err := LoadKey(); err != nil || key != "lin_api_Def456" {
		t.Errorf("LoadKey after a second save = %q, %v", key, err)
	}
	entries, err := os.ReadDir(filepath.Dir(path))
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		var names []string
		for _, e := range entries {
			names = append(names, e.Name())
		}
		t.Errorf("state dir holds %v, want the key file alone", names)
	}

	// A key file others can read is refused, without showing the key.
	if err := os.Chmod(path, 0o644); err != nil {
		t.Fatal(err)
	}
	key, err := LoadKey()
	if err == nil || key != "" {
		t.Errorf("LoadKey of a 0644 file = %q, %v; want an error", key, err)
	} else if strings.Contains(err.Error(), "lin_api_Def456") {
		t.Errorf("the error shows the key: %v", err)
	}

	if err := DeleteKey(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("the key file is still there: %v", err)
	}
	if err := DeleteKey(); err != nil {
		t.Errorf("DeleteKey with no key: %v", err)
	}
}

func TestKeychainLoadsTheKey(t *testing.T) {
	calls := fakeKeychain(t, "lin_api_Abc123\n")
	key, err := LoadKey()
	if err != nil || key != "lin_api_Abc123" {
		t.Fatalf("LoadKey = %q, %v", key, err)
	}
	want := []string{"find-generic-password", "-s", "herdr-tasks", "-a", "linear-api-key", "-w"}
	if len(*calls) != 1 || !slices.Equal((*calls)[0].args, want) {
		t.Errorf("ran security with %v, want %v", *calls, want)
	}
}

func TestKeychainNotFoundIsErrNoKey(t *testing.T) {
	fakeKeychain(t, "", 44)
	if _, err := LoadKey(); !errors.Is(err, ErrNoKey) {
		t.Errorf("LoadKey = %v, want ErrNoKey", err)
	}
}

func TestKeychainFailureIsErrKeychain(t *testing.T) {
	fakeKeychain(t, "", 51, 36, 51)
	if _, err := LoadKey(); !errors.Is(err, ErrKeychain) {
		t.Errorf("LoadKey = %v, want ErrKeychain", err)
	}
	if err := SaveKey("lin_api_Abc123"); !errors.Is(err, ErrKeychain) {
		t.Errorf("SaveKey = %v, want ErrKeychain", err)
	}
	if err := DeleteKey(); !errors.Is(err, ErrKeychain) {
		t.Errorf("DeleteKey = %v, want ErrKeychain", err)
	}
}

func TestSaveKeyNeverPutsTheKeyInArgv(t *testing.T) {
	calls := fakeKeychain(t, "")
	if err := SaveKey("lin_api_Abc123"); err != nil {
		t.Fatal(err)
	}
	if len(*calls) != 1 {
		t.Fatalf("ran security %d times, want once", len(*calls))
	}
	c := (*calls)[0]
	if !slices.Equal(c.args, []string{"-i"}) {
		t.Errorf("args = %q, want just -i", c.args)
	}
	for _, a := range c.args {
		if strings.Contains(a, "lin_api_") {
			t.Errorf("the key is in argv: %q", c.args)
		}
	}
	if want := "add-generic-password -U -s herdr-tasks -a linear-api-key -w lin_api_Abc123\n"; c.stdin != want {
		t.Errorf("stdin = %q, want %q", c.stdin, want)
	}
}

func TestKeychainDeleteWithoutAKey(t *testing.T) {
	calls := fakeKeychain(t, "", 44)
	if err := DeleteKey(); err != nil {
		t.Errorf("DeleteKey = %v, want nil when there's no key", err)
	}
	want := []string{"delete-generic-password", "-s", "herdr-tasks", "-a", "linear-api-key"}
	if len(*calls) != 1 || !slices.Equal((*calls)[0].args, want) {
		t.Errorf("ran security with %v, want %v", *calls, want)
	}
}

func TestKeyFileIsInTheStateDir(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("HERDR_PLUGIN_STATE_DIR", dir)
	if got, want := keyFile(), filepath.Join(dir, "linear-api-key"); got != want {
		t.Errorf("keyFile() = %s, want %s", got, want)
	}
}

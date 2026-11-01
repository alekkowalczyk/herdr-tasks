package store

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

type fileDoc struct {
	N int `json:"n"`
}

func TestFileUpdateAndLoad(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "state")
	f := File{Dir: dir, Name: "thing"}
	if f.Path() != filepath.Join(dir, "thing.json") {
		t.Fatalf("path = %s", f.Path())
	}

	got := fileDoc{N: 7}
	if err := f.Load(&got); err != nil || got.N != 7 {
		t.Fatalf("missing file: %+v, %v", got, err)
	}

	var d fileDoc
	if err := f.Update(&d, func() error { d.N = 3; return nil }); err != nil {
		t.Fatal(err)
	}
	fi, err := os.Stat(f.Path())
	if err != nil || fi.Mode().Perm() != 0o600 {
		t.Fatalf("file: %v, %v", fi, err)
	}
	di, _ := os.Stat(dir)
	if di.Mode().Perm() != 0o700 {
		t.Fatalf("dir mode = %v", di.Mode().Perm())
	}
	if _, err := os.Stat(filepath.Join(dir, "thing.lock")); err != nil {
		t.Fatalf("lock file: %v", err)
	}

	var back fileDoc
	if err := f.Load(&back); err != nil || back.N != 3 {
		t.Fatalf("load: %+v, %v", back, err)
	}

	boom := errors.New("boom")
	var d2 fileDoc
	if err := f.Update(&d2, func() error { d2.N = 99; return boom }); !errors.Is(err, boom) {
		t.Fatalf("err = %v", err)
	}
	var after fileDoc
	if err := f.Load(&after); err != nil || after.N != 3 {
		t.Fatalf("after failed update: %+v, %v", after, err)
	}
}

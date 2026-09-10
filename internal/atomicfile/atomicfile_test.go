package atomicfile

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

func TestWriteFileReplacesContentAndPerms(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "state.json")

	if err := WriteFile(path, []byte("one"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := WriteFile(path, []byte("two"), 0o600); err != nil {
		t.Fatal(err)
	}

	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "two" {
		t.Fatalf("content = %q, want %q", got, "two")
	}

	if runtime.GOOS != "windows" {
		info, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		if info.Mode().Perm() != 0o600 {
			t.Fatalf("perm = %o, want 600", info.Mode().Perm())
		}
	}

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		t.Fatalf("expected only the target file, found %d entries", len(entries))
	}
}

func TestWriteFileMissingDir(t *testing.T) {
	if err := WriteFile(filepath.Join(t.TempDir(), "missing", "x"), []byte("x"), 0o600); err == nil {
		t.Fatal("expected error for missing directory")
	}
}

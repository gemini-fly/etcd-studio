package safefile

import (
	"os"
	"path/filepath"
	"testing"
)

func TestOpenFileAllowsOnlyPathsInsideManagedRoot(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	inside := filepath.Join(root, "nested", "history.jsonl")
	file, absolutePath, err := OpenFile(root, inside, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	if absolutePath != inside {
		t.Fatalf("absolute path = %q, want %q", absolutePath, inside)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	outside := filepath.Join(filepath.Dir(root), "outside.jsonl")
	if _, _, err := OpenFile(root, outside, os.O_CREATE|os.O_RDWR, 0o600); err == nil {
		t.Fatal("outside path was accepted")
	}
	if _, err := os.Stat(outside); !os.IsNotExist(err) {
		t.Fatalf("outside path was created: %v", err)
	}
}

func TestReadFileRejectsTraversalAndEscapingSymlink(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	outsideDirectory := t.TempDir()
	outside := filepath.Join(outsideDirectory, "ca.pem")
	if err := os.WriteFile(outside, []byte("secret"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadFile(root, filepath.Join(root, "..", filepath.Base(outsideDirectory), "ca.pem")); err == nil {
		t.Fatal("traversal path was accepted")
	}
	link := filepath.Join(root, "certs")
	if err := os.Symlink(outsideDirectory, link); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadFile(root, filepath.Join(link, "ca.pem")); err == nil {
		t.Fatal("escaping symlink was accepted")
	}
}

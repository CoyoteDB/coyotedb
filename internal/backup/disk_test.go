package backup

import (
	"os"
	"path/filepath"
	"testing"
)

func TestNearestExistingAndSameDisk(t *testing.T) {
	root := t.TempDir()
	missing := filepath.Join(root, "a", "b", "c")
	if got := NearestExisting(missing); got != root {
		t.Fatalf("NearestExisting(%q) = %q, want %q", missing, got, root)
	}
	if err := os.MkdirAll(filepath.Join(root, "x"), 0o700); err != nil {
		t.Fatal(err)
	}
	same, err := SameDisk(missing, filepath.Join(root, "x"))
	if err != nil || !same {
		t.Fatalf("two folders in one temp folder: same=%v err=%v", same, err)
	}
}

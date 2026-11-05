package results

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLoadAllFollowsSymlinks(t *testing.T) {
	src, err := filepath.Abs("../../examples/demo/results/hotspot-r1")
	if err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	if err := os.Symlink(src, filepath.Join(root, "hotspot-r1")); err != nil {
		t.Skip("symlinks unavailable:", err)
	}
	runs, err := LoadAll(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(runs) != 1 {
		t.Fatalf("runs = %d, want the symlinked run", len(runs))
	}
}

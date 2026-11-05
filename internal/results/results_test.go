package results

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLoadAllFollowsSymlinks(t *testing.T) {
	src := filepath.Join(t.TempDir(), "s-r1")
	if err := os.MkdirAll(src, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := WriteJSON(filepath.Join(src, "meta.json"), Meta{RunID: "s-r1", Scenario: "s", Rep: 1}); err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	if err := os.Symlink(src, filepath.Join(root, "s-r1")); err != nil {
		t.Skip("symlinks unavailable:", err)
	}
	runs, err := LoadAll(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(runs) != 1 || runs[0].Meta.RunID != "s-r1" {
		t.Fatalf("runs = %d, want the symlinked run", len(runs))
	}
}

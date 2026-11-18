package adapter

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestListUIScreenshotsSortsByModificationTime(t *testing.T) {
	root := t.TempDir()
	if err := writeTestFile(filepath.Join(root, ".slopguard", "ui.yaml"),
		"base_url: http://127.0.0.1:3000\nscreenshot_dir: artifacts/shots\n"); err != nil {
		t.Fatal(err)
	}
	older := filepath.Join(root, "artifacts", "shots", "older.png")
	newer := filepath.Join(root, "artifacts", "shots", "newer.jpg")
	if err := writeTestFile(older, "old"); err != nil {
		t.Fatal(err)
	}
	if err := writeTestFile(newer, "new"); err != nil {
		t.Fatal(err)
	}
	if err := writeTestFile(filepath.Join(root, "artifacts", "shots", "ignore.txt"), "not an image"); err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	if err := os.Chtimes(older, now.Add(-2*time.Hour), now.Add(-2*time.Hour)); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(newer, now.Add(-time.Hour), now.Add(-time.Hour)); err != nil {
		t.Fatal(err)
	}

	shots, err := ListUIScreenshots(root)
	if err != nil {
		t.Fatalf("ListUIScreenshots error: %v", err)
	}
	if len(shots) != 2 {
		t.Fatalf("screenshots = %+v, want 2", shots)
	}
	if shots[0].Path != "artifacts/shots/newer.jpg" || shots[1].Path != "artifacts/shots/older.png" {
		t.Fatalf("screenshots order = %+v", shots)
	}
	if !shots[0].ModifiedAt.After(shots[1].ModifiedAt) {
		t.Fatalf("modified times = %v", shots)
	}
}

func TestListUIScreenshotsGracefulEmpty(t *testing.T) {
	root := t.TempDir()
	shots, err := ListUIScreenshots(root)
	if err != nil {
		t.Fatalf("missing config error: %v", err)
	}
	if len(shots) != 0 {
		t.Fatalf("missing config screenshots = %+v", shots)
	}

	if err := writeTestFile(filepath.Join(root, ".slopguard", "ui.yaml"),
		"base_url: http://127.0.0.1:3000\nscreenshot_dir: missing-dir\n"); err != nil {
		t.Fatal(err)
	}
	shots, err = ListUIScreenshots(root)
	if err != nil {
		t.Fatalf("missing screenshot dir error: %v", err)
	}
	if len(shots) != 0 {
		t.Fatalf("missing screenshot dir shots = %+v", shots)
	}

	shots, err = ListUIScreenshots(filepath.Join(root, "does-not-exist"))
	if err != nil {
		t.Fatalf("missing project root error: %v", err)
	}
	if len(shots) != 0 {
		t.Fatalf("missing project root shots = %+v", shots)
	}
}

func TestListUIScreenshotsRejectsBrokenConfig(t *testing.T) {
	root := t.TempDir()
	if err := writeTestFile(filepath.Join(root, ".slopguard", "ui.yaml"), "base_url: [\n"); err != nil {
		t.Fatal(err)
	}
	if _, err := ListUIScreenshots(root); err == nil {
		t.Fatal("broken ui.yaml should return an error")
	}
}

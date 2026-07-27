package server

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// TestBackupStats writes a couple of archives under the per-app layout and
// checks the aggregate count and that the newest modtime wins.
func TestBackupStats(t *testing.T) {
	root := t.TempDir()

	// Empty root: no archives.
	if n, latest := backupStats(root); n != 0 || !latest.IsZero() {
		t.Fatalf("empty root: got count=%d latest=%v, want 0/zero", n, latest)
	}

	older := filepath.Join(root, "demo_api", "20240101-000000.tar.gz")
	newer := filepath.Join(root, "demo_web", "20240202-000000.tar.gz")
	for _, p := range []string{older, newer} {
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	oldT := time.Now().Add(-48 * time.Hour)
	newT := time.Now().Add(-1 * time.Hour)
	os.Chtimes(older, oldT, oldT)
	os.Chtimes(newer, newT, newT)

	n, latest := backupStats(root)
	if n != 2 {
		t.Fatalf("count: got %d, want 2", n)
	}
	if latest.Unix() != newT.Unix() {
		t.Fatalf("latest: got %v, want %v (the newer archive)", latest, newT)
	}
}

func TestHumanizeSince(t *testing.T) {
	now := time.Now()
	cases := []struct {
		t    time.Time
		want string
	}{
		{now.Add(-10 * time.Second), "just now"},
		{now.Add(-5 * time.Minute), "5m ago"},
		{now.Add(-3 * time.Hour), "3h ago"},
		{now.Add(-50 * time.Hour), "2d ago"},
	}
	for _, c := range cases {
		if got := humanizeSince(c.t); got != c.want {
			t.Errorf("humanizeSince(%v): got %q, want %q", c.t, got, c.want)
		}
	}
}

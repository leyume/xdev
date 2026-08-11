package apps

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"os"
	"path/filepath"
	"testing"
)

// tgz builds an in-memory .tar.gz from name -> contents.
func tgz(t *testing.T, files map[string]string) *bytes.Reader {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	for name, body := range files {
		if err := tw.WriteHeader(&tar.Header{
			Name: name, Mode: 0o644, Size: int64(len(body)), Typeflag: tar.TypeReg,
		}); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write([]byte(body)); err != nil {
			t.Fatal(err)
		}
	}
	tw.Close()
	gz.Close()
	return bytes.NewReader(buf.Bytes())
}

// TestExtractAppArchiveOverlays covers the import rules: archive files land in
// the app dir and replace same-path files, untouched files survive (overlay,
// not wipe), and a templated container app keeps its xdev-rendered _/compose.yml
// instead of the one in the archive — while host and compose apps, which own
// their _/, take the archive's copy.
func TestExtractAppArchiveOverlays(t *testing.T) {
	for _, tc := range []struct {
		name        string
		all         bool // unpacksAll(app): host and compose apps own their _/
		wantCompose string
	}{
		{"host app takes everything", true, "from-archive"},
		{"compose app restores its own compose", true, "from-archive"},
		{"container app keeps its compose", false, "xdev-rendered"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			os.MkdirAll(filepath.Join(dir, "_"), 0o755)
			write(t, dir, "_/compose.yml", "xdev-rendered")
			write(t, dir, "app/keep.txt", "untouched")
			write(t, dir, "app/index.php", "old")

			archive := tgz(t, map[string]string{
				"_/compose.yml": "from-archive",
				"app/index.php": "new",
			})
			if err := extractAppArchive(archive, dir, tc.all); err != nil {
				t.Fatalf("extract: %v", err)
			}
			if got := read(t, dir, "_/compose.yml"); got != tc.wantCompose {
				t.Errorf("_/compose.yml = %q, want %q", got, tc.wantCompose)
			}
			if got := read(t, dir, "app/index.php"); got != "new" {
				t.Errorf("app/index.php = %q, want the archive's copy", got)
			}
			if got := read(t, dir, "app/keep.txt"); got != "untouched" {
				t.Errorf("file not in the archive was lost: %q", got)
			}
		})
	}
}

// TestExtractAppArchiveRefusesEscape makes sure a hostile archive can't write
// outside the app directory.
func TestExtractAppArchiveRefusesEscape(t *testing.T) {
	dir := t.TempDir()
	archive := tgz(t, map[string]string{"../escaped.txt": "pwned"})
	if err := extractAppArchive(archive, dir, true); err == nil {
		t.Fatal("extract accepted an entry escaping the destination")
	}
	if _, err := os.Stat(filepath.Join(filepath.Dir(dir), "escaped.txt")); err == nil {
		t.Fatal("archive wrote outside the app directory")
	}
}

func write(t *testing.T, dir, rel, body string) {
	t.Helper()
	p := filepath.Join(dir, filepath.FromSlash(rel))
	os.MkdirAll(filepath.Dir(p), 0o755)
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func read(t *testing.T, dir, rel string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(dir, filepath.FromSlash(rel)))
	if err != nil {
		t.Fatalf("read %s: %v", rel, err)
	}
	return string(b)
}

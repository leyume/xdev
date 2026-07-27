package apps

import (
	"archive/zip"
	"bytes"
	"os"
	"path/filepath"
	"testing"
)

// zipWith builds an in-memory zip from name->content entries.
func zipWith(t *testing.T, files map[string]string) *zip.Reader {
	t.Helper()
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	for name, body := range files {
		f, err := zw.Create(name)
		if err != nil {
			t.Fatal(err)
		}
		f.Write([]byte(body))
	}
	zw.Close()
	zr, err := zip.NewReader(bytes.NewReader(buf.Bytes()), int64(buf.Len()))
	if err != nil {
		t.Fatal(err)
	}
	return zr
}

// TestUnzipRejectsEscape ensures a zip entry can't write outside the target dir.
func TestUnzipRejectsEscape(t *testing.T) {
	dir := t.TempDir()
	zr := zipWith(t, map[string]string{"../evil.txt": "x"})
	if err := unzip(zr, dir); err == nil {
		t.Fatal("expected unzip to reject a path-escaping entry")
	}
	if _, err := os.Stat(filepath.Join(filepath.Dir(dir), "evil.txt")); err == nil {
		t.Fatal("path traversal wrote a file outside the destination")
	}
}

// TestUnzipNormal extracts a well-formed plugin zip.
func TestUnzipNormal(t *testing.T) {
	dir := t.TempDir()
	zr := zipWith(t, map[string]string{"akismet/akismet.php": "<?php"})
	if err := unzip(zr, dir); err != nil {
		t.Fatalf("unzip: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "akismet", "akismet.php")); err != nil {
		t.Fatalf("expected extracted file: %v", err)
	}
}

func TestPoolSlug(t *testing.T) {
	cases := map[string]string{
		"akismet.zip":      "akismet",
		"my-plugin_2.zip":  "my-plugin_2",
		"../../etc/passwd": "etcpasswd",
		"a b/c":            "abc",
	}
	for in, want := range cases {
		if got := poolSlug(in); got != want {
			t.Errorf("poolSlug(%q) = %q, want %q", in, got, want)
		}
	}
}

// TestWPPoolAddLinks drives the pool end-to-end (no engine): uploading a plugin
// extracts it once and symlinks it into an existing shared site; removing it
// deletes the copy and the link.
func TestWPPoolAddLinks(t *testing.T) {
	wp := t.TempDir()
	svc := &Service{wpDir: wp}
	// An existing shared site.
	siteContent := filepath.Join(wp, "sites", "demo_blog", "wp-content", "plugins")
	if err := os.MkdirAll(siteContent, 0o777); err != nil {
		t.Fatal(err)
	}

	zr := zipWith(t, map[string]string{"akismet/akismet.php": "<?php"})
	if err := svc.WPPoolAdd("plugins", "akismet.zip", zr); err != nil {
		t.Fatalf("WPPoolAdd: %v", err)
	}
	if items := svc.WPPoolList("plugins"); len(items) != 1 || items[0].Name != "akismet" {
		t.Fatalf("WPPoolList = %+v, want [akismet]", items)
	}
	link := filepath.Join(siteContent, "akismet")
	if !isSymlink(link) {
		t.Fatal("expected akismet symlinked into the shared site")
	}
	if _, err := os.Stat(filepath.Join(link, "akismet.php")); err != nil {
		t.Fatalf("symlink does not resolve to the plugin: %v", err)
	}

	if err := svc.WPPoolRemove("plugins", "akismet"); err != nil {
		t.Fatalf("WPPoolRemove: %v", err)
	}
	if isSymlink(link) {
		t.Fatal("expected the site symlink removed")
	}
	if len(svc.WPPoolList("plugins")) != 0 {
		t.Fatal("expected the pool item removed")
	}
}

func TestValidIdentAndEscape(t *testing.T) {
	for _, ok := range []string{"demo_api", "wp_site_1", "Users"} {
		if !validIdent(ok) {
			t.Errorf("validIdent(%q) = false, want true", ok)
		}
	}
	for _, bad := range []string{"", "a b", "drop;", "back`tick", "quote'", "toolong" + string(make([]byte, 70))} {
		if validIdent(bad) {
			t.Errorf("validIdent(%q) = true, want false", bad)
		}
	}
	if got := sqlStr(`a'b\c`); got != `a\'b\\c` {
		t.Errorf("sqlStr escape = %q", got)
	}
}

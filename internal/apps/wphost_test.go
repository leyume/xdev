package apps

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestWPConfigPHP checks the generated wp-config: shared-db creds injected, the
// derived table prefix, all eight salts present — and fresh per render.
func TestWPConfigPHP(t *testing.T) {
	saltKeys := []string{
		"AUTH_KEY", "SECURE_AUTH_KEY", "LOGGED_IN_KEY", "NONCE_KEY",
		"AUTH_SALT", "SECURE_AUTH_SALT", "LOGGED_IN_SALT", "NONCE_SALT",
	}
	cfg := wpConfigPHP("demo_blog", "demo_blog", "p4ss", "wp_blog_")
	for _, want := range append([]string{
		"'DB_NAME', 'demo_blog'",
		"'DB_USER', 'demo_blog'",
		"'DB_PASSWORD', 'p4ss'",
		"'DB_HOST', 'xdev-db'", // always the shared server
		"$table_prefix = 'wp_blog_';",
		"require_once ABSPATH . 'wp-settings.php';",
	}, saltKeys...) {
		if !strings.Contains(cfg, want) {
			t.Errorf("wp-config missing %q\n%s", want, cfg)
		}
	}

	// Salts must differ between two renders (crypto/rand, not a fixed template).
	cfg2 := wpConfigPHP("demo_blog", "demo_blog", "p4ss", "wp_blog_")
	saltLine := func(s, key string) string {
		for _, line := range strings.Split(s, "\n") {
			if strings.Contains(line, "'"+key+"'") {
				return line
			}
		}
		return ""
	}
	for _, k := range saltKeys {
		a, b := saltLine(cfg, k), saltLine(cfg2, k)
		if a == "" || a == b {
			t.Errorf("salt %s not unique per render: %q vs %q", k, a, b)
		}
	}
}

// TestWriteWPSite lays a site out against a fake core and checks the sharing
// mechanism: core entries symlinked in, wp-config.php and wp-content/ real and
// per-site, and the core's wp-config-sample never leaking in. Also verifies
// the symlinks actually resolve (what both Caddy and PHP-FPM rely on).
func TestWriteWPSite(t *testing.T) {
	core := t.TempDir()
	for p, data := range map[string]string{
		"index.php":                     "<?php // core index",
		"wp-settings.php":               "<?php // core settings",
		"wp-config-sample.php":          "<?php // sample — must not be linked",
		"wp-admin/index.php":            "<?php // admin",
		"wp-content/themes/t/style.css": "/* theme */",
	} {
		full := filepath.Join(core, p)
		os.MkdirAll(filepath.Dir(full), 0o755)
		if err := os.WriteFile(full, []byte(data), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	site := filepath.Join(t.TempDir(), "demo_blog")
	if err := writeWPSite(site, core, "THE-CONFIG"); err != nil {
		t.Fatalf("writeWPSite: %v", err)
	}

	// Core files arrive as symlinks that resolve to the core content.
	for _, p := range []string{"index.php", "wp-settings.php", "wp-admin"} {
		fi, err := os.Lstat(filepath.Join(site, p))
		if err != nil || fi.Mode()&os.ModeSymlink == 0 {
			t.Errorf("%s should be a symlink into core (err=%v)", p, err)
		}
	}
	if b, err := os.ReadFile(filepath.Join(site, "wp-admin", "index.php")); err != nil || string(b) != "<?php // admin" {
		t.Errorf("wp-admin symlink does not resolve: %v %q", err, b)
	}

	// wp-config.php and wp-content are real, per-site copies.
	if b, _ := os.ReadFile(filepath.Join(site, "wp-config.php")); string(b) != "THE-CONFIG" {
		t.Errorf("wp-config.php = %q, want the generated config", b)
	}
	if fi, err := os.Lstat(filepath.Join(site, "wp-content")); err != nil || !fi.IsDir() {
		t.Errorf("wp-content must be a real directory, got %v (err=%v)", fi, err)
	}
	if _, err := os.Stat(filepath.Join(site, "wp-content", "themes", "t", "style.css")); err != nil {
		t.Errorf("default theme not copied into site wp-content: %v", err)
	}
	if _, err := os.Lstat(filepath.Join(site, "wp-config-sample.php")); err == nil {
		t.Error("core wp-config-sample.php must not be linked into the site")
	}

	// Idempotent: a retried create refreshes links without erroring.
	if err := writeWPSite(site, core, "THE-CONFIG"); err != nil {
		t.Fatalf("writeWPSite retry: %v", err)
	}
}

// TestUntarGzRejectsEscape checks the core-tarball extractor refuses entries
// that would write outside the destination.
func TestUntarGzRejectsEscape(t *testing.T) {
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	tw.WriteHeader(&tar.Header{Name: "../evil.php", Typeflag: tar.TypeReg, Size: 4, Mode: 0o644})
	tw.Write([]byte("evil"))
	tw.Close()
	gz.Close()

	dest := t.TempDir()
	if err := untarGz(&buf, dest); err == nil {
		t.Fatal("untarGz should reject a path-traversal entry")
	}
	if _, err := os.Stat(filepath.Join(filepath.Dir(dest), "evil.php")); err == nil {
		t.Fatal("traversal entry was written outside the destination")
	}
}

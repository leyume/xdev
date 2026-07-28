// Shared WordPress host (PLANx §C1): one xdev-managed `xdev-wp` PHP-FPM
// container serves every shared-mode WordPress site, instead of an Apache+PHP
// container per site. Each site is just a docroot under data/wp/sites/ holding
// its own wp-config.php and wp-content/, with the rest of a single xdev-owned
// WP core (data/wp/core/, downloaded from wordpress.org on first use)
// symlinked in — the classic shared-hosting layout. xdev's Caddy serves the
// files directly and hands *.php to the published fpm port; data/wp is
// bind-mounted into the container at the same absolute path as on the host so
// SCRIPT_FILENAME (and the core symlinks) resolve on both sides. Databases
// live on the shared xdev-db server (shareddb.go).
//
// ponytail: one PHP pool is a noisy neighbor across every shared WP site —
// move to per-site FPM pools if it ever matters. Core updates are an explicit
// xdev action (C3), never WP self-update: the core dir is xdev-owned and
// sites can't write it.
package apps

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"fmt"
	"io"
	"io/fs"
	"net"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"xdev/internal/runtime"
	"xdev/internal/store"
)

const (
	wpHostContainer = "xdev-wp"
	// The official WordPress fpm variant rather than bare php:8.3-fpm: the same
	// php-fpm 8.3 on :9000, but with the PHP extensions WordPress requires
	// (mysqli, gd, opcache, …) already compiled in — bare php:8.3-fpm cannot
	// talk to MariaDB at all.
	wpHostImage = "docker.io/library/wordpress:php8.3-fpm"
	wpCoreURL   = "https://wordpress.org/latest.tar.gz"
)

// wpSiteDir returns a shared site's docroot: data/wp/sites/<project>_<app>
// (project-prefixed because app slugs are only unique per project).
func (s *Service) wpSiteDir(projectSlug, appSlug string) string {
	return filepath.Join(s.wpDir, "sites", projectSlug+"_"+appSlug)
}

// layoutWPShared provisions everything a shared-host WordPress site needs:
// the WP core (downloaded on first use), the wp-host container, a database on
// the shared server, and the site docroot. Ordered so a failure leaves no
// half-created app — the docroot comes last and is removed (with its database)
// if it can't be completed.
func (s *Service) layoutWPShared(app *store.App, proj store.Project) error {
	ctx, cancel := context.WithTimeout(context.Background(), composeTimeout)
	defer cancel()
	engine := runtime.Engine(app.Runtime)

	if err := s.ensureWPCore(ctx); err != nil {
		return err
	}
	if _, err := s.ensureWPHost(ctx, engine); err != nil {
		return err
	}
	dbName := sharedDBName(proj.Slug, app.Slug)
	dbPass, err := s.provisionSharedDB(ctx, engine, dbName)
	if err != nil {
		return err
	}
	prefix := "wp_" + strings.ReplaceAll(app.Slug, "-", "_") + "_"
	siteDir := s.wpSiteDir(proj.Slug, app.Slug)
	if err := writeWPSite(siteDir, filepath.Join(s.wpDir, "core"),
		wpConfigPHP(dbName, dbName, dbPass, prefix)); err != nil {
		os.RemoveAll(siteDir)
		s.dropSharedDB(ctx, engine, dbName)
		return err
	}
	// Share the central plugin/theme pools into the new site.
	s.linkPoolsInto(filepath.Join(siteDir, "wp-content"))
	app.DBMode = store.DBShared
	app.WPMode = store.WPShared
	return nil
}

// ensureWPCore downloads and unpacks the WordPress core into data/wp/core on
// first use. Extraction happens in a temp dir on the same filesystem and is
// moved into place with a single rename, so a failed or interrupted download
// never leaves a usable-looking half core behind.
func (s *Service) ensureWPCore(ctx context.Context) error {
	core := filepath.Join(s.wpDir, "core")
	if _, err := os.Stat(filepath.Join(core, "wp-settings.php")); err == nil {
		return nil
	}
	if err := os.MkdirAll(s.wpDir, 0o755); err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, wpCoreURL, nil)
	if err != nil {
		return err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return fmt.Errorf("download WordPress core: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("download WordPress core: %s returned %s", wpCoreURL, resp.Status)
	}
	tmp, err := os.MkdirTemp(s.wpDir, ".core-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(tmp)
	if err := untarGz(resp.Body, tmp); err != nil {
		return fmt.Errorf("extract WordPress core: %w", err)
	}
	src := filepath.Join(tmp, "wordpress") // the tarball's single top-level dir
	if _, err := os.Stat(filepath.Join(src, "wp-settings.php")); err != nil {
		return fmt.Errorf("unexpected WordPress tarball layout: %w", err)
	}
	os.RemoveAll(core) // a leftover partial core (crashed earlier attempt) blocks the rename
	return os.Rename(src, core)
}

// untarGz extracts a .tar.gz stream into dest, refusing entries that would
// escape it. Only files and directories are written — the WP tarball carries
// nothing else.
// hasMount reports whether inspect output (one "source:destination" per line)
// contains a bind of dir at the identical path inside the container. Matching
// whole lines, not a substring: "/data/wp:/data/wp" is a substring of a mount
// line for "/other/data/wp:/data/wp", which binds a *different* source at the
// right destination — exactly the mismatch this check exists to catch.
func hasMount(inspect, dir string) bool {
	want := dir + ":" + dir
	for _, line := range strings.Split(inspect, "\n") {
		if strings.TrimSpace(line) == want {
			return true
		}
	}
	return false
}

func untarGz(r io.Reader, dest string) error {
	return untarGzFilter(r, dest, nil)
}

// untarGzFilter is untarGz with an optional per-entry gate: keep reports whether
// an entry (its slash-separated path inside the archive) should be written.
func untarGzFilter(r io.Reader, dest string, keep func(name string) bool) error {
	gz, err := gzip.NewReader(r)
	if err != nil {
		return err
	}
	defer gz.Close()
	tr := tar.NewReader(gz)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}
		name := filepath.FromSlash(hdr.Name)
		if !filepath.IsLocal(name) {
			return fmt.Errorf("tar entry escapes destination: %q", hdr.Name)
		}
		if keep != nil && !keep(path.Clean(hdr.Name)) {
			continue
		}
		dst := filepath.Join(dest, name)
		switch hdr.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(dst, 0o755); err != nil {
				return err
			}
		case tar.TypeReg:
			if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
				return err
			}
			f, err := os.OpenFile(dst, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, fs.FileMode(hdr.Mode)&0o777)
			if err != nil {
				return err
			}
			_, err = io.Copy(f, tr)
			f.Close()
			if err != nil {
				return err
			}
		}
	}
}

// ensureWPHost lazily brings up the shared PHP-FPM container: allocates and
// persists its fpm host port (and the wp dir, for route building) on first
// use, creates the xdev_shared network (it must reach xdev-db), starts (or
// creates) the xdev-wp container, and waits until the fpm port accepts
// connections. Idempotent. Returns the fpm host port.
func (s *Service) ensureWPHost(ctx context.Context, engine runtime.Engine) (int, error) {
	portStr, err := s.store.GetSetting(store.WPHostFPMPortKey)
	if err != nil {
		return 0, err
	}
	port, _ := strconv.Atoi(portStr)
	if port == 0 {
		if port, err = s.allocPort(); err != nil {
			return 0, err
		}
		if err := s.store.SetSetting(store.WPHostFPMPortKey, strconv.Itoa(port)); err != nil {
			return 0, err
		}
	}
	if err := s.store.SetSetting(store.WPHostDirKey, s.wpDir); err != nil {
		return 0, err
	}
	if err := runtime.NetworkCreate(ctx, engine, sharedDBNetwork); err != nil {
		return 0, err
	}
	// Reuse the container only if it bind-mounts the CURRENT wp dir at the same
	// path. One created under a different data dir (e.g. an earlier run) would
	// mount the wrong tree, so php-fpm couldn't see this data dir's sites — drop
	// it and recreate. Missing -> run (first use pulls the image); stopped -> start.
	running, inspectErr := runtime.Exec(ctx, engine, "container", "inspect",
		"--format", "{{.State.Running}}", wpHostContainer)
	needRun := inspectErr != nil
	if inspectErr == nil {
		mounts, _ := runtime.Exec(ctx, engine, "container", "inspect",
			"--format", "{{range .Mounts}}{{.Source}}:{{.Destination}}\n{{end}}", wpHostContainer)
		if !hasMount(mounts, s.wpDir) {
			if _, rmErr := runtime.Exec(ctx, engine, "rm", "-f", wpHostContainer); rmErr != nil {
				return 0, fmt.Errorf("wp-host mounts the wrong dir (need %s) and could not be recreated: %w", s.wpDir, rmErr)
			}
			needRun = true
		}
	}
	switch {
	case needRun:
		if _, err := runtime.Exec(ctx, engine, "run", "-d",
			"--name", wpHostContainer, "--restart", "unless-stopped",
			"--network", sharedDBNetwork,
			"-p", fmt.Sprintf("127.0.0.1:%d:9000", port),
			"-v", s.wpDir+":"+s.wpDir, // same absolute path inside and out
			wpHostImage); err != nil {
			return 0, err
		}
	case strings.TrimSpace(running) != "true":
		if _, err := runtime.Exec(ctx, engine, "start", wpHostContainer); err != nil {
			return 0, err
		}
	}
	// Wait for the published fpm port to accept connections — that's exactly
	// what Caddy will dial.
	deadline := time.Now().Add(60 * time.Second)
	for {
		conn, err := net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", port), 2*time.Second)
		if err == nil {
			conn.Close()
			return port, nil
		}
		if time.Now().After(deadline) || ctx.Err() != nil {
			return 0, fmt.Errorf("wp-host (%s) fpm port %d did not become ready: %w", wpHostContainer, port, err)
		}
		time.Sleep(time.Second)
	}
}

// writeWPSite lays out one site's docroot against the shared core: every
// top-level core entry is symlinked in (wp-admin/, wp-includes/, *.php) except
// wp-config*, while wp-config.php and wp-content/ stay real — per-site config
// and content over a read-only shared core. Idempotent, so a retried create
// just refreshes the links.
func writeWPSite(siteDir, coreDir, wpConfig string) error {
	if err := os.MkdirAll(siteDir, 0o755); err != nil {
		return err
	}
	entries, err := os.ReadDir(coreDir)
	if err != nil {
		return err
	}
	for _, e := range entries {
		name := e.Name()
		if name == "wp-content" || strings.HasPrefix(name, "wp-config") {
			continue
		}
		dst := filepath.Join(siteDir, name)
		os.Remove(dst) // refresh a stale link on retry
		if err := os.Symlink(filepath.Join(coreDir, name), dst); err != nil {
			return err
		}
	}
	// wp-content starts as a real copy of the core defaults (themes etc.) so a
	// fresh site works immediately; C2 replaces the per-site copies of plugins/
	// themes with symlinks into shared pools.
	content := filepath.Join(siteDir, "wp-content")
	if _, err := os.Stat(content); os.IsNotExist(err) {
		if err := os.CopyFS(content, os.DirFS(filepath.Join(coreDir, "wp-content"))); err != nil {
			return err
		}
	}
	// ponytail: wp-content is opened world-writable so the container's www-data
	// (a different uid than the host owner) can write uploads and install
	// plugins. Per-site FPM pools running as the file owner is the upgrade path.
	filepath.WalkDir(content, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return os.Chmod(p, 0o777)
		}
		return os.Chmod(p, 0o666)
	})
	// Drop in the shared pool-guard mu-plugin (data/wp is coreDir's parent).
	linkGuardInto(filepath.Dir(coreDir), content)
	if err := os.WriteFile(filepath.Join(siteDir, "wp-config.php"), []byte(wpConfig), 0o644); err != nil {
		return err
	}
	// Pin ABSPATH/wp-content to this site. The docroot is symlinks into the
	// shared core, so PHP resolves __DIR__ (and thus wp-load's ABSPATH) to the
	// core — which would make every site read core/wp-config.php and
	// core/wp-content, bypassing this site's own config, uploads, and the shared
	// plugin/theme pools. A PHP-FPM auto_prepend_file runs before the symlinked
	// wp-load and fixes ABSPATH first. .xdev-bootstrap.php is a real file here,
	// so its __DIR__ is the site (not the core).
	boot := "<?php\n" +
		"if (!defined('ABSPATH')) define('ABSPATH', __DIR__ . '/');\n" +
		"if (!defined('WP_CONTENT_DIR')) define('WP_CONTENT_DIR', __DIR__ . '/wp-content');\n"
	if err := os.WriteFile(filepath.Join(siteDir, ".xdev-bootstrap.php"), []byte(boot), 0o644); err != nil {
		return err
	}
	userINI := fmt.Sprintf("auto_prepend_file=%q\n", filepath.Join(siteDir, ".xdev-bootstrap.php"))
	return os.WriteFile(filepath.Join(siteDir, ".user.ini"), []byte(userINI), 0o644)
}

// wpConfigPHP generates a site's wp-config.php: shared-db credentials, a
// unique table prefix, and fresh crypto/rand salts. Injected values are
// xdev-generated (hex passwords, slug-derived names), so no PHP escaping is
// needed.
func wpConfigPHP(dbName, dbUser, dbPass, tablePrefix string) string {
	var b strings.Builder
	b.WriteString("<?php\n// Generated by xdev (shared WordPress host). DB settings and salts are managed by xdev.\n")
	for _, k := range [][2]string{
		{"DB_NAME", dbName}, {"DB_USER", dbUser}, {"DB_PASSWORD", dbPass},
		{"DB_HOST", sharedDBContainer}, {"DB_CHARSET", "utf8mb4"}, {"DB_COLLATE", ""},
	} {
		fmt.Fprintf(&b, "define( '%s', '%s' );\n", k[0], k[1])
	}
	for _, k := range []string{
		"AUTH_KEY", "SECURE_AUTH_KEY", "LOGGED_IN_KEY", "NONCE_KEY",
		"AUTH_SALT", "SECURE_AUTH_SALT", "LOGGED_IN_SALT", "NONCE_SALT",
	} {
		fmt.Fprintf(&b, "define( '%s', '%s' );\n", k, randHex(32))
	}
	fmt.Fprintf(&b, "$table_prefix = '%s';\n", tablePrefix)
	b.WriteString("define( 'WP_DEBUG', false );\n")
	// Write plugins/themes/uploads directly — there is no FTP on this host.
	b.WriteString("define( 'FS_METHOD', 'direct' );\n")
	// Core updates are an xdev action on the shared core, never per-site.
	b.WriteString("define( 'AUTOMATIC_UPDATER_DISABLED', true );\n")
	b.WriteString("if ( ! defined( 'ABSPATH' ) ) {\n\tdefine( 'ABSPATH', __DIR__ . '/' );\n}\nrequire_once ABSPATH . 'wp-settings.php';\n")
	return b.String()
}

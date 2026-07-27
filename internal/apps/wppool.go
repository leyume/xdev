// Central WordPress plugin/theme pools (PLANx §C2): a plugin or theme uploaded
// once here is symlinked into every shared-host WP site's wp-content, so all
// sites share one copy alongside their own site-local installs. Removing a
// pool item unlinks it from every site. Activation stays per-site (WordPress
// handles that) — this only shares the files.
//
// ponytail: a pool update is global (one copy, all sites) — no per-site version
// pinning. A site that needs its own version just installs it site-locally,
// which shadows the pooled symlink (see linkPoolItem).
package apps

import (
	"archive/zip"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

var poolKinds = map[string]bool{"plugins": true, "themes": true}

// muGuardFilename is the shared must-use plugin symlinked into every site. It
// stops a site from updating or deleting a pooled (symlinked) plugin/theme in
// wp-admin — an update would rewrite the one shared copy for all sites, and a
// delete under FS_METHOD=direct would recurse through the symlink and wipe the
// pooled files for everyone. Background auto-updates are already off via
// AUTOMATIC_UPDATER_DISABLED in wp-config; this covers the manual click.
const muGuardFilename = "xdev-pool-guard.php"

const poolGuardPHP = `<?php
/**
 * Plugin Name: xdev shared-pool guard
 * Description: Blocks updating/deleting pooled (symlinked) plugins & themes so one
 *   site can't change or remove them for every site. Site-local installs are free.
 */
if (!defined('ABSPATH')) { exit; }
function xdev_is_pooled($dir) { return is_link(rtrim($dir, '/')); }
// Hide update offers for pooled items (a click would rewrite the shared copy).
add_filter('site_transient_update_plugins', function ($t) {
	if (empty($t->response)) { return $t; }
	foreach (array_keys($t->response) as $file) {
		if (xdev_is_pooled(WP_PLUGIN_DIR . '/' . strtok($file, '/'))) { unset($t->response[$file]); }
	}
	return $t;
});
add_filter('site_transient_update_themes', function ($t) {
	if (empty($t->response)) { return $t; }
	foreach (array_keys($t->response) as $slug) {
		if (xdev_is_pooled(get_theme_root($slug) . '/' . $slug)) { unset($t->response[$slug]); }
	}
	return $t;
});
// Drop the Delete action for pooled plugins (delete would wipe the shared copy).
add_filter('plugin_action_links', function ($actions, $file) {
	if (xdev_is_pooled(WP_PLUGIN_DIR . '/' . strtok($file, '/'))) { unset($actions['delete']); }
	return $actions;
}, 10, 2);
`

// ensureSharedMU writes the shared pool-guard mu-plugin into data/wp/mu and
// returns its path. Idempotent; the file is refreshed on every call.
func ensureSharedMU(wpDir string) (string, error) {
	dir := filepath.Join(wpDir, "mu")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	p := filepath.Join(dir, muGuardFilename)
	return p, os.WriteFile(p, []byte(poolGuardPHP), 0o644)
}

// linkGuardInto symlinks the shared pool-guard into one site's mu-plugins dir so
// WordPress auto-loads it. Best-effort: the guard is a safety net, not a
// prerequisite for the site to run.
func linkGuardInto(wpDir, siteContent string) {
	guard, err := ensureSharedMU(wpDir)
	if err != nil {
		return
	}
	muDir := filepath.Join(siteContent, "mu-plugins")
	if err := os.MkdirAll(muDir, 0o777); err != nil {
		return
	}
	link := filepath.Join(muDir, muGuardFilename)
	if isSymlink(link) {
		os.Remove(link)
	}
	os.Symlink(guard, link)
}

// wpPoolDir is data/wp/pool/<kind>.
func (s *Service) wpPoolDir(kind string) string {
	return filepath.Join(s.wpDir, "pool", kind)
}

// WPPoolItem is one shared plugin or theme.
type WPPoolItem struct {
	Name string
	Kind string // plugins | themes
}

// WPPoolList returns the shared items of a kind, sorted by name.
func (s *Service) WPPoolList(kind string) []WPPoolItem {
	if !poolKinds[kind] {
		return nil
	}
	entries, err := os.ReadDir(s.wpPoolDir(kind))
	if err != nil {
		return nil
	}
	var items []WPPoolItem
	for _, e := range entries {
		if e.IsDir() && !strings.HasPrefix(e.Name(), ".") {
			items = append(items, WPPoolItem{Name: e.Name(), Kind: kind})
		}
	}
	sort.Slice(items, func(i, j int) bool { return items[i].Name < items[j].Name })
	return items
}

// WPPoolAdd extracts an uploaded plugin/theme zip into the pool and links it
// into every shared site. fallbackName (the upload's filename) is used only
// when the zip has no single top-level directory of its own to name the item.
func (s *Service) WPPoolAdd(kind, fallbackName string, zr *zip.Reader) error {
	if !poolKinds[kind] {
		return fmt.Errorf("unknown kind %q", kind)
	}
	if err := os.MkdirAll(s.wpDir, 0o755); err != nil {
		return err
	}
	tmp, err := os.MkdirTemp(s.wpDir, ".pool-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(tmp)
	if err := unzip(zr, tmp); err != nil {
		return err
	}
	// A well-formed plugin/theme zip has one top-level dir (its slug); use that
	// as the item. Otherwise wrap the extracted files under the fallback name.
	name, src := fallbackName, tmp
	if entries, _ := os.ReadDir(tmp); len(entries) == 1 && entries[0].IsDir() {
		name = entries[0].Name()
		src = filepath.Join(tmp, name)
	}
	name = poolSlug(name)
	if name == "" {
		return fmt.Errorf("could not determine a name for the upload")
	}
	dst := filepath.Join(s.wpPoolDir(kind), name)
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return err
	}
	os.RemoveAll(dst) // overwrite = update to the newly uploaded version
	if err := os.Rename(src, dst); err != nil {
		if err := os.CopyFS(dst, os.DirFS(src)); err != nil { // cross-device fallback
			return err
		}
	}
	s.linkPoolItemEverywhere(kind, name)
	return nil
}

// WPPoolRemove deletes a shared item and unlinks it from every site.
func (s *Service) WPPoolRemove(kind, name string) error {
	if !poolKinds[kind] || name == "" || poolSlug(name) != name {
		return fmt.Errorf("invalid item")
	}
	s.unlinkPoolItemEverywhere(kind, name)
	return os.RemoveAll(filepath.Join(s.wpPoolDir(kind), name))
}

// linkPoolsInto symlinks every current pool item into one site's wp-content,
// so a freshly-created shared site starts with the shared plugins/themes.
func (s *Service) linkPoolsInto(siteContent string) {
	for kind := range poolKinds {
		for _, it := range s.WPPoolList(kind) {
			linkPoolItem(s.wpPoolDir(kind), it.Name, filepath.Join(siteContent, kind))
		}
	}
}

func (s *Service) linkPoolItemEverywhere(kind, name string) {
	for _, content := range s.sharedSiteContents() {
		linkPoolItem(s.wpPoolDir(kind), name, filepath.Join(content, kind))
	}
}

func (s *Service) unlinkPoolItemEverywhere(kind, name string) {
	for _, content := range s.sharedSiteContents() {
		if link := filepath.Join(content, kind, name); isSymlink(link) {
			os.Remove(link)
		}
	}
}

// sharedSiteContents lists every shared site's wp-content dir.
func (s *Service) sharedSiteContents() []string {
	entries, err := os.ReadDir(filepath.Join(s.wpDir, "sites"))
	if err != nil {
		return nil
	}
	var out []string
	for _, e := range entries {
		if e.IsDir() {
			out = append(out, filepath.Join(s.wpDir, "sites", e.Name(), "wp-content"))
		}
	}
	return out
}

// linkPoolItem creates dir/<name> -> poolDir/<name> (absolute, so it resolves
// identically inside the bind-mounted container), refreshing a stale symlink
// but never clobbering a real site-local install of the same name.
func linkPoolItem(poolDir, name, dir string) {
	if err := os.MkdirAll(dir, 0o777); err != nil {
		return
	}
	link := filepath.Join(dir, name)
	if info, err := os.Lstat(link); err == nil {
		if info.Mode()&os.ModeSymlink == 0 {
			return // a real site-local plugin/theme with this name wins
		}
		os.Remove(link)
	}
	os.Symlink(filepath.Join(poolDir, name), link)
}

func isSymlink(p string) bool {
	info, err := os.Lstat(p)
	return err == nil && info.Mode()&os.ModeSymlink != 0
}

// poolSlug reduces a name to a safe folder slug (drops a .zip suffix).
func poolSlug(s string) string {
	s = strings.TrimSuffix(s, ".zip")
	var b strings.Builder
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_':
			b.WriteRune(r)
		}
	}
	return b.String()
}

// unzip extracts a zip into dest, refusing entries that escape it.
func unzip(zr *zip.Reader, dest string) error {
	for _, f := range zr.File {
		name := filepath.FromSlash(f.Name)
		if !filepath.IsLocal(name) {
			return fmt.Errorf("zip entry escapes destination: %q", f.Name)
		}
		path := filepath.Join(dest, name)
		if f.FileInfo().IsDir() {
			if err := os.MkdirAll(path, 0o777); err != nil {
				return err
			}
			continue
		}
		if err := os.MkdirAll(filepath.Dir(path), 0o777); err != nil {
			return err
		}
		rc, err := f.Open()
		if err != nil {
			return err
		}
		out, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o666)
		if err != nil {
			rc.Close()
			return err
		}
		_, err = io.Copy(out, rc)
		out.Close()
		rc.Close()
		if err != nil {
			return err
		}
	}
	return nil
}

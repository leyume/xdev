package store

import (
	"fmt"
	"path/filepath"
	"strconv"
)

// Settings keys for the shared WordPress host (PLANx §C1), written by the apps
// service when the first shared site is created and read back here to build
// each site's route.
const (
	WPHostDirKey     = "wp_host_dir"      // absolute path of data/wp on the host
	WPHostFPMPortKey = "wp_host_fpm_port" // host port publishing xdev-wp's fpm :9000
)

// RouteInfo is a hostname paired with the upstream its app is served from — the
// pair the reverse proxy needs to route traffic. Exactly one of Port (a host
// port to reverse-proxy to), Root (a directory for Caddy to file-server), or
// Upstream (a proxy app's remote URL) is set. Shared-WP sites set Root plus
// FCGIPort (*.php handed to the wp-host fpm pool).
//
// One per *hostname*, not per app: an app given several domains (test.com,
// www.test.com, test.com.ng) produces one of these each, identical but for
// Host, so each becomes its own route and its own certificate.
type RouteInfo struct {
	Host     string
	Port     int
	Root     string // filesystem dir to serve directly (serve-mode static apps, shared-WP sites)
	FCGIPort int    // fastcgi host port for *.php under Root (shared-WP sites)
	Upstream string // remote URL to forward to (proxy apps)
	Local    bool   // local (.test, internal CA) vs public (ACME)
}

// AppServiceDomain returns the hostname of an app's secondary-service route
// (a domain row with its own port, e.g. Adminer), or "" if it has none.
func (s *Store) AppServiceDomain(appID int64) string {
	var host string
	s.db.QueryRow(
		`SELECT hostname FROM domains WHERE app_id = ? AND port > 0 ORDER BY id LIMIT 1`,
		appID).Scan(&host)
	return host
}

// ServiceDomain is a hostname routed straight to one of an app's published
// ports, rather than to the app's own upstream (compose apps' extra ports,
// Adminer, the mail admin console).
type ServiceDomain struct {
	Host string
	Port int
}

// AppServiceDomains returns every secondary-service route an app has, in
// creation order — a compose app can have one per published port.
func (s *Store) AppServiceDomains(appID int64) ([]ServiceDomain, error) {
	rows, err := s.db.Query(
		`SELECT hostname, port FROM domains WHERE app_id = ? AND port > 0 ORDER BY id`, appID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []ServiceDomain
	for rows.Next() {
		var d ServiceDomain
		if err := rows.Scan(&d.Host, &d.Port); err != nil {
			return nil, err
		}
		out = append(out, d)
	}
	return out, rows.Err()
}

// DomainOwner returns the app id that owns a hostname, or 0 if it's free.
func (s *Store) DomainOwner(hostname string) int64 {
	var id int64
	s.db.QueryRow(`SELECT app_id FROM domains WHERE hostname = ?`, hostname).Scan(&id)
	return id
}

// SetAppDomain updates the domain stored on the app row itself.
func (s *Store) SetAppDomain(appID int64, hostname string) error {
	_, err := s.db.Exec(
		`UPDATE apps SET subdomain = ?, updated_at = datetime('now') WHERE id = ?`, hostname, appID)
	return err
}

// AppHostnames returns every hostname served from the app's own upstream — the
// app's domain and any additional ones given alongside it — in the order they
// were entered. Service domains (port > 0) are excluded: those route to a
// specific published port, not to the app itself.
//
// Insertion order is the entry order because ReplaceAppDomains rewrites the
// whole set in one pass, so ids run in the order the form listed them.
func (s *Store) AppHostnames(appID int64) ([]string, error) {
	rows, err := s.db.Query(
		`SELECT hostname FROM domains WHERE app_id = ? AND port = 0 ORDER BY id`, appID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var h string
		if err := rows.Scan(&h); err != nil {
			return nil, err
		}
		out = append(out, h)
	}
	return out, rows.Err()
}

// ReplaceAppDomains rewrites every hostname an app answers on in one
// transaction: the app's own hostnames (port 0) in the order given, then the
// service domains (port > 0) — for a compose app that second order is the
// ${PORT_n} slot numbering, which AppServiceDomains reads back by id.
//
// Both kinds go in one statement pair on purpose. Editing an app can swap a
// hostname from one role to the other (a stack's second domain becoming its
// first), and doing that as two separate replaces would trip the unique
// hostname index halfway through. Replacing rather than merging is also what
// makes a removed hostname stop routing.
func (s *Store) ReplaceAppDomains(appID int64, hostnames []string, svc []ServiceDomain, isLocal bool, sslMode string) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.Exec(`DELETE FROM domains WHERE app_id = ?`, appID); err != nil {
		return err
	}
	il := 0
	if isLocal {
		il = 1
	}
	ins := `INSERT INTO domains (app_id, hostname, is_local, ssl_mode, port) VALUES (?, ?, ?, ?, ?)`
	for _, h := range hostnames {
		if h == "" {
			continue
		}
		if _, err := tx.Exec(ins, appID, h, il, sslMode, 0); err != nil {
			return err
		}
	}
	for _, d := range svc {
		if _, err := tx.Exec(ins, appID, d.Host, il, sslMode, d.Port); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// CreateDomain attaches a hostname to an app (upserting on hostname). A non-zero
// port routes that hostname straight to it (a secondary service like Adminer);
// 0 means the hostname uses the app's own upstream.
func (s *Store) CreateDomain(appID int64, hostname string, isLocal bool, sslMode string, port int) error {
	il := 0
	if isLocal {
		il = 1
	}
	_, err := s.db.Exec(
		`INSERT INTO domains (app_id, hostname, is_local, ssl_mode, port)
		 VALUES (?, ?, ?, ?, ?)
		 ON CONFLICT(hostname) DO UPDATE SET
		   app_id = excluded.app_id, is_local = excluded.is_local,
		   ssl_mode = excluded.ssl_mode, port = excluded.port`,
		appID, hostname, il, sslMode, port,
	)
	return err
}

// ProxyRoutes returns every domain whose app has a routable upstream, for
// building the reverse-proxy config: a published host port (containers and
// command-mode static apps), a serve-mode static app's on-disk directory to
// file-server, a shared-WP site's docroot + fastcgi port, or a proxy app's
// remote upstream URL. Proxy and shared-WP apps only route while running
// (stop = drop the route; there is nothing else to stop).
func (s *Store) ProxyRoutes() ([]RouteInfo, error) {
	// Shared-WP routes need the wp-host settings (written on first shared-site
	// create, so present whenever a shared-WP row exists).
	wpDir, _ := s.GetSetting(WPHostDirKey)
	wpPortStr, _ := s.GetSetting(WPHostFPMPortKey)
	wpPort, _ := strconv.Atoi(wpPortStr)

	rows, err := s.db.Query(`
		SELECT d.hostname, d.port, a.port, d.is_local, a.type, a.serve_mode, a.root_dir, a.upstream, a.wp_mode, a.slug, a.source_dir, p.slug, p.dir
		FROM domains d
		JOIN apps a ON a.id = d.app_id
		JOIN projects p ON p.id = a.project_id
		WHERE d.port > 0 OR a.port > 0 OR (a.type = 'static' AND a.serve_mode = 'serve')
		   OR (a.type = 'proxy' AND a.status = 'running')
		   OR (a.wp_mode = 'shared' AND a.status = 'running')
		ORDER BY d.hostname`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []RouteInfo
	for rows.Next() {
		var r RouteInfo
		var isLocal, domainPort, appPort int
		var appType, serveMode, rootDir, upstream, wpMode, slug, sourceDir, projSlug, projDir string
		if err := rows.Scan(&r.Host, &domainPort, &appPort, &isLocal, &appType, &serveMode, &rootDir, &upstream, &wpMode, &slug, &sourceDir, &projSlug, &projDir); err != nil {
			return nil, err
		}
		r.Local = isLocal == 1
		switch {
		case domainPort > 0 && appType == "mail":
			// Stalwart's admin console serves over HTTPS (self-signed) on its own
			// port; Caddy proxies to it over TLS with verification skipped (loopback).
			r.Upstream = fmt.Sprintf("https://127.0.0.1:%d", domainPort)
		case domainPort > 0:
			// Secondary service domain (e.g. Adminer): route straight to its port.
			r.Port = domainPort
		case appType == TypeProxy:
			// Proxy apps forward to a remote URL instead of a local port.
			r.Upstream = upstream
		case wpMode == WPShared:
			// Shared-WP sites: Caddy file-serves the docroot and hands *.php to
			// the wp-host fpm port. Without the settings there is no PHP handler,
			// and a bare file_server would serve wp-config.php source — skip.
			if wpDir == "" || wpPort == 0 {
				continue
			}
			r.Root = filepath.Join(wpDir, "sites", projSlug+"_"+slug)
			r.FCGIPort = wpPort
		case appType == TypeStatic && serveMode == ServeStatic:
			// Serve-mode static apps have no upstream port; Caddy serves their
			// files from <app dir>/<root_dir> directly — the app dir being the one
			// the user pointed it at, or <project.dir>/<slug> by default.
			base := sourceDir
			if base == "" {
				base = filepath.Join(projDir, slug)
			}
			r.Root = filepath.Join(base, rootDir)
		default:
			r.Port = appPort
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

package store

import "path/filepath"

// RouteInfo is a hostname paired with the upstream its app is served from — the
// pair the reverse proxy needs to route traffic. Exactly one of Port (a host
// port to reverse-proxy to) or Root (a directory for Caddy to file-server) is
// set; Root is used by serve-mode static apps that have no process.
type RouteInfo struct {
	Host  string
	Port  int
	Root  string // filesystem dir to serve directly (serve-mode static apps)
	Local bool   // local (.test, internal CA) vs public (ACME)
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

// ReplaceAppDomain makes hostname the app's primary domain (delete then insert),
// so changing an app's domain doesn't leave the old one routing. It only touches
// the primary domain (port 0) — secondary service domains like Adminer (which
// carry their own non-zero port) are left in place.
func (s *Store) ReplaceAppDomain(appID int64, hostname string, isLocal bool, sslMode string) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	if _, err := tx.Exec(`DELETE FROM domains WHERE app_id = ? AND port = 0`, appID); err != nil {
		tx.Rollback()
		return err
	}
	il := 0
	if isLocal {
		il = 1
	}
	if _, err := tx.Exec(
		`INSERT INTO domains (app_id, hostname, is_local, ssl_mode, port) VALUES (?, ?, ?, ?, 0)`,
		appID, hostname, il, sslMode); err != nil {
		tx.Rollback()
		return err
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
// command-mode static apps), or — for serve-mode static apps — the on-disk
// directory Caddy should file-server directly.
func (s *Store) ProxyRoutes() ([]RouteInfo, error) {
	rows, err := s.db.Query(`
		SELECT d.hostname, d.port, a.port, d.is_local, a.type, a.serve_mode, a.root_dir, a.slug, p.dir
		FROM domains d
		JOIN apps a ON a.id = d.app_id
		JOIN projects p ON p.id = a.project_id
		WHERE d.port > 0 OR a.port > 0 OR (a.type = 'static' AND a.serve_mode = 'serve')
		ORDER BY d.hostname`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []RouteInfo
	for rows.Next() {
		var r RouteInfo
		var isLocal, domainPort, appPort int
		var appType, serveMode, rootDir, slug, projDir string
		if err := rows.Scan(&r.Host, &domainPort, &appPort, &isLocal, &appType, &serveMode, &rootDir, &slug, &projDir); err != nil {
			return nil, err
		}
		r.Local = isLocal == 1
		switch {
		case domainPort > 0:
			// Secondary service domain (e.g. Adminer): route straight to its port.
			r.Port = domainPort
		case appType == TypeStatic && serveMode == ServeStatic:
			// Serve-mode static apps have no upstream port; Caddy serves their
			// files from <project.dir>/<slug>/<root_dir> directly.
			r.Root = filepath.Join(projDir, slug, rootDir)
		default:
			r.Port = appPort
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

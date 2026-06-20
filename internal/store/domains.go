package store

// RouteInfo is a hostname paired with the host port its app publishes — the
// pair the reverse proxy needs to route traffic.
type RouteInfo struct {
	Host  string
	Port  int
	Local bool // local (.test, internal CA) vs public (ACME)
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

// ReplaceAppDomain makes hostname the app's single domain (delete then insert),
// so changing an app's domain doesn't leave the old one routing.
func (s *Store) ReplaceAppDomain(appID int64, hostname string, isLocal bool, sslMode string) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	if _, err := tx.Exec(`DELETE FROM domains WHERE app_id = ?`, appID); err != nil {
		tx.Rollback()
		return err
	}
	il := 0
	if isLocal {
		il = 1
	}
	if _, err := tx.Exec(
		`INSERT INTO domains (app_id, hostname, is_local, ssl_mode) VALUES (?, ?, ?, ?)`,
		appID, hostname, il, sslMode); err != nil {
		tx.Rollback()
		return err
	}
	return tx.Commit()
}

// CreateDomain attaches a hostname to an app (upserting on hostname).
func (s *Store) CreateDomain(appID int64, hostname string, isLocal bool, sslMode string) error {
	il := 0
	if isLocal {
		il = 1
	}
	_, err := s.db.Exec(
		`INSERT INTO domains (app_id, hostname, is_local, ssl_mode)
		 VALUES (?, ?, ?, ?)
		 ON CONFLICT(hostname) DO UPDATE SET
		   app_id = excluded.app_id, is_local = excluded.is_local, ssl_mode = excluded.ssl_mode`,
		appID, hostname, il, sslMode,
	)
	return err
}

// ProxyRoutes returns every domain whose app has a published port, for building
// the reverse-proxy config.
func (s *Store) ProxyRoutes() ([]RouteInfo, error) {
	rows, err := s.db.Query(`
		SELECT d.hostname, a.port, d.is_local
		FROM domains d
		JOIN apps a ON a.id = d.app_id
		WHERE a.port > 0
		ORDER BY d.hostname`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []RouteInfo
	for rows.Next() {
		var r RouteInfo
		var isLocal int
		if err := rows.Scan(&r.Host, &r.Port, &isLocal); err != nil {
			return nil, err
		}
		r.Local = isLocal == 1
		out = append(out, r)
	}
	return out, rows.Err()
}

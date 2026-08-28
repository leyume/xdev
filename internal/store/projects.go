package store

import (
	"database/sql"
	"errors"
	"fmt"
)

// Project is a top-level group of apps (see schema notes). bizepp is the
// canonical example: one project = a Laravel backend app + a Vue frontend app,
// sharing a private network and a base domain.
type Project struct {
	ID          int64
	Name        string
	Slug        string
	BaseDomain  string
	Environment string
	NetworkName string
	Engine      string // container engine this project was created with (podman|docker)
	Dir         string
	CreatedAt   string
	// AppCount is populated by ListProjects for dashboard display.
	AppCount int
}

// CreateProject inserts a project and returns it with its assigned id.
func (s *Store) CreateProject(p Project) (Project, error) {
	// A new project lands at the front, which is where the newest project has
	// always appeared. Below the current minimum rather than at some index: the
	// column is a relative ordering, and the next saved arrangement renumbers
	// the whole list from 1 anyway.
	res, err := s.db.Exec(
		`INSERT INTO projects (name, slug, base_domain, environment, network_name, engine, dir, position)
		 VALUES (?, ?, ?, ?, ?, ?, ?, (SELECT COALESCE(MIN(position), 1) - 1 FROM projects))`,
		p.Name, p.Slug, p.BaseDomain, p.Environment, p.NetworkName, p.Engine, p.Dir,
	)
	if err != nil {
		return Project{}, err
	}
	id, _ := res.LastInsertId()
	return s.ProjectByID(id)
}

// ProjectByID looks up one project (without AppCount).
func (s *Store) ProjectByID(id int64) (Project, error) {
	return s.scanProject(s.db.QueryRow(
		`SELECT id, name, slug, base_domain, environment, network_name, engine, dir, created_at
		 FROM projects WHERE id = ?`, id))
}

// ProjectBySlug looks up one project by slug.
func (s *Store) ProjectBySlug(slug string) (Project, error) {
	return s.scanProject(s.db.QueryRow(
		`SELECT id, name, slug, base_domain, environment, network_name, engine, dir, created_at
		 FROM projects WHERE slug = ?`, slug))
}

// ProjectSlugExists reports whether a slug is already taken.
func (s *Store) ProjectSlugExists(slug string) bool {
	var x int
	err := s.db.QueryRow(`SELECT 1 FROM projects WHERE slug = ?`, slug).Scan(&x)
	return err == nil
}

// RenameProject changes a project's display name.
//
// Only the name. The slug is identity, not presentation: it names the project's
// directory under projects/, its container network, every container in every
// app's stack, and the shared databases those apps authenticate against. Moving
// it is a migration of running infrastructure, not an edit to a row — so the
// name is free to change and the slug never does.
func (s *Store) RenameProject(id int64, name string) error {
	_, err := s.db.Exec(`UPDATE projects SET name = ? WHERE id = ?`, name, id)
	return err
}

// SetProjectConfig changes a project's base domain and environment, and brings
// its apps' existing domain rows into line with the environment.
//
// The restamp is the whole point. environment is read when an app is created or
// edited, and turned into per-domain is_local/ssl_mode — which is what actually
// decides whether Caddy issues an internal certificate or goes to Let's
// Encrypt, and whether the hostname needs an /etc/hosts entry. Changing the
// project's label without touching those rows would leave a project marked
// "local" whose apps still ask ACME for certificates: a switch that appears to
// work and changes nothing.
//
// One transaction, because a project whose environment disagrees with its own
// domains is the exact state this is here to prevent. Applied unconditionally
// rather than only when the environment changed, so a row that drifted — an app
// created before an earlier switch — is repaired rather than left behind.
//
// Nothing is done to the *hostnames*: they belong to the apps, and an app named
// after the old base domain keeps answering on it. Changing the base domain
// only changes what a future app is named after.
func (s *Store) SetProjectConfig(id int64, baseDomain, environment string, isLocal bool, sslMode string) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()

	if _, err := tx.Exec(
		`UPDATE projects SET base_domain = ?, environment = ? WHERE id = ?`,
		baseDomain, environment, id); err != nil {
		return err
	}
	if _, err := tx.Exec(
		`UPDATE domains SET is_local = ?, ssl_mode = ?
		 WHERE app_id IN (SELECT id FROM apps WHERE project_id = ?)`,
		isLocal, sslMode, id); err != nil {
		return err
	}
	return tx.Commit()
}

// DeleteProject removes a project row. Apps cascade via the FK.
func (s *Store) DeleteProject(id int64) error {
	_, err := s.db.Exec(`DELETE FROM projects WHERE id = ?`, id)
	return err
}

// ListProjects returns all projects with their app counts, newest first.
func (s *Store) ListProjects() ([]Project, error) {
	rows, err := s.db.Query(`
		SELECT p.id, p.name, p.slug, p.base_domain, p.environment,
		       p.network_name, p.engine, p.dir, p.created_at,
		       (SELECT COUNT(*) FROM apps a WHERE a.project_id = p.id) AS app_count
		FROM projects p
		ORDER BY p.position ASC, p.created_at DESC, p.id DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []Project
	for rows.Next() {
		var p Project
		if err := rows.Scan(&p.ID, &p.Name, &p.Slug, &p.BaseDomain, &p.Environment,
			&p.NetworkName, &p.Engine, &p.Dir, &p.CreatedAt, &p.AppCount); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

func (s *Store) scanProject(row *sql.Row) (Project, error) {
	var p Project
	err := row.Scan(&p.ID, &p.Name, &p.Slug, &p.BaseDomain, &p.Environment,
		&p.NetworkName, &p.Engine, &p.Dir, &p.CreatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return Project{}, ErrNotFound
	}
	return p, err
}

// ErrStaleProjectOrder reports that a submitted project order does not describe
// the set of projects as it stands — the page that produced it has since gone
// out of date.
var ErrStaleProjectOrder = errors.New("this list of projects no longer matches what is saved")

// SetProjectOrder rewrites project positions to match ids, which must name
// every project exactly once.
//
// The whole list is required rather than a moved-card-and-target pair, for the
// same reason SetAppOrder demands one: the client already knows the final
// order, and sending it whole is the only version that cannot drift. A partial
// update has to reason about what the server currently holds, and two people
// dragging cards at once would interleave into an order neither of them chose.
//
// A mismatched list is rejected outright rather than applied to the ids it does
// recognise. A page open since before a project was created would otherwise
// save an order that silently drops the one it never knew about.
func (s *Store) SetProjectOrder(ids []int64) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()

	rows, err := tx.Query(`SELECT id FROM projects`)
	if err != nil {
		return err
	}
	current := map[int64]bool{}
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return err
		}
		current[id] = true
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}

	if len(ids) != len(current) {
		return fmt.Errorf("%w: %d projects submitted, there are %d", ErrStaleProjectOrder, len(ids), len(current))
	}
	seen := make(map[int64]bool, len(ids))
	for _, id := range ids {
		if !current[id] {
			return fmt.Errorf("%w: project %d does not exist", ErrStaleProjectOrder, id)
		}
		if seen[id] {
			return fmt.Errorf("%w: project %d listed twice", ErrStaleProjectOrder, id)
		}
		seen[id] = true
	}

	stmt, err := tx.Prepare(`UPDATE projects SET position = ? WHERE id = ?`)
	if err != nil {
		return err
	}
	defer stmt.Close()
	// From 1, so the negative positions CreateProject hands out to keep new
	// projects at the front are tidied away the first time anything is dragged.
	for i, id := range ids {
		if _, err := stmt.Exec(i+1, id); err != nil {
			return err
		}
	}
	return tx.Commit()
}

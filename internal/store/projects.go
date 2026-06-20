package store

import (
	"database/sql"
	"errors"
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
	res, err := s.db.Exec(
		`INSERT INTO projects (name, slug, base_domain, environment, network_name, engine, dir)
		 VALUES (?, ?, ?, ?, ?, ?, ?)`,
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
		ORDER BY p.created_at DESC, p.id DESC`)
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

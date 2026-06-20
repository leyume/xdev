package store

import (
	"database/sql"
	"errors"
)

// App statuses.
const (
	AppStopped = "stopped"
	AppRunning = "running"
	AppError   = "error"
)

// App is one deployable component (compose stack) inside a project.
type App struct {
	ID          int64
	ProjectID   int64
	Name        string
	Slug        string
	Type        string // wordpress | laravel | static-prebuilt | static-build
	Runtime     string // podman | docker ("" = use default)
	Status      string
	Domain      string // full hostname this app is served at (e.g. aa.test, api.aa.test)
	CPULimit    float64 // cores; 0 = unlimited
	MemLimit    int64   // bytes; 0 = unlimited
	Port        int     // host port (0 = none)
	ComposePath string
	CreatedAt   string
	UpdatedAt   string
}

// CreateApp inserts an app and returns it with its assigned id.
func (s *Store) CreateApp(a App) (App, error) {
	res, err := s.db.Exec(
		`INSERT INTO apps (project_id, name, slug, type, runtime, status, subdomain,
		                   cpu_limit, mem_limit, port, compose_path)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		a.ProjectID, a.Name, a.Slug, a.Type, a.Runtime, statusOr(a.Status),
		a.Domain, a.CPULimit, a.MemLimit, a.Port, a.ComposePath,
	)
	if err != nil {
		return App{}, err
	}
	id, _ := res.LastInsertId()
	return s.AppByID(id)
}

// AppByID looks up one app.
func (s *Store) AppByID(id int64) (App, error) {
	return s.scanApp(s.db.QueryRow(appSelect+` WHERE id = ?`, id))
}

// AppBySlug looks up one app within a project.
func (s *Store) AppBySlug(projectID int64, slug string) (App, error) {
	return s.scanApp(s.db.QueryRow(appSelect+` WHERE project_id = ? AND slug = ?`, projectID, slug))
}

// AppSlugExists reports whether a slug is taken within a project.
func (s *Store) AppSlugExists(projectID int64, slug string) bool {
	var x int
	err := s.db.QueryRow(`SELECT 1 FROM apps WHERE project_id = ? AND slug = ?`, projectID, slug).Scan(&x)
	return err == nil
}

// ListAppsByProject returns a project's apps, oldest first (creation order).
func (s *Store) ListAppsByProject(projectID int64) ([]App, error) {
	rows, err := s.db.Query(appSelect+` WHERE project_id = ? ORDER BY id ASC`, projectID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []App
	for rows.Next() {
		a, err := scanAppRows(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// SetAppStatus updates an app's lifecycle status and bumps updated_at.
func (s *Store) SetAppStatus(id int64, status string) error {
	_, err := s.db.Exec(
		`UPDATE apps SET status = ?, updated_at = datetime('now') WHERE id = ?`, status, id)
	return err
}

// UsedPorts returns every non-zero host port currently assigned to an app, so
// the allocator can avoid collisions.
func (s *Store) UsedPorts() ([]int, error) {
	rows, err := s.db.Query(`SELECT port FROM apps WHERE port > 0`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var ports []int
	for rows.Next() {
		var p int
		if err := rows.Scan(&p); err != nil {
			return nil, err
		}
		ports = append(ports, p)
	}
	return ports, rows.Err()
}

// DeleteApp removes an app row.
func (s *Store) DeleteApp(id int64) error {
	_, err := s.db.Exec(`DELETE FROM apps WHERE id = ?`, id)
	return err
}

const appSelect = `SELECT id, project_id, name, slug, type, runtime, status, subdomain,
	cpu_limit, mem_limit, port, compose_path, created_at, updated_at FROM apps`

func (s *Store) scanApp(row *sql.Row) (App, error) {
	var a App
	err := row.Scan(&a.ID, &a.ProjectID, &a.Name, &a.Slug, &a.Type, &a.Runtime,
		&a.Status, &a.Domain, &a.CPULimit, &a.MemLimit, &a.Port, &a.ComposePath,
		&a.CreatedAt, &a.UpdatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return App{}, ErrNotFound
	}
	return a, err
}

func scanAppRows(rows *sql.Rows) (App, error) {
	var a App
	err := rows.Scan(&a.ID, &a.ProjectID, &a.Name, &a.Slug, &a.Type, &a.Runtime,
		&a.Status, &a.Domain, &a.CPULimit, &a.MemLimit, &a.Port, &a.ComposePath,
		&a.CreatedAt, &a.UpdatedAt)
	return a, err
}

func statusOr(s string) string {
	if s == "" {
		return AppStopped
	}
	return s
}

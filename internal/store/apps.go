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

// Static app types and serve modes.
const (
	TypeStatic = "static" // runs on system Node / served by Caddy, no container

	ServeStatic  = "serve"   // Caddy file-servers RootDir directly (no process)
	ServeCommand = "command" // xdev supervises StartCmd as a host process on Port
)

// App is one deployable component inside a project. Container apps (wordpress,
// laravel) are a compose stack; static apps run on the host (system Node or
// Caddy file-server) and carry the serve_mode/*_cmd fields instead.
type App struct {
	ID          int64
	ProjectID   int64
	Name        string
	Slug        string
	Type        string // wordpress | laravel | static
	Runtime     string // podman | docker ("" = use default)
	Status      string
	Domain      string  // full hostname this app is served at (e.g. aa.test, api.aa.test)
	CPULimit    float64 // cores; 0 = unlimited
	MemLimit    int64   // bytes; 0 = unlimited
	Port        int     // host port (0 = none)
	ComposePath string
	// Static-app config (blank for container apps; see migration 0004).
	ServeMode string // serve | command
	RootDir   string // served subdir for serve mode ("" = the app folder)
	BuildCmd  string // optional one-shot build step (system Node)
	StartCmd  string // long-lived command for command mode (system Node)
	CreatedAt string
	UpdatedAt string
}

// IsStatic reports whether the app runs on the host rather than in a container.
func (a App) IsStatic() bool { return a.Type == TypeStatic }

// CreateApp inserts an app and returns it with its assigned id.
func (s *Store) CreateApp(a App) (App, error) {
	res, err := s.db.Exec(
		`INSERT INTO apps (project_id, name, slug, type, runtime, status, subdomain,
		                   cpu_limit, mem_limit, port, compose_path,
		                   serve_mode, root_dir, build_cmd, start_cmd)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		a.ProjectID, a.Name, a.Slug, a.Type, a.Runtime, statusOr(a.Status),
		a.Domain, a.CPULimit, a.MemLimit, a.Port, a.ComposePath,
		a.ServeMode, a.RootDir, a.BuildCmd, a.StartCmd,
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

// ResumableStaticApps returns command-mode static apps that were running when
// xdev last stopped — their host processes died with xdev and must be respawned
// on boot (unlike containers, which the engine keeps alive).
func (s *Store) ResumableStaticApps() ([]App, error) {
	rows, err := s.db.Query(appSelect+` WHERE type = ? AND serve_mode = ? AND status = ?`,
		TypeStatic, ServeCommand, AppRunning)
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

const appSelect = `SELECT id, project_id, name, slug, type, runtime, status, subdomain,
	cpu_limit, mem_limit, port, compose_path,
	serve_mode, root_dir, build_cmd, start_cmd, created_at, updated_at FROM apps`

func (s *Store) scanApp(row *sql.Row) (App, error) {
	var a App
	err := row.Scan(&a.ID, &a.ProjectID, &a.Name, &a.Slug, &a.Type, &a.Runtime,
		&a.Status, &a.Domain, &a.CPULimit, &a.MemLimit, &a.Port, &a.ComposePath,
		&a.ServeMode, &a.RootDir, &a.BuildCmd, &a.StartCmd, &a.CreatedAt, &a.UpdatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return App{}, ErrNotFound
	}
	return a, err
}

func scanAppRows(rows *sql.Rows) (App, error) {
	var a App
	err := rows.Scan(&a.ID, &a.ProjectID, &a.Name, &a.Slug, &a.Type, &a.Runtime,
		&a.Status, &a.Domain, &a.CPULimit, &a.MemLimit, &a.Port, &a.ComposePath,
		&a.ServeMode, &a.RootDir, &a.BuildCmd, &a.StartCmd, &a.CreatedAt, &a.UpdatedAt)
	return a, err
}

func statusOr(s string) string {
	if s == "" {
		return AppStopped
	}
	return s
}

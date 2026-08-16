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
	TypeStatic  = "static"  // runs on system Node / served by Caddy, no container
	TypeProxy   = "proxy"   // a Caddy route to another server — no container/process/port
	TypeGo      = "go"      // built and run on the host with the system Go toolchain, no container
	TypeCompose = "compose" // a user-supplied docker-compose.yml / compose.yml, run as-is

	ServeStatic  = "serve"   // Caddy file-servers RootDir directly (no process)
	ServeCommand = "command" // xdev supervises StartCmd as a host process on Port
)

// DBShared marks an app whose database lives on the shared xdev-db MariaDB
// server instead of a per-app db service ("" = dedicated / not applicable).
const DBShared = "shared"

// The PHP server a laravel app runs on (migration 0015). LaravelSwoole is the
// zero value's meaning as well as its name: an app row written before the
// column existed reads as "", and must keep the Swoole stack it was built with.
const (
	LaravelSwoole = "swoole" // Octane/Swoole — framework resident per worker
	LaravelFPM    = "fpm"    // php-fpm — per-request bootstrap, idle workers reaped
)

// LaravelServerOr resolves a stored value to a server name, mapping "" (and
// anything unrecognised) to Swoole. Every reader goes through this rather than
// comparing against "" directly, so the default lives in exactly one place.
func LaravelServerOr(v string) string {
	if v == LaravelFPM {
		return LaravelFPM
	}
	return LaravelSwoole
}

// WPShared marks a WordPress app served by the shared platform wp-host (the
// xdev-wp PHP-FPM container) from data/wp/sites/ — no container, compose file,
// or port of its own ("" = separate container / not applicable).
const WPShared = "shared"

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
	// SourceDir points a host app at a directory the user already has, instead of
	// the one xdev would create at <project.dir>/<slug> ("" = managed; see
	// migration 0010). Absolute path, validated on the way in.
	SourceDir string
	// Git source (migration 0011): a host app whose code is a clone of a
	// repository rather than a scaffold or a folder the user points at. Mutually
	// exclusive with SourceDir — a deploy resets the working tree hard.
	GitURL      string // canonical https://host/owner/name ("" = not a git app)
	GitRef      string // branch or tag ("" = the remote's default branch)
	GitSubdir   string // build from this subdirectory ("" = the repo root)
	DeployedSHA string // commit currently built and served ("" = never deployed)
	DeployedAt  string
	// Deploy endpoints served on this app's own hostname under /_xdev/
	// (migration 0012). Both are off until switched on from the settings page.
	HookID        string // webhook path segment ("" = no webhook)
	HookSecret    string // HMAC secret shared with GitHub, encrypted at rest
	PushTokenHash string // sha256 of the CI deploy token ("" = push disabled)
	PushTokenHint string // its first few characters, for the UI
	// SkipDBDump opts a container app out of the database dump a deploy takes
	// before applying pending migrations (migration 0013). Off by default: the
	// dump is the only way back from a migration that fails halfway, since
	// files roll back with a rename and a schema does not.
	SkipDBDump bool
	// LaravelServer selects a laravel app's PHP server (migration 0015):
	// "" or LaravelSwoole for Octane/Swoole, LaravelFPM for php-fpm. Blank is
	// Swoole so apps created before the column existed keep their stack.
	LaravelServer string
	// Proxy-app config (see migration 0007).
	Upstream  string // URL the domain forwards to (http(s)://host[:port])
	DBMode    string // "shared" = uses xdev-db; "" = dedicated / n/a (migration 0008)
	WPMode    string // "shared" = served by wp-host; "" = own container / n/a (migration 0009)
	CreatedAt string
	UpdatedAt string
}

// IsHostProc reports whether the app runs on the host (static or go) rather
// than in a container.
func (a App) IsHostProc() bool { return a.Type == TypeStatic || a.Type == TypeGo }

// IsExternalDir reports whether the app's files live in a directory the user
// pointed it at rather than one xdev created. Such a directory is never
// scaffolded into and never deleted with the app — xdev is a tenant there, not
// its owner.
func (a App) IsExternalDir() bool { return a.SourceDir != "" }

// IsGit reports whether the app's code comes from a repository xdev clones and
// keeps up to date, rather than files somebody puts there.
func (a App) IsGit() bool { return a.GitURL != "" }

// HasEndpoints reports whether this app publishes any /_xdev/ deploy endpoint,
// which is what decides whether Caddy routes those paths for its hostname.
func (a App) HasEndpoints() bool { return a.HookID != "" || a.PushTokenHash != "" }

// IsProxy reports whether the app is only a route to another server.
func (a App) IsProxy() bool { return a.Type == TypeProxy }

// IsCompose reports whether the app is a bring-your-own compose stack: xdev runs
// the file the user supplied instead of one it rendered from a template, so the
// compose file (and the _/.env beside it) is user content, not generated output.
func (a App) IsCompose() bool { return a.Type == TypeCompose }

// IsSharedWP reports whether the app is a shared-host WordPress site: a docroot
// served by the platform wp-host, with no container of its own (route-only
// lifecycle, like proxy apps).
func (a App) IsSharedWP() bool { return a.Type == "wordpress" && a.WPMode == WPShared }

// CreateApp inserts an app and returns it with its assigned id.
func (s *Store) CreateApp(a App) (App, error) {
	res, err := s.db.Exec(
		`INSERT INTO apps (project_id, name, slug, type, runtime, status, subdomain,
		                   cpu_limit, mem_limit, port, compose_path,
		                   serve_mode, root_dir, build_cmd, start_cmd, upstream, db_mode, wp_mode,
		                   source_dir, git_url, git_ref, git_subdir, laravel_server)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		a.ProjectID, a.Name, a.Slug, a.Type, a.Runtime, statusOr(a.Status),
		a.Domain, a.CPULimit, a.MemLimit, a.Port, a.ComposePath,
		a.ServeMode, a.RootDir, a.BuildCmd, a.StartCmd, a.Upstream, a.DBMode, a.WPMode,
		a.SourceDir, a.GitURL, a.GitRef, a.GitSubdir, a.LaravelServer,
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

// UpdateApp saves the columns an app's settings page can change. Identity is
// deliberately not among them: id, project, slug, type, runtime, db_mode and
// wp_mode name the app's containers, database and route shape, so changing one
// is a migration rather than an edit.
//
// source_dir is on the editable side even though it names a directory: a host
// app's files aren't generated, so re-pointing one only changes where its build
// and its served folder are read from. Nothing moves and nothing is deleted.
func (s *Store) UpdateApp(a App) error {
	_, err := s.db.Exec(
		`UPDATE apps SET name = ?, subdomain = ?, cpu_limit = ?, mem_limit = ?, port = ?,
		                 serve_mode = ?, root_dir = ?, build_cmd = ?, start_cmd = ?, upstream = ?,
		                 source_dir = ?, git_url = ?, git_ref = ?, git_subdir = ?,
		                 updated_at = datetime('now')
		 WHERE id = ?`,
		a.Name, a.Domain, a.CPULimit, a.MemLimit, a.Port,
		a.ServeMode, a.RootDir, a.BuildCmd, a.StartCmd, a.Upstream, a.SourceDir,
		a.GitURL, a.GitRef, a.GitSubdir, a.ID,
	)
	return err
}

// SetAppStatus updates an app's lifecycle status and bumps updated_at.
func (s *Store) SetAppStatus(id int64, status string) error {
	_, err := s.db.Exec(
		`UPDATE apps SET status = ?, updated_at = datetime('now') WHERE id = ?`, status, id)
	return err
}

// SetAppPort repoints an app at a different host port. Used when a compose app's
// file is edited to publish somewhere else: the proxy route is rebuilt from
// apps.port, so the stored port has to follow the file.
func (s *Store) SetAppPort(id int64, port int) error {
	_, err := s.db.Exec(
		`UPDATE apps SET port = ?, updated_at = datetime('now') WHERE id = ?`, port, id)
	return err
}

// UsedPorts returns every non-zero host port currently assigned — app ports,
// secondary-service domain ports (e.g. Adminer), and the wp-host fpm port (a
// settings row, not an app) — so the allocator can avoid collisions even while
// the owning container is stopped.
func (s *Store) UsedPorts() ([]int, error) {
	rows, err := s.db.Query(
		`SELECT port FROM apps WHERE port > 0
		 UNION SELECT port FROM domains WHERE port > 0
		 UNION SELECT CAST(value AS INTEGER) FROM settings WHERE key = ? AND CAST(value AS INTEGER) > 0`,
		WPHostFPMPortKey)
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

// ResumableStaticApps returns command-mode host apps (static, go) that were
// running when xdev last stopped — their host processes died with xdev and must
// be respawned on boot (unlike containers, which the engine keeps alive).
func (s *Store) ResumableStaticApps() ([]App, error) {
	rows, err := s.db.Query(appSelect+` WHERE type IN (?, ?) AND serve_mode = ? AND status = ?`,
		TypeStatic, TypeGo, ServeCommand, AppRunning)
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

// SetDeployed records the commit an app is now built from. Written by a deploy
// (and by the first clone), separately from UpdateApp: it is observed state,
// not something the settings form can set.
func (s *Store) SetDeployed(id int64, sha string) error {
	_, err := s.db.Exec(
		`UPDATE apps SET deployed_sha = ?, deployed_at = datetime('now'),
		                 updated_at = datetime('now') WHERE id = ?`, sha, id)
	return err
}

const appSelect = `SELECT id, project_id, name, slug, type, runtime, status, subdomain,
	cpu_limit, mem_limit, port, compose_path,
	serve_mode, root_dir, build_cmd, start_cmd, upstream, db_mode, wp_mode, source_dir,
	git_url, git_ref, git_subdir, deployed_sha, deployed_at,
	hook_id, hook_secret, push_token_hash, push_token_hint, skip_db_dump,
	laravel_server, created_at, updated_at FROM apps`

func (s *Store) scanApp(row *sql.Row) (App, error) {
	var a App
	err := row.Scan(&a.ID, &a.ProjectID, &a.Name, &a.Slug, &a.Type, &a.Runtime,
		&a.Status, &a.Domain, &a.CPULimit, &a.MemLimit, &a.Port, &a.ComposePath,
		&a.ServeMode, &a.RootDir, &a.BuildCmd, &a.StartCmd, &a.Upstream, &a.DBMode, &a.WPMode,
		&a.SourceDir, &a.GitURL, &a.GitRef, &a.GitSubdir, &a.DeployedSHA, &a.DeployedAt,
		&a.HookID, &a.HookSecret, &a.PushTokenHash, &a.PushTokenHint, &a.SkipDBDump,
		&a.LaravelServer, &a.CreatedAt, &a.UpdatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return App{}, ErrNotFound
	}
	return a, err
}

func scanAppRows(rows *sql.Rows) (App, error) {
	var a App
	err := rows.Scan(&a.ID, &a.ProjectID, &a.Name, &a.Slug, &a.Type, &a.Runtime,
		&a.Status, &a.Domain, &a.CPULimit, &a.MemLimit, &a.Port, &a.ComposePath,
		&a.ServeMode, &a.RootDir, &a.BuildCmd, &a.StartCmd, &a.Upstream, &a.DBMode, &a.WPMode,
		&a.SourceDir, &a.GitURL, &a.GitRef, &a.GitSubdir, &a.DeployedSHA, &a.DeployedAt,
		&a.HookID, &a.HookSecret, &a.PushTokenHash, &a.PushTokenHint, &a.SkipDBDump,
		&a.LaravelServer, &a.CreatedAt, &a.UpdatedAt)
	return a, err
}

func statusOr(s string) string {
	if s == "" {
		return AppStopped
	}
	return s
}

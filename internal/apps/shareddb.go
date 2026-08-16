// Shared MariaDB platform service (PLANx §B): one xdev-managed `xdev-db`
// container on the external `xdev_shared` network serves every shared-mode
// wordpress/laravel app, instead of a ~200MB MariaDB per app. SQL runs through
// `<engine> exec xdev-db mariadb ...` — no Go MySQL driver needed.
//
// ponytail: one MariaDB is a shared blast radius — a restart/crash/upgrade
// touches every shared-mode site at once. Acceptable at this scale; per-app
// "dedicated" mode is the escape hatch for a site that needs isolation. The
// server also lives on whichever engine first opted in — mixing docker and
// podman shared-mode apps would need one server per engine.
package apps

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"xdev/internal/runtime"
	"xdev/internal/store"
	"xdev/internal/templates"
)

// identRE guards database/user identifiers we interpolate into DDL (no MySQL
// placeholders for those). validIdent keeps them to a safe charset so the
// interpolation can't inject.
var identRE = regexp.MustCompile(`^[A-Za-z0-9_]{1,64}$`)

// hostRE guards the host part of a MySQL account ('name'@'host').
var hostRE = regexp.MustCompile(`^[A-Za-z0-9_.%:-]{1,255}$`)

func validIdent(s string) bool   { return identRE.MatchString(s) }
func validHostPat(s string) bool { return hostRE.MatchString(s) }

// sqlStr escapes a value for a single-quoted MySQL string literal (passwords —
// which can't go through DDL placeholders). Backslash first, then quote.
func sqlStr(s string) string {
	s = strings.ReplaceAll(s, `\`, `\\`)
	return strings.ReplaceAll(s, `'`, `\'`)
}

const (
	sharedDBContainer = "xdev-db"
	sharedDBNetwork   = "xdev_shared"
	sharedDBImage     = "docker.io/library/mariadb:11"
	sharedDBVolume    = "xdev_db_data" // engine-managed named volume
	sharedDBRootKey   = "shared_db_root_password"
	sharedDBPortKey   = "shared_db_host_port" // 127.0.0.1 port the server is published on

	adminerContainer = "xdev-adminer"
	adminerImage     = "docker.io/library/adminer:latest"
	adminerPortKey   = "shared_adminer_port"
)

// sharedDBRunArgs is the `<engine> run` invocation for the shared MariaDB —
// shared by first-run creation and the update/recreate path so they can't drift.
// hostPort (0 = none) publishes the server on 127.0.0.1 so host-process apps
// (go) can reach it; containers use the xdev-db hostname on xdev_shared instead.
func sharedDBRunArgs(rootPass string, hostPort int) []string {
	args := []string{"run", "-d",
		"--name", sharedDBContainer, "--restart", "unless-stopped",
		"--network", sharedDBNetwork,
		"-e", "MARIADB_ROOT_PASSWORD=" + rootPass,
		"-v", sharedDBVolume + ":/var/lib/mysql"}
	if hostPort > 0 {
		args = append(args, "-p", fmt.Sprintf("127.0.0.1:%d:3306", hostPort))
	}
	return append(args, sharedDBImage)
}

// SharedDBName is the database AND user name for an app on the shared server:
// <project>_<app>, with slug dashes flattened to underscores (MySQL-identifier
// safe without quoting games).
func SharedDBName(projectSlug, appSlug string) string {
	return strings.ReplaceAll(projectSlug+"_"+appSlug, "-", "_")
}

// ensureSharedDB lazily brings up the shared MariaDB: creates the xdev_shared
// network, generates + persists the root password on first use, starts (or
// creates) the xdev-db container, and waits until it answers SQL. Idempotent.
func (s *Service) ensureSharedDB(ctx context.Context, engine runtime.Engine) (rootPass string, err error) {
	rootPass, err = s.store.GetSetting(sharedDBRootKey)
	if err != nil {
		return "", err
	}
	if rootPass == "" {
		rootPass = randHex(16)
		if err := s.store.SetSetting(sharedDBRootKey, rootPass); err != nil {
			return "", err
		}
	}
	if err := runtime.NetworkCreate(ctx, engine, sharedDBNetwork); err != nil {
		return "", err
	}
	// Missing container -> run it (first use pulls the image); stopped -> start.
	out, err := runtime.Exec(ctx, engine, "container", "inspect",
		"--format", "{{.State.Running}}", sharedDBContainer)
	switch {
	case err != nil:
		if _, err := runtime.Exec(ctx, engine, sharedDBRunArgs(rootPass, s.storedSharedDBPort())...); err != nil {
			return "", err
		}
	case strings.TrimSpace(out) != "true":
		if _, err := runtime.Exec(ctx, engine, "start", sharedDBContainer); err != nil {
			return "", err
		}
	}
	// A fresh MariaDB takes a few seconds to initialize; poll until SQL answers.
	deadline := time.Now().Add(90 * time.Second)
	for {
		_, err = s.sharedSQL(ctx, engine, rootPass, "SELECT 1")
		if err == nil {
			return rootPass, nil
		}
		if time.Now().After(deadline) || ctx.Err() != nil {
			return "", fmt.Errorf("shared MariaDB (%s) did not become ready: %w", sharedDBContainer, err)
		}
		time.Sleep(2 * time.Second)
	}
}

// sharedSQL runs statements against the shared server via `<engine> exec`. The
// root password travels in the MYSQL_PWD env of the exec'd process, not argv,
// so it doesn't show up in host process lists.
func (s *Service) sharedSQL(ctx context.Context, engine runtime.Engine, rootPass, stmts string) (string, error) {
	return runtime.Exec(ctx, engine, "exec", "-e", "MYSQL_PWD="+rootPass,
		sharedDBContainer, "mariadb", "-uroot", "-e", stmts)
}

// provisionSharedDB creates the app's database and user (name db, generated
// password) on the shared server, bringing the server up first if needed, and
// returns the password for injection into the app's compose env.
func (s *Service) provisionSharedDB(ctx context.Context, engine runtime.Engine, db string) (string, error) {
	rootPass, err := s.ensureSharedDB(ctx, engine)
	if err != nil {
		return "", err
	}
	pass := randHex(16)
	// IF NOT EXISTS + ALTER keeps a retried create idempotent while ensuring the
	// password we return is the one that's live.
	stmts := fmt.Sprintf("CREATE DATABASE IF NOT EXISTS `%[1]s`;"+
		" CREATE USER IF NOT EXISTS '%[1]s'@'%%' IDENTIFIED BY '%[2]s';"+
		" ALTER USER '%[1]s'@'%%' IDENTIFIED BY '%[2]s';"+
		" GRANT ALL PRIVILEGES ON `%[1]s`.* TO '%[1]s'@'%%';"+
		" FLUSH PRIVILEGES;", db, pass)
	if _, err := s.sharedSQL(ctx, engine, rootPass, stmts); err != nil {
		return "", err
	}
	return pass, nil
}

// storedSharedDBPort is the already-allocated loopback port (0 if a host app
// has never asked for one). Creation/recreation paths pass it through so they
// don't silently drop the publish an existing go app depends on.
func (s *Service) storedSharedDBPort() int {
	portStr, _ := s.store.GetSetting(sharedDBPortKey)
	port, _ := strconv.Atoi(portStr)
	return port
}

// sharedDBHostPort returns the 127.0.0.1 port the shared MariaDB is published
// on, so host-process apps (which can't resolve the xdev-db container name on
// the engine's network) can connect. The port is allocated once and remembered;
// a server that predates it — or one recreated without it — is recreated with
// the publish flag, which is safe (the data lives in the named volume).
// ponytail: recreating briefly drops every shared-mode app's connections. Only
// happens the first time a host app asks for a database; a live-migrate path is
// the upgrade if that blip ever matters.
func (s *Service) sharedDBHostPort(ctx context.Context, engine runtime.Engine, avoid ...int) (int, error) {
	rootPass, err := s.ensureSharedDB(ctx, engine)
	if err != nil {
		return 0, err
	}
	port := s.storedSharedDBPort()
	if port == 0 {
		if port, err = s.allocPort(avoid...); err != nil {
			return 0, err
		}
		if err := s.store.SetSetting(sharedDBPortKey, strconv.Itoa(port)); err != nil {
			return 0, err
		}
	}
	// Already publishing it? Nothing to do.
	if out, err := runtime.Exec(ctx, engine, "port", sharedDBContainer, "3306/tcp"); err == nil &&
		strings.Contains(out, strconv.Itoa(port)) {
		return port, nil
	}
	if _, err := runtime.Exec(ctx, engine, "rm", "-f", sharedDBContainer); err != nil {
		return 0, err
	}
	if _, err := runtime.Exec(ctx, engine, sharedDBRunArgs(rootPass, port)...); err != nil {
		return 0, err
	}
	if _, err := s.ensureSharedDB(ctx, engine); err != nil { // wait until it answers SQL again
		return 0, err
	}
	return port, nil
}

// dumpSharedDB writes a timestamped mariadb-dump of the app's shared database
// into its backups dir (next to the .tar.gz app backups, so the existing
// list/download flow picks it up) and returns the file path.
func (s *Service) dumpSharedDB(ctx context.Context, engine runtime.Engine, app store.App, db, backupsRoot string) (string, error) {
	rootPass, err := s.store.GetSetting(sharedDBRootKey)
	if err != nil || rootPass == "" {
		return "", fmt.Errorf("shared db root password not found: %v", err)
	}
	dir, err := s.backupsDirFor(app, backupsRoot)
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	dest := filepath.Join(dir, time.Now().Format("20060102-150405")+"-db.sql")
	err = runtime.ExecToFile(ctx, engine, dest, "exec", "-e", "MYSQL_PWD="+rootPass,
		sharedDBContainer, "mariadb-dump", "--databases", db)
	if err != nil {
		return "", err
	}
	return dest, nil
}

// archiveSharedDB dumps a shared-mode app's database into the backups dir and,
// only when the dump landed, drops it — so a hiccup (shared server down, disk
// full) can't silently lose the data. Failures are logged, not fatal: it runs
// on the app-delete path, which proceeds regardless.
func (s *Service) archiveSharedDB(ctx context.Context, engine runtime.Engine, app store.App, projSlug, backupsRoot string) {
	db := SharedDBName(projSlug, app.Slug)
	if _, err := s.dumpSharedDB(ctx, engine, app, db, backupsRoot); err != nil {
		log.Printf("dump shared db %s: %v (leaving database in place)", db, err)
	} else if err := s.dropSharedDB(ctx, engine, db); err != nil {
		log.Printf("drop shared db %s: %v", db, err)
	}
}

// dropSharedDB removes the app's database and user from the shared server.
func (s *Service) dropSharedDB(ctx context.Context, engine runtime.Engine, db string) error {
	rootPass, err := s.store.GetSetting(sharedDBRootKey)
	if err != nil || rootPass == "" {
		return fmt.Errorf("shared db root password not found: %v", err)
	}
	_, err = s.sharedSQL(ctx, engine, rootPass,
		fmt.Sprintf("DROP DATABASE IF EXISTS `%[1]s`; DROP USER IF EXISTS '%[1]s'@'%%';", db))
	return err
}

// --- Management surface for the /database settings page ---

// SharedDB is one database on the shared server, with its owning app resolved.
type SharedDB struct {
	Name   string
	SizeMB string // megabytes, one decimal (as MariaDB reports it)
	App    string // "project / app" owner, "" if it maps to no known app
}

// SharedDBInfo is the shared-MariaDB status the settings page renders.
type SharedDBInfo struct {
	Configured  bool // root password generated => the service has been used
	Running     bool // xdev-db container up on the current engine
	Container   string
	Image       string
	Network     string
	Host        string // hostname apps reach it at (inside xdev_shared)
	Port        int
	RootPass    string
	Databases   []SharedDB
	AdminerPort int // host port of the running Adminer helper; 0 = not running
}

// SharedDBInfo reports the shared server's status plus its per-app databases.
// It never starts anything — a stopped/absent server just reports Running=false.
func (s *Service) SharedDBInfo(ctx context.Context) (SharedDBInfo, error) {
	engine := s.sel.Current()
	info := SharedDBInfo{
		Container: sharedDBContainer, Image: sharedDBImage,
		Network: sharedDBNetwork, Host: sharedDBContainer, Port: 3306,
	}
	rootPass, err := s.store.GetSetting(sharedDBRootKey)
	if err != nil {
		return info, err
	}
	info.Configured = rootPass != ""
	info.RootPass = rootPass
	info.Running = containerRunning(ctx, engine, sharedDBContainer)
	if p, _ := s.store.GetSetting(adminerPortKey); p != "" && containerRunning(ctx, engine, adminerContainer) {
		info.AdminerPort, _ = strconv.Atoi(p)
	}
	if info.Running && rootPass != "" {
		info.Databases = s.sharedDatabases(ctx, engine, rootPass)
	}
	return info, nil
}

// containerRunning reports whether a named container exists and is running on
// the given engine (a missing container inspects with an error => false).
func containerRunning(ctx context.Context, engine runtime.Engine, name string) bool {
	out, err := runtime.Exec(ctx, engine, "container", "inspect", "--format", "{{.State.Running}}", name)
	return err == nil && strings.TrimSpace(out) == "true"
}

// sharedDatabases lists every app database (schemas minus the server-internal
// ones) with its on-disk size and resolved owner.
func (s *Service) sharedDatabases(ctx context.Context, engine runtime.Engine, rootPass string) []SharedDB {
	const q = `SELECT s.schema_name, ROUND(COALESCE(SUM(t.data_length+t.index_length),0)/1048576,1)
		FROM information_schema.schemata s
		LEFT JOIN information_schema.tables t ON t.table_schema = s.schema_name
		WHERE s.schema_name NOT IN ('mysql','information_schema','performance_schema','sys')
		GROUP BY s.schema_name ORDER BY s.schema_name;`
	out, err := runtime.Exec(ctx, engine, "exec", "-e", "MYSQL_PWD="+rootPass,
		sharedDBContainer, "mariadb", "-uroot", "-N", "-B", "-e", q)
	if err != nil {
		return nil
	}
	owners := s.sharedDBOwners()
	var dbs []SharedDB
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		if line == "" {
			continue
		}
		f := strings.Split(line, "\t")
		db := SharedDB{Name: f[0], App: owners[f[0]]}
		if len(f) > 1 {
			db.SizeMB = f[1]
		}
		dbs = append(dbs, db)
	}
	return dbs
}

// SharedTable is one table within a database (for the DB detail view).
type SharedTable struct {
	Name   string
	Rows   string // approximate for InnoDB (as information_schema reports it)
	SizeMB string
}

// SharedDBDetail is one database's full view: owner, size, and its tables.
type SharedDBDetail struct {
	Name        string
	SizeMB      string
	App         string // "project / app" owner, "" if it maps to no known app
	Host        string
	Port        int
	AdminerPort int // host port of the running Adminer helper; 0 = not running
	Tables      []SharedTable
	Found       bool // schema exists on the server
}

// SharedDatabaseDetail returns one database's owner, total size, and table list.
// Found is false (with no error) when the schema doesn't exist on the server.
func (s *Service) SharedDatabaseDetail(ctx context.Context, name string) (SharedDBDetail, error) {
	d := SharedDBDetail{Name: name, Host: sharedDBContainer, Port: 3306}
	if !validIdent(name) {
		return d, fmt.Errorf("invalid database name")
	}
	engine := s.sel.Current()
	rootPass, err := s.store.GetSetting(sharedDBRootKey)
	if err != nil || rootPass == "" {
		return d, fmt.Errorf("shared MariaDB is not configured")
	}
	if !containerRunning(ctx, engine, sharedDBContainer) {
		return d, fmt.Errorf("shared MariaDB is not running")
	}
	if p, _ := s.store.GetSetting(adminerPortKey); p != "" && containerRunning(ctx, engine, adminerContainer) {
		d.AdminerPort, _ = strconv.Atoi(p)
	}
	d.App = s.sharedDBOwners()[name]
	// name is validIdent-checked above, so interpolating it into the schema
	// filter is injection-safe (same pattern as the other admin DDL here).
	q := fmt.Sprintf(`SELECT table_name, COALESCE(table_rows,0),
			ROUND((COALESCE(data_length,0)+COALESCE(index_length,0))/1048576,2)
		FROM information_schema.tables WHERE table_schema='%s' ORDER BY table_name;`, name)
	out, err := runtime.Exec(ctx, engine, "exec", "-e", "MYSQL_PWD="+rootPass,
		sharedDBContainer, "mariadb", "-uroot", "-N", "-B", "-e",
		fmt.Sprintf("SELECT schema_name FROM information_schema.schemata WHERE schema_name='%s';\n%s", name, q))
	if err != nil {
		return d, err
	}
	var total float64
	for i, line := range strings.Split(strings.TrimSpace(out), "\n") {
		if line == "" {
			continue
		}
		if i == 0 {
			d.Found = true // first line is the schema_name existence probe
			continue
		}
		f := strings.Split(line, "\t")
		t := SharedTable{Name: f[0]}
		if len(f) > 1 {
			t.Rows = f[1]
		}
		if len(f) > 2 {
			t.SizeMB = f[2]
			if v, e := strconv.ParseFloat(f[2], 64); e == nil {
				total += v
			}
		}
		d.Tables = append(d.Tables, t)
	}
	d.SizeMB = strconv.FormatFloat(total, 'f', 1, 64)
	return d, nil
}

// sharedDBOwners maps each shared database name back to a "project / app" label
// by recomputing SharedDBName() for every shared-mode app (the reverse mapping
// is ambiguous once dashes are flattened, so we go forward instead).
func (s *Service) sharedDBOwners() map[string]string {
	owners := map[string]string{}
	projects, err := s.store.ListProjects()
	if err != nil {
		return owners
	}
	for _, p := range projects {
		apps, err := s.store.ListAppsByProject(p.ID)
		if err != nil {
			continue
		}
		for _, a := range apps {
			if a.DBMode == store.DBShared || a.IsSharedWP() {
				owners[SharedDBName(p.Slug, a.Slug)] = p.Name + " / " + a.Name
			}
		}
	}
	return owners
}

// DedicatedDB is one app's own MariaDB container (dedicated DB mode) — a
// database that lives in its own container rather than on the shared xdev-db.
type DedicatedDB struct {
	ProjectName string
	ProjectSlug string
	AppName     string
	Container   string // <project>_<app>_db (matches the compose container_name)
	DBName      string // "laravel" | "wordpress" (the schema inside the container)
	Running     bool   // follows the app's status (same compose lifecycle)
}

// DedicatedDatabases lists the per-app MariaDB containers — apps that use a
// database but opted out of the shared server. Pure store read: the DB
// container shares the app's compose lifecycle, so the app's status is its
// status; no engine calls needed.
func (s *Service) DedicatedDatabases() []DedicatedDB {
	var out []DedicatedDB
	projects, err := s.store.ListProjects()
	if err != nil {
		return out
	}
	for _, p := range projects {
		apps, err := s.store.ListAppsByProject(p.ID)
		if err != nil {
			continue
		}
		for _, a := range apps {
			if !UsesDB(a.Type) || a.DBMode == store.DBShared || a.IsSharedWP() {
				continue
			}
			dbName := "laravel"
			if a.Type == "wordpress" {
				dbName = "wordpress"
			}
			out = append(out, DedicatedDB{
				ProjectName: p.Name, ProjectSlug: p.Slug, AppName: a.Name,
				Container: p.Slug + "_" + a.Slug + "_db",
				DBName:    dbName, Running: a.Status == "running",
			})
		}
	}
	return out
}

// SharedDBRestart restarts the shared server (creating it first if it has never
// been started). Brief downtime for every shared-mode app — the blast radius.
func (s *Service) SharedDBRestart(ctx context.Context) error {
	engine := s.sel.Current()
	if !containerRunning(ctx, engine, sharedDBContainer) {
		_, err := s.ensureSharedDB(ctx, engine) // absent -> create; stopped -> start + wait
		return err
	}
	if _, err := runtime.Exec(ctx, engine, "restart", sharedDBContainer); err != nil {
		return err
	}
	_, err := s.ensureSharedDB(ctx, engine) // wait until it answers SQL again
	return err
}

// SharedDBUpdate pulls the latest shared-MariaDB image and recreates the
// container against the same named volume (data persists). Every shared-mode
// app is briefly down during the swap — the documented blast-radius ceiling.
func (s *Service) SharedDBUpdate(ctx context.Context) error {
	engine := s.sel.Current()
	rootPass, err := s.store.GetSetting(sharedDBRootKey)
	if err != nil || rootPass == "" {
		_, err := s.ensureSharedDB(ctx, engine) // never used yet -> just bring it up
		return err
	}
	if _, err := runtime.Exec(ctx, engine, "pull", sharedDBImage); err != nil {
		return err
	}
	runtime.Exec(ctx, engine, "rm", "-f", sharedDBContainer) // ignore "no such container"
	if _, err := runtime.Exec(ctx, engine, sharedDBRunArgs(rootPass, s.storedSharedDBPort())...); err != nil {
		return err
	}
	_, err = s.ensureSharedDB(ctx, engine)
	return err
}

// AdminerStart lazily runs an Adminer container on xdev_shared, pointed at the
// shared server and published on 127.0.0.1 only (root DB access — never exposed
// off-host; reach prod over an SSH tunnel). Returns the host port.
// ponytail: localhost-bound + no auth of its own; a routed, authed Adminer is
// the upgrade if remote browser access without a tunnel ever matters.
func (s *Service) AdminerStart(ctx context.Context) (int, error) {
	engine := s.sel.Current()
	if _, err := s.ensureSharedDB(ctx, engine); err != nil {
		return 0, err
	}
	portStr, _ := s.store.GetSetting(adminerPortKey)
	port, _ := strconv.Atoi(portStr)
	if port == 0 {
		var err error
		if port, err = s.allocPort(); err != nil {
			return 0, err
		}
		if err := s.store.SetSetting(adminerPortKey, strconv.Itoa(port)); err != nil {
			return 0, err
		}
	}
	// Recreate on every start (Adminer is stateless) so the current config —
	// notably the mounted stylesheet — always takes effect, even if an older
	// container is still around.
	runtime.Exec(ctx, engine, "rm", "-f", adminerContainer) // ignore "no such container"
	args := []string{"run", "-d",
		"--name", adminerContainer, "--restart", "unless-stopped",
		"--network", sharedDBNetwork,
		"-e", "ADMINER_DEFAULT_SERVER=" + sharedDBContainer}
	// Mount the same Pepa Linha dark theme the per-app (laravel) Adminers use so
	// the platform Adminer matches them, rather than the image's older built-in
	// design. A user-provided <data>/adminer.css is left in place as an override.
	if css := s.ensureAdminerCSS(); css != "" {
		args = append(args, "-v", css+":/var/www/html/adminer.css:ro")
	}
	args = append(args, "-p", fmt.Sprintf("127.0.0.1:%d:8080", port), adminerImage)
	if _, err := runtime.Exec(ctx, engine, args...); err != nil {
		return 0, err
	}
	return port, nil
}

// ensureAdminerCSS makes sure the shared Adminer stylesheet exists on disk (so
// it can be bind-mounted), seeding it from the bundled template on first use.
// A file already there (a user override) is kept. Returns "" if it can't be
// provided, so the caller just skips the mount.
func (s *Service) ensureAdminerCSS() string {
	css := s.adminerCSSPath()
	if fileExists(css) {
		return css
	}
	data, err := templates.AdminerCSS()
	if err != nil {
		return ""
	}
	if err := os.MkdirAll(filepath.Dir(css), 0o755); err != nil {
		return ""
	}
	if err := os.WriteFile(css, data, 0o644); err != nil {
		return ""
	}
	return css
}

// AdminerStop removes the Adminer helper container (the port stays reserved in
// settings so a later start reuses the same one).
func (s *Service) AdminerStop(ctx context.Context) error {
	_, err := runtime.Exec(ctx, s.sel.Current(), "rm", "-f", adminerContainer)
	return err
}

// SharedDBCount is the number of app databases xdev manages on the shared
// server (shared-mode wordpress/laravel apps) — cheap, read from the store
// without touching the engine, for the dashboard KPI.
func (s *Service) SharedDBCount() int {
	return len(s.sharedDBOwners())
}

// DBContainers names every container that holds a database: the shared server
// first, then each dedicated per-app one. The metrics collector uses this to
// pick database rows out of the engine's stats, so it is a plain store read on
// the collector's 10s tick — no engine calls. The shared container is listed
// whether or not it is running; a name that has no stats row this tick simply
// records no sample.
func (s *Service) DBContainers() []string {
	out := []string{sharedDBContainer}
	for _, d := range s.DedicatedDatabases() {
		out = append(out, d.Container)
	}
	return out
}

// --- Database & user administration (the /database CRUD controls) ---

// SharedUser is one MySQL account on the shared server.
type SharedUser struct {
	Name   string
	Host   string
	System bool // built-in account (root, mariadb.sys, …) — not user-removable
}

// systemAccounts are the built-in MySQL users the UI must not let you drop.
var systemAccounts = map[string]bool{
	"root": true, "mariadb.sys": true, "mysql": true, "healthcheck": true,
	"": true, "PUBLIC": true,
}

// SharedUsers lists every account on the shared server, flagging the built-in
// ones. Empty if the server isn't reachable.
func (s *Service) SharedUsers(ctx context.Context) []SharedUser {
	rootPass, _ := s.store.GetSetting(sharedDBRootKey)
	if rootPass == "" {
		return nil
	}
	engine := s.sel.Current()
	out, err := runtime.Exec(ctx, engine, "exec", "-e", "MYSQL_PWD="+rootPass,
		sharedDBContainer, "mariadb", "-uroot", "-N", "-B", "-e",
		"SELECT User, Host FROM mysql.user ORDER BY User, Host;")
	if err != nil {
		return nil
	}
	var users []SharedUser
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		if line == "" {
			continue
		}
		f := strings.SplitN(line, "\t", 2)
		u := SharedUser{Name: f[0], System: systemAccounts[f[0]]}
		if len(f) > 1 {
			u.Host = f[1]
		}
		users = append(users, u)
	}
	return users
}

// adminSQL runs root DDL against the shared server, bringing it up first (so
// the CRUD controls work even if it happens to be stopped).
func (s *Service) adminSQL(ctx context.Context, stmts string) error {
	engine := s.sel.Current()
	rootPass, err := s.ensureSharedDB(ctx, engine)
	if err != nil {
		return err
	}
	_, err = s.sharedSQL(ctx, engine, rootPass, stmts)
	return err
}

// CreateSharedDatabase creates an empty database (utf8mb4).
func (s *Service) CreateSharedDatabase(ctx context.Context, name string) error {
	if !validIdent(name) {
		return fmt.Errorf("invalid database name (letters, digits, underscore)")
	}
	return s.adminSQL(ctx, fmt.Sprintf(
		"CREATE DATABASE IF NOT EXISTS `%s` CHARACTER SET utf8mb4;", name))
}

// DropSharedDatabase drops a database. Refuses the server-internal ones.
func (s *Service) DropSharedDatabase(ctx context.Context, name string) error {
	if !validIdent(name) {
		return fmt.Errorf("invalid database name")
	}
	if systemAccounts[name] || name == "information_schema" || name == "performance_schema" || name == "sys" {
		return fmt.Errorf("refusing to drop the system database %q", name)
	}
	return s.adminSQL(ctx, fmt.Sprintf("DROP DATABASE IF EXISTS `%s`;", name))
}

// CreateSharedUser creates 'name'@'%' with the given password and, when grant
// is set, ALL PRIVILEGES on that database ("*" = server-wide).
func (s *Service) CreateSharedUser(ctx context.Context, name, pass, grant string) error {
	if !validIdent(name) {
		return fmt.Errorf("invalid user name (letters, digits, underscore)")
	}
	if pass == "" {
		return fmt.Errorf("password required")
	}
	stmts := fmt.Sprintf("CREATE USER IF NOT EXISTS '%s'@'%%' IDENTIFIED BY '%s';",
		name, sqlStr(pass))
	switch {
	case grant == "*":
		stmts += fmt.Sprintf("GRANT ALL PRIVILEGES ON *.* TO '%s'@'%%';", name)
	case grant != "":
		if !validIdent(grant) {
			return fmt.Errorf("invalid database to grant on")
		}
		stmts += fmt.Sprintf("GRANT ALL PRIVILEGES ON `%s`.* TO '%s'@'%%';", grant, name)
	}
	return s.adminSQL(ctx, stmts+"FLUSH PRIVILEGES;")
}

// SetSharedUserPassword resets an account's password.
func (s *Service) SetSharedUserPassword(ctx context.Context, name, host, pass string) error {
	if !validIdent(name) || !validHostPat(host) {
		return fmt.Errorf("invalid user")
	}
	if pass == "" {
		return fmt.Errorf("password required")
	}
	return s.adminSQL(ctx, fmt.Sprintf("ALTER USER '%s'@'%s' IDENTIFIED BY '%s';",
		name, host, sqlStr(pass)))
}

// DropSharedUser removes an account. Refuses the built-in ones.
func (s *Service) DropSharedUser(ctx context.Context, name, host string) error {
	if !validIdent(name) || !validHostPat(host) {
		return fmt.Errorf("invalid user")
	}
	if systemAccounts[name] {
		return fmt.Errorf("refusing to drop the system account %q", name)
	}
	return s.adminSQL(ctx, fmt.Sprintf("DROP USER IF EXISTS '%s'@'%s';", name, host))
}

// adminerCSSPath is the optional user-supplied Adminer stylesheet. When present
// it's mounted over the built-in pepa-linha-dark design (drop bizepp's exact
// file here to match it precisely). xdev never writes this file.
func (s *Service) adminerCSSPath() string {
	return filepath.Join(filepath.Dir(s.wpDir), "adminer.css")
}

func fileExists(p string) bool {
	info, err := os.Stat(p)
	return err == nil && !info.IsDir()
}

// randHex returns n random bytes as hex (crypto/rand; 2n chars).
func randHex(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		panic("xdev: crypto/rand failed: " + err.Error())
	}
	return hex.EncodeToString(b)
}

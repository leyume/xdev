package apps

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"xdev/internal/runtime"
	"xdev/internal/store"
)

// DBColumn is one column of a table, as information_schema describes it.
type DBColumn struct {
	Name, Type, Null, Key, Default, Extra string
}

// DBGrantee is an account that can reach a database: through a grant on the
// schema itself, or (Global) through a server-wide grant such as root's.
type DBGrantee struct {
	User, Host string
	Privs      []string
	Global     bool
}

// dbSQL runs one batch with -N -B (tab-separated, no headers) as root inside a
// MariaDB container. With rootPass it travels as MYSQL_PWD on the exec, as for
// the shared server; without it the container's own root password env is used,
// which is how the dedicated per-app databases are set up.
func dbSQL(ctx context.Context, engine runtime.Engine, container, rootPass, stmts string) (string, error) {
	if rootPass != "" {
		return runtime.Exec(ctx, engine, "exec", "-e", "MYSQL_PWD="+rootPass,
			container, "mariadb", "-uroot", "-N", "-B", "-e", stmts)
	}
	out, err := runtime.Exec(ctx, engine, "exec", container, "sh", "-c",
		`MYSQL_PWD="${MARIADB_ROOT_PASSWORD:-$MYSQL_ROOT_PASSWORD}" exec mariadb -uroot -N -B -e "$0"`, stmts)
	if err == nil || !strings.Contains(out+err.Error(), "not found") {
		return out, err
	}
	return dbSQLFromOutside(ctx, engine, container, stmts)
}

// dbSQLFromOutside covers older dedicated images that ship the server but no
// client: a throwaway client container (the shared server's image, normally
// already pulled) joins the database's network and connects by name. Sharing
// its network namespace instead is refused for podman-compose's pod members.
func dbSQLFromOutside(ctx context.Context, engine runtime.Engine, container, stmts string) (string, error) {
	var pass string
	for _, k := range []string{"MARIADB_ROOT_PASSWORD", "MYSQL_ROOT_PASSWORD"} {
		if v, err := runtime.Exec(ctx, engine, "exec", container, "printenv", k); err == nil {
			if pass = strings.TrimSpace(v); pass != "" {
				break
			}
		}
	}
	nets, err := runtime.Exec(ctx, engine, "inspect", container, "--format",
		`{{range $k, $v := .NetworkSettings.Networks}}{{$k}} {{end}}`)
	if err != nil || strings.TrimSpace(nets) == "" {
		return "", fmt.Errorf("no network to reach %s on", container)
	}
	// --skip-ssl: the 11.x client insists on TLS by default; these servers have none.
	return runtime.Exec(ctx, engine, "run", "--rm", "--network", strings.Fields(nets)[0],
		"-e", "MYSQL_PWD="+pass, sharedDBImage,
		"mariadb", "--skip-ssl", "-h"+container, "-uroot", "-N", "-B", "-e", stmts)
}

// describeDB fills d.Found, Tables (with their columns), Users, SizeMB and
// TotalRows for schema d.Name in one round trip. Every result row is tagged
// (S/T/C/P/G) because -N -B output gives no boundary between result sets.
// d.Name must already be validIdent-checked: it is interpolated into the SQL.
func describeDB(ctx context.Context, engine runtime.Engine, container, rootPass string, d *SharedDBDetail) error {
	n := d.Name
	q := fmt.Sprintf(`SELECT 'S', schema_name FROM information_schema.schemata WHERE schema_name='%[1]s';
SELECT 'T', table_name, COALESCE(table_rows,0),
	ROUND((COALESCE(data_length,0)+COALESCE(index_length,0))/1048576,2)
	FROM information_schema.tables WHERE table_schema='%[1]s' ORDER BY table_name;
SELECT 'C', table_name, column_name, column_type, is_nullable, column_key, COALESCE(column_default,''), extra
	FROM information_schema.columns WHERE table_schema='%[1]s' ORDER BY table_name, ordinal_position;
SELECT 'P', grantee, privilege_type FROM information_schema.schema_privileges WHERE table_schema='%[1]s';
SELECT 'G', grantee, 'ALL' FROM information_schema.user_privileges WHERE privilege_type='SELECT';`, n)
	out, err := dbSQL(ctx, engine, container, rootPass, q)
	if err != nil {
		return err
	}
	parseDescribe(out, d)
	return nil
}

// parseDescribe reads describeDB's tagged rows into d.
func parseDescribe(out string, d *SharedDBDetail) {
	tables := map[string]int{} // name -> index in d.Tables
	users := map[string]*DBGrantee{}
	var order []string
	var total float64
	for _, line := range strings.Split(out, "\n") {
		f := strings.Split(line, "\t")
		switch {
		case f[0] == "S":
			d.Found = true
		case f[0] == "T" && len(f) >= 4:
			tables[f[1]] = len(d.Tables)
			d.Tables = append(d.Tables, SharedTable{Name: f[1], Rows: f[2], SizeMB: f[3]})
			if v, e := strconv.ParseFloat(f[3], 64); e == nil {
				total += v
			}
			if v, e := strconv.ParseInt(f[2], 10, 64); e == nil {
				d.TotalRows += v
			}
		case f[0] == "C" && len(f) >= 8:
			if i, ok := tables[f[1]]; ok {
				d.Tables[i].Columns = append(d.Tables[i].Columns,
					DBColumn{Name: f[2], Type: f[3], Null: f[4], Key: f[5], Default: f[6], Extra: f[7]})
			}
		case (f[0] == "P" || f[0] == "G") && len(f) >= 3:
			user, host, _ := strings.Cut(strings.Trim(f[1], "'"), "'@'")
			key := f[1]
			if f[0] == "G" {
				key = "G " + user // root@localhost, root@::1, root@% read as one root
			}
			g, ok := users[key]
			if !ok {
				g = &DBGrantee{User: user, Host: host, Global: f[0] == "G"}
				users[key] = g
				order = append(order, key)
			} else if g.Global {
				g.Host += ", " + host
				continue
			}
			g.Privs = append(g.Privs, f[2])
		}
	}
	for _, k := range order {
		g := users[k]
		// ponytail: a schema grant of ~20 privileges is GRANT ALL in practice;
		// list them only when it is a narrower set.
		if len(g.Privs) >= 17 {
			g.Privs = []string{"ALL"}
		}
		sort.Strings(g.Privs)
		d.Users = append(d.Users, *g)
	}
	// Schema-specific accounts first: they are the ones that belong to this DB.
	sort.SliceStable(d.Users, func(i, j int) bool { return !d.Users[i].Global && d.Users[j].Global })
	d.SizeMB = strconv.FormatFloat(total, 'f', 1, 64)
}

// DedicatedDatabaseDetail describes the database inside an app's own MariaDB
// container (an app that opted out of the shared server).
func (s *Service) DedicatedDatabaseDetail(ctx context.Context, appID int64) (SharedDBDetail, error) {
	var d SharedDBDetail
	app, proj, err := s.dedicatedApp(appID)
	if err != nil {
		return d, err
	}
	d = SharedDBDetail{
		Name: dedicatedDBName(app), App: proj.Name + " / " + app.Name, Port: 3306,
		Dedicated: true, Container: proj.Slug + "_" + app.Slug + "_db",
		ProjectSlug: proj.Slug, AppID: app.ID,
	}
	d.Host = d.Container
	engine := s.dedicatedEngine(app)
	d.Env = dbContainerEnv(ctx, engine, d.Container)
	if !containerRunning(ctx, engine, d.Container) {
		return d, fmt.Errorf("%s's database isn't running — start the app first", d.App)
	}
	return d, describeDB(ctx, engine, d.Container, "", &d)
}

// dedicatedApp loads an app that runs its own database container.
func (s *Service) dedicatedApp(appID int64) (store.App, store.Project, error) {
	app, err := s.store.AppByID(appID)
	if err != nil {
		return app, store.Project{}, fmt.Errorf("no such app")
	}
	if !UsesDB(app.Type) || app.DBMode == store.DBShared || app.IsSharedWP() {
		return app, store.Project{}, fmt.Errorf("%s has no dedicated database", app.Name)
	}
	proj, err := s.store.ProjectByID(app.ProjectID)
	if err != nil {
		return app, proj, fmt.Errorf("no such project")
	}
	return app, proj, nil
}

// dedicatedEngine is the engine the app's stack runs on, as composeCtx picks it.
func (s *Service) dedicatedEngine(app store.App) runtime.Engine {
	if e := runtime.Engine(app.Runtime); e != "" || s.sel == nil {
		return e
	}
	return s.sel.Current()
}

// dedicatedDBName is the schema the app's compose template creates.
func dedicatedDBName(a store.App) string {
	if a.Type == "wordpress" {
		return "wordpress"
	}
	return "laravel"
}

// DBEnvVar is one connection setting from a dedicated database container's
// environment, as the app's compose file passed it in.
type DBEnvVar struct {
	Key, Value string
	Secret     bool
}

// dbEnvOrder lists the settings worth showing, in reading order; the image's own
// (MARIADB_VERSION and the like) are left out.
var dbEnvOrder = []string{"DATABASE", "USER", "PASSWORD", "ROOT_PASSWORD"}

// dbContainerEnv reads the MARIADB_*/MYSQL_* credentials a database container
// was created with. It works on a stopped container too; nil when it can't.
func dbContainerEnv(ctx context.Context, engine runtime.Engine, container string) []DBEnvVar {
	out, err := runtime.Exec(ctx, engine, "container", "inspect", "--format",
		`{{range .Config.Env}}{{println .}}{{end}}`, container)
	if err != nil {
		return nil
	}
	return parseDBEnv(out)
}

func parseDBEnv(out string) []DBEnvVar {
	got := map[string]string{}
	for _, line := range strings.Split(out, "\n") {
		if k, v, ok := strings.Cut(strings.TrimSpace(line), "="); ok {
			got[k] = v
		}
	}
	var env []DBEnvVar
	for _, suffix := range dbEnvOrder {
		for _, prefix := range []string{"MARIADB_", "MYSQL_"} {
			if v, ok := got[prefix+suffix]; ok {
				env = append(env, DBEnvVar{Key: prefix + suffix, Value: v, Secret: strings.HasSuffix(suffix, "PASSWORD")})
			}
		}
	}
	return env
}

// dbPassRE is what a dedicated database password may hold: enough for a strong
// one, and nothing that needs quoting in YAML, a dotenv file or SQL, or that
// compose would read as ${interpolation}.
var dbPassRE = regexp.MustCompile(`^[A-Za-z0-9._~@%^*+=!-]{8,128}$`)

// The compose keys each password travels under — the db service's own and the
// ones the app reads it from. Laravel's fpm/prod stacks spell the user one as
// ${DB_PASSWORD:-secret}; a literal replaces that.
var (
	dbUserPassKeys = []string{"MYSQL_PASSWORD", "MARIADB_PASSWORD", "DB_PASSWORD", "WORDPRESS_DB_PASSWORD"}
	dbRootPassKeys = []string{"MYSQL_ROOT_PASSWORD", "MARIADB_ROOT_PASSWORD"}
)

// SetDedicatedDBPassword changes the app user's password (or root's) in an app's
// own MariaDB, writes it into the compose file (and _/laravel.env) the app
// connects with, and recreates the stack so both sides hold the new value. The
// database must be running: the change is made through it.
func (s *Service) SetDedicatedDBPassword(ctx context.Context, appID int64, root bool, pass string) error {
	if !dbPassRE.MatchString(pass) {
		return fmt.Errorf("use 8–128 characters: letters, digits and . _ ~ @ %% ^ * + = ! -")
	}
	app, proj, err := s.dedicatedApp(appID)
	if err != nil {
		return err
	}
	container := proj.Slug + "_" + app.Slug + "_db"
	engine := s.dedicatedEngine(app)
	if !containerRunning(ctx, engine, container) {
		return fmt.Errorf("%s's database isn't running — start the app first", app.Name)
	}
	user, keys := "root", dbRootPassKeys
	if !root {
		keys = dbUserPassKeys
		user = ""
		for _, e := range dbContainerEnv(ctx, engine, container) {
			if e.Key == "MYSQL_USER" || e.Key == "MARIADB_USER" {
				user = e.Value
			}
		}
		if !validIdent(user) {
			return fmt.Errorf("can't tell which user %s's database gives the app", app.Name)
		}
	}
	hosts, err := dbSQL(ctx, engine, container, "", fmt.Sprintf("SELECT host FROM mysql.user WHERE user='%s';", user))
	if err != nil {
		return fmt.Errorf("read %s's accounts: %w", user, err)
	}
	var stmts string
	for _, h := range strings.Fields(hosts) {
		stmts += fmt.Sprintf("ALTER USER '%s'@'%s' IDENTIFIED BY '%s';", user, sqlStr(h), pass)
	}
	if stmts == "" {
		return fmt.Errorf("no %s account in %s's database", user, app.Name)
	}
	// Files first, database last: a failed write leaves everything as it was,
	// and once the ALTER lands the files already say what the app needs.
	b, err := os.ReadFile(app.ComposePath)
	if err != nil {
		return err
	}
	if err := writeComposeEdit(app.ComposePath, setComposeKeys(string(b), keys, pass)); err != nil {
		return err
	}
	dotenv := filepath.Join(filepath.Dir(app.ComposePath), "laravel.env")
	prevEnv, envErr := os.ReadFile(dotenv)
	if !root && envErr == nil {
		re := regexp.MustCompile(`(?m)^DB_PASSWORD=.*$`)
		if err := os.WriteFile(dotenv, re.ReplaceAll(prevEnv, []byte("DB_PASSWORD="+pass)), 0o600); err != nil {
			return err
		}
	}
	if _, err := dbSQL(ctx, engine, container, "", stmts+"FLUSH PRIVILEGES;"); err != nil {
		// The database kept the old password, so the files go back to it.
		os.Rename(app.ComposePath+".bak", app.ComposePath)
		if !root && envErr == nil {
			os.WriteFile(dotenv, prevEnv, 0o600)
		}
		return fmt.Errorf("change the password: %w", err)
	}
	s.store.AddEvent(app.ProjectID, app.ID, "info", "Changed the "+user+" password of "+app.Name+"'s database")
	// Recreate, not restart: a container keeps the environment it was created with.
	if _, eng, workdir, pname, file, err := s.composeCtx(app.ID); err == nil {
		runtime.Down(ctx, eng, workdir, pname, file)
	}
	return s.Start(app.ID)
}

// setComposeKeys sets every `KEY: value` environment entry named in keys to v.
func setComposeKeys(yaml string, keys []string, v string) string {
	re := regexp.MustCompile(`(?m)^([ \t]+)(` + strings.Join(keys, "|") + `):[ \t]*.*$`)
	return re.ReplaceAllString(yaml, "${1}${2}: \""+v+"\"")
}

// carryDBPasswords copies the dedicated database passwords set in prev (an
// app's current compose file) into next (its regenerated one), so regenerating
// a stack never reverts a changed password and locks the app out.
func carryDBPasswords(prev, next string) string {
	for _, k := range append(append([]string{}, dbUserPassKeys...), dbRootPassKeys...) {
		m := regexp.MustCompile(`(?m)^[ \t]+` + k + `:[ \t]*"?([^"\n]*)"?[ \t]*$`).FindStringSubmatch(prev)
		if m != nil && dbPassRE.MatchString(m[1]) {
			next = setComposeKeys(next, []string{k}, m[1])
		}
	}
	return next
}

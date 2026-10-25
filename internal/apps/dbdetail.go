package apps

import (
	"context"
	"fmt"
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
	app, err := s.store.AppByID(appID)
	if err != nil {
		return d, fmt.Errorf("no such app")
	}
	if !UsesDB(app.Type) || app.DBMode == store.DBShared || app.IsSharedWP() {
		return d, fmt.Errorf("%s has no dedicated database", app.Name)
	}
	proj, err := s.store.ProjectByID(app.ProjectID)
	if err != nil {
		return d, fmt.Errorf("no such project")
	}
	d = SharedDBDetail{
		Name: dedicatedDBName(app), App: proj.Name + " / " + app.Name, Port: 3306,
		Dedicated: true, Container: proj.Slug + "_" + app.Slug + "_db",
		ProjectSlug: proj.Slug, AppID: app.ID,
	}
	d.Host = d.Container
	engine := s.sel.Current()
	d.Env = dbContainerEnv(ctx, engine, d.Container)
	if !containerRunning(ctx, engine, d.Container) {
		return d, fmt.Errorf("%s's database isn't running — start the app first", d.App)
	}
	return d, describeDB(ctx, engine, d.Container, "", &d)
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

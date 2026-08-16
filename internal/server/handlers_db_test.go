package server

import (
	"bytes"
	"html/template"
	"strings"
	"testing"

	"xdev/internal/apps"
	"xdev/internal/metrics"
	"xdev/internal/runtime"
	"xdev/internal/store"
	"xdev/web"
)

// renderPage builds a page template set exactly like server.New does and
// executes the full layout, so field-reference and template-syntax errors on the
// page surface as a test failure (there's no live render otherwise).
func renderPage(t *testing.T, data viewData) string {
	t.Helper()
	return renderNamed(t, "database", data)
}

func renderNamed(t *testing.T, page string, data viewData) string {
	t.Helper()
	tmpl, err := template.New(page).Funcs(tmplFuncs()).ParseFS(web.TemplatesFS,
		"templates/layout.html", "templates/partials.html", "templates/twstyles.html", "templates/"+page+".html")
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	data["User"] = store.User{ID: 1, Email: "a@b.co"}
	data["CSRF"] = "tok"
	data["NavLayout"] = "topbar"
	data["Path"] = "/database"
	var buf bytes.Buffer
	if err := tmpl.ExecuteTemplate(&buf, "layout", data); err != nil {
		t.Fatalf("execute: %v", err)
	}
	return buf.String()
}

func TestDatabasePageRenders(t *testing.T) {
	// Not-configured: shows the "start" call to action, no databases table.
	out := renderPage(t, viewData{"DB": apps.SharedDBInfo{Configured: false}})
	if !strings.Contains(out, "not started") || !strings.Contains(out, "/database/restart") {
		t.Errorf("unconfigured page missing start CTA:\n%s", out)
	}

	// Configured + running with one owned database and Adminer up.
	info := apps.SharedDBInfo{
		Configured: true, Running: true, Container: "xdev-db",
		Image: "mariadb:11", Network: "xdev_shared", Host: "xdev-db", Port: 3306,
		RootPass:    "s3cret",
		AdminerPort: 34567,
		Databases:   []apps.SharedDB{{Name: "demo_api", SizeMB: "1.5", App: "Demo / api"}},
	}
	out = renderPage(t, viewData{
		"DB":    info,
		"Msg":   "done",
		"Users": []apps.SharedUser{{Name: "demo_api", Host: "%"}, {Name: "root", Host: "localhost", System: true}},
		"Dedicated": []apps.DedicatedDB{{
			ProjectName: "Bizepp", ProjectSlug: "bizepp", AppName: "API",
			Container: "bizepp_api_db", DBName: "laravel", Running: true,
		}},
	})
	for _, want := range []string{"demo_api", "Demo / api", "1.5", "s3cret",
		"127.0.0.1:34567", "/database/update", `value="stop"`, "/database/demo_api",
		"/database/db/create", "/database/user/create", ">system<",
		"bizepp_api_db", "/projects/bizepp", "Dedicated"} {
		if !strings.Contains(out, want) {
			t.Errorf("running page missing %q", want)
		}
	}
}

func TestDatabaseDetailRenders(t *testing.T) {
	d := apps.SharedDBDetail{
		Name: "demo_api", SizeMB: "1.5", App: "Demo / api",
		Host: "xdev-db", Port: 3306, AdminerPort: 34567, Found: true,
		Tables: []apps.SharedTable{{Name: "wp_users", Rows: "42", SizeMB: "0.30"}},
	}
	out := renderNamed(t, "database_detail", viewData{"D": d})
	for _, want := range []string{"demo_api", "Demo / api", "1.5", "wp_users", "42",
		"db=demo_api", "/database/db/drop", "Overview", "Tables"} {
		if !strings.Contains(out, want) {
			t.Errorf("db detail page missing %q", want)
		}
	}
}

// TestAllPagesParse mirrors server.New's parse loop so a template syntax error
// on any page (not just the ones we fully render) fails a test.
func TestAllPagesParse(t *testing.T) {
	pages := []string{"setup", "login", "dashboard", "projects", "project_new",
		"project", "app_metrics", "app_logs", "app_env", "app_backups", "app_settings",
		"events", "admins", "database", "database_detail", "wordpress"}
	for _, p := range pages {
		if _, err := template.New(p).Funcs(tmplFuncs()).ParseFS(web.TemplatesFS,
			"templates/layout.html", "templates/partials.html", "templates/twstyles.html", "templates/"+p+".html"); err != nil {
			t.Errorf("parse %s: %v", p, err)
		}
	}
}

func TestWordPressPageRenders(t *testing.T) {
	out := renderNamed(t, "wordpress", viewData{
		"Plugins": []apps.WPPoolItem{{Name: "akismet", Kind: "plugins"}},
		"Themes":  nil,
	})
	for _, want := range []string{"akismet", "/wordpress/pool/upload", "/wordpress/pool/remove",
		`name="kind" value="plugins"`, `name="kind" value="themes"`} {
		if !strings.Contains(out, want) {
			t.Errorf("wordpress page missing %q", want)
		}
	}
}

// The vitals strip is the point of the DB page's detail work: container cost
// from the collector, MariaDB's own counters beside it.
func TestDatabasePageShowsServerVitals(t *testing.T) {
	out := renderPage(t, viewData{
		"DB": apps.SharedDBInfo{
			Configured: true, Running: true, Container: "xdev-db",
			Host: "xdev-db", Port: 3306,
			Databases: []apps.SharedDB{{Name: "demo_api", SizeMB: "1.5"}},
		},
		"Usage": map[string]store.DBMetric{
			"xdev-db": {Container: "xdev-db", TS: "2026-08-16T04:00:00Z", CPUPct: 2.5, MemBytes: 314572800},
		},
		"Stats": apps.SharedDBStats{
			Available: true, Version: "11.4.2", Uptime: "3d 4h",
			Threads: "7", Running: "1", MaxUsed: "12", MaxConns: "151",
			Questions: "84210", QPS: "0.3", SlowLog: "2", Aborted: "0",
			PoolSize: "128.0 MiB", PoolUsed: "41.2 MiB", PoolPct: "32",
			TableCount: "38",
		},
	})
	for _, want := range []string{
		"300 ", "MB", // 314572800 bytes rendered through mib
		"2.5",                   // CPU percent
		"3d 4h",                 // uptime
		"7",                     // connections
		"peak 12 of 151",        // high-water mark against the ceiling
		"0.3",                   // queries/sec
		"41.2 MiB of 128.0 MiB", // buffer pool
		"38",                    // table count
		"MariaDB 11.4.2",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("vitals strip missing %q", want)
		}
	}
	// The averaged figure must say so; a bare "queries/sec" would read as live.
	if !strings.Contains(out, "avg, not current") {
		t.Error("queries/sec is not labelled as an average since start")
	}
}

// A running server that will not answer a status query still has to render:
// the container figures come from the collector, not from MariaDB.
func TestDatabasePageRendersWithoutServerCounters(t *testing.T) {
	out := renderPage(t, viewData{
		"DB": apps.SharedDBInfo{Configured: true, Running: true, Container: "xdev-db"},
		"Usage": map[string]store.DBMetric{
			"xdev-db": {Container: "xdev-db", TS: "2026-08-16T04:00:00Z", CPUPct: 1, MemBytes: 104857600},
		},
		"Stats": apps.SharedDBStats{}, // Available=false
	})
	if !strings.Contains(out, "counters unavailable") {
		t.Error("no explanation shown when the server did not answer a status query")
	}
	if !strings.Contains(out, "100 ") {
		t.Error("container memory should still render when MariaDB counters are missing")
	}
}

// Nothing on this page may require Usage to be populated: the collector has not
// ticked when xdev has just started, and `index` on a nil map is a hard error.
func TestDatabasePageRendersWithNoUsageData(t *testing.T) {
	out := renderPage(t, viewData{
		"DB": apps.SharedDBInfo{
			Configured: true, Running: true, Container: "xdev-db",
		},
		"Dedicated": []apps.DedicatedDB{{
			ProjectName: "Bizepp", ProjectSlug: "bizepp", AppName: "API",
			Container: "bizepp_api_db", DBName: "laravel", Running: true,
		}},
	})
	if !strings.Contains(out, "xdev-db") {
		t.Error("page did not render without usage data")
	}
}

// The Databases KPI gains what those databases actually cost, so the dashboard
// answers "how much is my data layer" without a click through.
func TestDashboardKPIShowsDatabaseUsage(t *testing.T) {
	base := viewData{
		"Projects": []store.Project{}, "Apps": []appRow{}, "Events": []store.Event{},
		"Runtime": runtime.Info{}, "Engine": "docker", "Host": metrics.HostSnapshot(),
		"TotalApps": 0, "Running": 0, "Stopped": 0,
		"SharedDBs": 2, "DedicatedDBs": 1, "TotalDBs": 3,
	}

	base["DBMemMiB"], base["DBLive"] = int64(412), 2
	out := renderNamed(t, "dashboard", base)
	if !strings.Contains(out, "412 MB across 2 containers") {
		t.Errorf("dashboard KPI missing database usage:\n%s", out)
	}

	// One container must not read "1 containers".
	base["DBMemMiB"], base["DBLive"] = int64(300), 1
	out = renderNamed(t, "dashboard", base)
	if !strings.Contains(out, "300 MB across 1 container<") {
		t.Error("dashboard KPI pluralised a single container")
	}

	// No samples yet: the line disappears rather than claiming 0 MB.
	base["DBMemMiB"], base["DBLive"] = int64(0), 0
	out = renderNamed(t, "dashboard", base)
	if strings.Contains(out, "MB across") {
		t.Error("dashboard KPI showed a usage line with no samples")
	}
}

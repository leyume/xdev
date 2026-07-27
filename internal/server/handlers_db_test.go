package server

import (
	"bytes"
	"html/template"
	"strings"
	"testing"

	"xdev/internal/apps"
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
		"project", "app_metrics", "app_logs", "app_env", "app_backups", "events",
		"admins", "database", "database_detail", "wordpress"}
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

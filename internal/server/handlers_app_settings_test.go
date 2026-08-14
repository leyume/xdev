package server

import (
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"xdev/internal/store"
)

// renderSettings renders the settings page for one app, with the view data the
// handler would build.
func renderSettings(t *testing.T, app store.App, form appSettingsForm, extra viewData) string {
	t.Helper()
	data := viewData{
		"Title":        "settings",
		"App":          app,
		"Project":      store.Project{Name: "Demo", Slug: "demo", BaseDomain: "demo.test"},
		"Form":         form,
		"SlotPorts":    []int{},
		"ServicePort":  0,
		"ServiceLabel": serviceLabel(app.Type),
		"MaxDomains":   maxComposeDomains,
		"HasCompose":   app.ComposePath != "",
		"ComposeName":  composeFileLabel(app),
		"EnvPath":      "_/.env",
	}
	for k, v := range extra {
		data[k] = v
	}
	return renderNamed(t, "app_settings", data)
}

// TestSettingsPageIsPerType checks that each type is offered the fields it
// actually has — and not the ones it doesn't, which is the part that would
// quietly save nonsense.
func TestSettingsPageIsPerType(t *testing.T) {
	compose := store.App{
		ID: 1, Name: "byo", Slug: "byo", Type: store.TypeCompose, Status: store.AppRunning,
		Domain: "byo.demo.test", Port: 20001, ComposePath: "/p/demo/byo/_/compose.yml",
	}
	out := renderSettings(t, compose, appSettingsForm{
		Name: "byo", Domain: "byo.demo.test", ExtraDomains: []string{"api.demo.test"},
		Compose: "services:\n  web:\n    ports:\n      - \"${PORT}:80\"\n",
	}, viewData{"SlotPorts": []int{20002}})
	for _, want := range []string{
		`action="/apps/1/settings"`,
		`name="name"`, `name="domain"`, `name="compose_yaml"`,
		// The rows are seeded from Go as JSON. Quotes come out as character
		// references because this is an HTML attribute; the browser decodes them
		// before Alpine parses the expression, so this is the JS it evaluates.
		`appSettings([&#34;api.demo.test&#34;], [20002], 5)`,
		"${PORT_2}", "Add a domain", "compose.yml.bak",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("compose settings page missing %q", want)
		}
	}
	if strings.Contains(out, `name="upstream"`) || strings.Contains(out, `name="serve_mode"`) {
		t.Error("compose settings page offers fields from another type")
	}

	// A proxy app: an upstream, and nothing that implies files or a stack.
	proxy := store.App{ID: 2, Name: "api", Slug: "api", Type: store.TypeProxy,
		Domain: "api.demo.test", Upstream: "http://10.0.0.5:3000"}
	out = renderSettings(t, proxy, appSettingsForm{Name: "api", Domain: "api.demo.test", Upstream: "http://10.0.0.5:3000"}, nil)
	if !strings.Contains(out, `name="upstream"`) || !strings.Contains(out, "http://10.0.0.5:3000") {
		t.Error("proxy settings page is missing its upstream field")
	}
	for _, unwanted := range []string{`name="compose_yaml"`, `name="extra_domain"`, `name="serve_mode"`} {
		if strings.Contains(out, unwanted) {
			t.Errorf("proxy settings page offers %q", unwanted)
		}
	}

	// A static app: serve mode and the commands, no compose editor.
	static := store.App{ID: 3, Name: "site", Slug: "site", Type: store.TypeStatic,
		Domain: "site.demo.test", ServeMode: store.ServeCommand, StartCmd: "npm run dev", Port: 20005}
	out = renderSettings(t, static, appSettingsForm{
		Name: "site", Domain: "site.demo.test", ServeMode: store.ServeCommand, StartCmd: "npm run dev",
	}, nil)
	for _, want := range []string{`name="serve_mode"`, `name="build_cmd"`, `name="start_cmd"`, "npm run dev"} {
		if !strings.Contains(out, want) {
			t.Errorf("static settings page missing %q", want)
		}
	}
	if strings.Contains(out, `name="compose_yaml"`) {
		t.Error("static settings page offers a compose editor")
	}

	// A host app pointed at a folder of the user's own shows that path back, so
	// saving the page doesn't silently move the app to the managed default.
	static.SourceDir = "/home/li/ui/xyz"
	out = renderSettings(t, static, appSettingsForm{
		Name: "site", Domain: "site.demo.test", ServeMode: store.ServeCommand, SourceDir: static.SourceDir,
	}, nil)
	if !strings.Contains(out, `name="source_dir"`) || !strings.Contains(out, "/home/li/ui/xyz") {
		t.Error("static settings page does not show the app's folder")
	}

	// Laravel: the bundled Adminer hostname is editable, and named.
	laravel := store.App{ID: 4, Name: "app", Slug: "app", Type: "laravel",
		Domain: "app.demo.test", ComposePath: "/p/demo/app/_/compose.yml"}
	out = renderSettings(t, laravel, appSettingsForm{
		Name: "app", Domain: "app.demo.test", ServiceDomain: "adminer.app.demo.test",
	}, viewData{"ServicePort": 20009})
	for _, want := range []string{`name="service_domain"`, "adminer.app.demo.test", "Adminer", "port 20009"} {
		if !strings.Contains(out, want) {
			t.Errorf("laravel settings page missing %q", want)
		}
	}
}

// TestSettingsPageShowsTheDatabase: a db-backed app's database is shown on its
// settings page in *both* modes — "shared" is a choice that was made, not the
// absence of one — along with the Adminer that opens it where there is one.
func TestSettingsPageShowsTheDatabase(t *testing.T) {
	proj := store.Project{Name: "Demo", Slug: "demo", BaseDomain: "demo.test"}

	// Laravel on the shared server: the database it was given there, and its
	// bundled Adminer pointed at that server.
	laravel := store.App{ID: 1, Name: "app", Slug: "app", Type: "laravel", Domain: "app.demo.test",
		DBMode: store.DBShared, ComposePath: "/p/demo/app/_/compose.yml"}
	svc := []store.ServiceDomain{{Host: "adminer.app.demo.test", Port: 20009}}
	out := renderSettings(t, laravel, appSettingsForm{Name: "app", Domain: "app.demo.test",
		ServiceDomain: "adminer.app.demo.test"}, viewData{
		"ServicePort": 20009, "DB": appDB(laravel, proj, svc)})
	for _, want := range []string{"Database", "Shared", "demo_app", "xdev-db:3306",
		"/database/demo_app", "adminer.app.demo.test", "port 20009", "shared xdev-db server"} {
		if !strings.Contains(out, want) {
			t.Errorf("laravel/shared settings page missing %q", want)
		}
	}

	// The same app on its own MariaDB: the names its stack actually uses, no
	// link to the shared server's page, and the Adminer following it.
	laravel.DBMode = ""
	out = renderSettings(t, laravel, appSettingsForm{Name: "app", Domain: "app.demo.test"},
		viewData{"ServicePort": 20009, "DB": appDB(laravel, proj, svc)})
	for _, want := range []string{"Dedicated", "laravel", "db:3306", "this app&#39;s own db container"} {
		if !strings.Contains(out, want) {
			t.Errorf("laravel/dedicated settings page missing %q", want)
		}
	}
	if strings.Contains(out, "/database/demo_app") {
		t.Error("a dedicated database was linked to the shared server's page")
	}

	// WordPress on the shared host: shared database, and no Adminer of its own.
	wp := store.App{ID: 2, Name: "blog", Slug: "blog", Type: "wordpress", Domain: "blog.demo.test",
		DBMode: store.DBShared, WPMode: store.WPShared}
	out = renderSettings(t, wp, appSettingsForm{Name: "blog", Domain: "blog.demo.test"},
		viewData{"DB": appDB(wp, proj, nil)})
	for _, want := range []string{"Shared", "demo_blog", "no Adminer bundled"} {
		if !strings.Contains(out, want) {
			t.Errorf("wordpress settings page missing %q", want)
		}
	}

	// A type with no database says nothing about one.
	compose := store.App{ID: 3, Name: "byo", Slug: "byo", Type: store.TypeCompose, Domain: "byo.demo.test"}
	out = renderSettings(t, compose, appSettingsForm{Name: "byo", Domain: "byo.demo.test"},
		viewData{"DB": appDB(compose, proj, nil)})
	if strings.Contains(out, "<h3>Database</h3>") {
		t.Error("a compose app was given a database section")
	}
}

// TestAppDB covers the facts behind that section without the HTML.
func TestAppDB(t *testing.T) {
	proj := store.Project{Slug: "my-proj"}
	shared := appDB(store.App{Type: "laravel", Slug: "my-app", DBMode: store.DBShared}, proj, nil)
	if shared == nil || !shared.Shared {
		t.Fatalf("laravel/shared = %+v", shared)
	}
	// Dashes are flattened: the name has to be a bare MySQL identifier.
	if shared.Name != "my_proj_my_app" || shared.User != shared.Name {
		t.Errorf("shared name/user = %q/%q", shared.Name, shared.User)
	}

	// Shared-host WordPress is always on the shared server, whatever db_mode says.
	wp := appDB(store.App{Type: "wordpress", Slug: "blog", WPMode: store.WPShared}, proj, nil)
	if wp == nil || !wp.Shared {
		t.Errorf("shared-host wordpress = %+v", wp)
	}

	if got := appDB(store.App{Type: store.TypeStatic}, proj, nil); got != nil {
		t.Errorf("a static app reported a database: %+v", got)
	}
}

// TestSettingsPageKeepsRejectedInput: a save that fails re-renders what was
// typed, not what is stored — retyping a hand-edited compose file because a
// hostname was taken is how people lose work.
func TestSettingsPageKeepsRejectedInput(t *testing.T) {
	app := store.App{ID: 1, Name: "byo", Slug: "byo", Type: store.TypeCompose,
		Domain: "byo.demo.test", ComposePath: "/p/demo/byo/_/compose.yml"}
	typed := "services:\n  web:\n    image: caddy:2 # my edit\n    ports:\n      - \"${PORT}:80\"\n"
	out := renderSettings(t, app, appSettingsForm{
		Name: "renamed", Domain: "taken.demo.test", Compose: typed,
	}, viewData{"Error": `domain "taken.demo.test" is already in use`})

	for _, want := range []string{"already in use", "renamed", "taken.demo.test", "caddy:2 # my edit"} {
		if !strings.Contains(out, want) {
			t.Errorf("rejected settings page lost %q", want)
		}
	}
}

// TestSettingsFormFrom covers the parse of a submitted form, including the
// compose rows whose order is their ${PORT_n} slot.
func TestSettingsFormFrom(t *testing.T) {
	form := url.Values{}
	form.Set("name", "  My App  ")
	form.Set("domain", " App.Demo.Test ")
	form.Set("compose_yaml", "services:\r\n  web:\r\n")
	form.Set("source_dir", "/home/li/ui/xyz")
	form.Add("extra_domain", "api.demo.test")
	form.Add("extra_domain", "admin.demo.test")

	r := httptest.NewRequest("POST", "/apps/1/settings", strings.NewReader(form.Encode()))
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	if err := r.ParseForm(); err != nil {
		t.Fatalf("ParseForm: %v", err)
	}

	got := settingsFormFrom(r)
	if got.Name != "My App" {
		t.Errorf("name = %q, want trimmed", got.Name)
	}
	if got.Domain != "App.Demo.Test" {
		t.Errorf("domain = %q, want trimmed (the apps service lowercases it)", got.Domain)
	}
	if len(got.ExtraDomains) != 2 || got.ExtraDomains[0] != "api.demo.test" || got.ExtraDomains[1] != "admin.demo.test" {
		t.Errorf("extra domains = %q, want them in slot order", got.ExtraDomains)
	}
	if !strings.Contains(got.Compose, "services:") {
		t.Errorf("compose body lost: %q", got.Compose)
	}
	if got.SourceDir != "/home/li/ui/xyz" {
		t.Errorf("source dir = %q, want the submitted path", got.SourceDir)
	}
}

// TestServiceLabel: only the types that bundle a second web UI name one.
func TestServiceLabel(t *testing.T) {
	for appType, want := range map[string]string{
		"laravel":         "Adminer",
		"mail":            "Mail admin",
		store.TypeCompose: "",
		store.TypeStatic:  "",
	} {
		if got := serviceLabel(appType); got != want {
			t.Errorf("serviceLabel(%q) = %q, want %q", appType, got, want)
		}
	}
}

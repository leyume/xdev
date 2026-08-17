// Package server wires up the HTTP layer: routing, the embedded html/template
// pages, static assets, and the Phase 0 handlers (first-run setup, login,
// logout, dashboard). It deliberately uses the standard library's net/http and
// ServeMux (Go 1.22+ method+path patterns) — no framework — to stay readable.
package server

import (
	"bytes"
	"html/template"
	"io/fs"
	"log"
	"net/http"

	"xdev/internal/apps"
	"xdev/internal/auth"
	"xdev/internal/config"
	"xdev/internal/platform"
	"xdev/internal/projects"
	"xdev/internal/runtime"
	"xdev/internal/store"
	"xdev/web"
)

// Server holds dependencies shared across handlers.
type Server struct {
	store      *store.Store
	auth       *auth.Service
	engine     *runtime.Selector
	cfg        config.Config
	projects   *projects.Service
	apps       *apps.Service
	reconciler *platform.Reconciler
	httpsPort  int                           // public HTTPS port, for building site URLs
	tmpl       map[string]*template.Template // page name -> parsed template set
	mux        *http.ServeMux
	// jobs tracks long creates so the add-app dialog can show their progress
	// instead of a spinner. In memory only — see jobs.go.
	jobs jobs
}

// New builds the server, parses templates, and registers routes.
func New(st *store.Store, authsvc *auth.Service, engine *runtime.Selector, cfg config.Config, projSvc *projects.Service, appSvc *apps.Service, recon *platform.Reconciler, httpsPort int) (*Server, error) {
	s := &Server{store: st, auth: authsvc, engine: engine, cfg: cfg, projects: projSvc, apps: appSvc, reconciler: recon, httpsPort: httpsPort}
	if err := s.parseTemplates(); err != nil {
		return nil, err
	}
	s.routes()
	return s, nil
}

// Handler returns the root HTTP handler.
func (s *Server) Handler() http.Handler { return s.mux }

// parseTemplates builds one template set per page, each combined with the
// shared layout so {{block "content"}} resolves per page.
func (s *Server) parseTemplates() error {
	pages := []string{"setup", "login", "dashboard", "projects", "project_new", "project", "app_metrics",
		"app_logs", "app_env", "app_backups", "app_settings", "events", "admins", "database", "database_detail",
		"wordpress", "settings_env"}
	s.tmpl = make(map[string]*template.Template, len(pages))
	for _, p := range pages {
		t, err := template.New(p).Funcs(tmplFuncs()).ParseFS(web.TemplatesFS,
			"templates/layout.html", "templates/partials.html", "templates/twstyles.html", "templates/"+p+".html")
		if err != nil {
			return err
		}
		s.tmpl[p] = t
	}
	return nil
}

// routes registers all URL patterns.
func (s *Server) routes() {
	mux := http.NewServeMux()

	// Static assets straight from the embedded FS.
	staticFS, _ := fs.Sub(web.StaticFS, "static")
	mux.Handle("GET /static/", http.StripPrefix("/static/", http.FileServer(http.FS(staticFS))))

	// First-run admin setup.
	mux.HandleFunc("GET /setup", s.handleSetupForm)
	mux.HandleFunc("POST /setup", s.handleSetupSubmit)

	// Auth.
	mux.HandleFunc("GET /login", s.handleLoginForm)
	mux.HandleFunc("POST /login", s.handleLoginSubmit)
	mux.HandleFunc("POST /logout", s.auth.RequireAuth(s.handleLogout))

	// Dashboard (protected).
	mux.HandleFunc("GET /{$}", s.auth.RequireAuth(s.handleHomeDashboard))

	// Global search (topbar).
	mux.HandleFunc("GET /search", s.auth.RequireAuth(s.handleSearch))

	// Projects.
	mux.HandleFunc("GET /projects", s.auth.RequireAuth(s.handleProjectsList))
	mux.HandleFunc("GET /projects/new", s.auth.RequireAuth(s.handleProjectNewForm))
	mux.HandleFunc("POST /projects", s.auth.RequireAuth(s.handleProjectCreate))
	mux.HandleFunc("GET /projects/{slug}", s.auth.RequireAuth(s.handleProjectDetail))
	mux.HandleFunc("POST /projects/{slug}/rename", s.auth.RequireAuth(s.handleProjectRename))
	mux.HandleFunc("POST /projects/{slug}/apps/order", s.auth.RequireAuth(s.handleAppOrder))
	mux.HandleFunc("POST /projects/{slug}/delete", s.auth.RequireAuth(s.handleProjectDelete))
	mux.HandleFunc("POST /projects/{slug}/apps", s.auth.RequireAuth(s.handleAppCreate))

	// App actions.
	// Git-backed apps: a key is generated before the app exists (a private repo
	// cannot be cloned until its key is on GitHub), then deploys pull and build.
	mux.HandleFunc("POST /deploy-keys", s.auth.RequireAuth(s.handleDeployKeyNew))
	mux.HandleFunc("POST /apps/{id}/deploy", s.auth.RequireAuth(s.handleAppDeploy))
	mux.HandleFunc("POST /apps/{id}/deploy-key", s.auth.RequireAuth(s.handleAppDeployKeyRotate))
	mux.HandleFunc("POST /apps/{id}/webhook", s.auth.RequireAuth(s.handleAppWebhookSet))
	mux.HandleFunc("POST /apps/{id}/push-token", s.auth.RequireAuth(s.handleAppPushTokenSet))
	// Maintenance commands inside a container app. The body names an action
	// key, which is resolved against a fixed allowlist — never a command.
	mux.HandleFunc("GET /jobs/{id}", s.auth.RequireAuth(s.handleJob))
	mux.HandleFunc("POST /apps/{id}/action", s.auth.RequireAuth(s.handleAppAction))
	mux.HandleFunc("POST /apps/{id}/db-dump", s.auth.RequireAuth(s.handleAppDumpToggle))
	mux.HandleFunc("POST /apps/{id}/adminer", s.auth.RequireAuth(s.handleAppAdminerToggle))
	mux.HandleFunc("POST /apps/{id}/server", s.auth.RequireAuth(s.handleAppLaravelServer))
	mux.HandleFunc("GET /apps/{id}/deploys/partial", s.auth.RequireAuth(s.handleAppDeploysPartial))
	mux.HandleFunc("POST /apps/{id}/deploys/clear", s.auth.RequireAuth(s.handleAppDeploysClear))

	// The two endpoints reachable from the internet. No session, no CSRF: they
	// carry no cookie and prove themselves with a signature or a bearer token.
	// Caddy publishes these paths only on the hostnames of apps that switched
	// them on — see handlers_deploy.go.
	mux.HandleFunc("POST "+store.HookPathPrefix+"{hook}", s.handleGitHubHook)
	mux.HandleFunc("POST "+store.PushPath, s.handlePushDeploy)

	mux.HandleFunc("POST /apps/{id}/start", s.auth.RequireAuth(s.handleAppStart))
	mux.HandleFunc("POST /apps/{id}/stop", s.auth.RequireAuth(s.handleAppStop))
	mux.HandleFunc("POST /apps/{id}/delete", s.auth.RequireAuth(s.handleAppDelete))

	// App metrics.
	mux.HandleFunc("GET /apps/metrics.json", s.auth.RequireAuth(s.handleAppsMetricsJSON))
	mux.HandleFunc("GET /apps/{id}/metrics", s.auth.RequireAuth(s.handleAppMetrics))
	mux.HandleFunc("GET /apps/{id}/metrics.json", s.auth.RequireAuth(s.handleAppMetricsJSON))
	mux.HandleFunc("GET /host/metrics.json", s.auth.RequireAuth(s.handleHostMetricsJSON))

	// App ops: logs, env editor, backups.
	mux.HandleFunc("GET /apps/{id}/logs", s.auth.RequireAuth(s.handleAppLogs))
	mux.HandleFunc("POST /apps/{id}/logs/clear", s.auth.RequireAuth(s.handleAppLogsClear))
	mux.HandleFunc("GET /apps/{id}/env", s.auth.RequireAuth(s.handleAppEnvForm))
	mux.HandleFunc("POST /apps/{id}/env", s.auth.RequireAuth(s.handleAppEnvSave))
	mux.HandleFunc("GET /apps/{id}/settings", s.auth.RequireAuth(s.handleAppSettingsForm))
	mux.HandleFunc("POST /apps/{id}/settings", s.auth.RequireAuth(s.handleAppSettingsSave))
	mux.HandleFunc("POST /apps/{id}/backup", s.auth.RequireAuth(s.handleAppBackupCreate))
	mux.HandleFunc("POST /apps/{id}/import", s.auth.RequireAuth(s.handleAppImport))
	mux.HandleFunc("GET /apps/{id}/backups", s.auth.RequireAuth(s.handleAppBackups))
	mux.HandleFunc("GET /apps/{id}/backups/{name}", s.auth.RequireAuth(s.handleBackupDownload))

	// App ops: inline HTML fragments for the lazily-loaded project-page tabs.
	mux.HandleFunc("GET /apps/{id}/logs/partial", s.auth.RequireAuth(s.handleAppLogsPartial))
	mux.HandleFunc("GET /apps/{id}/env/partial", s.auth.RequireAuth(s.handleAppEnvPartial))
	mux.HandleFunc("GET /apps/{id}/backups/partial", s.auth.RequireAuth(s.handleAppBackupsPartial))

	// Activity / audit log.
	mux.HandleFunc("GET /events", s.auth.RequireAuth(s.handleEvents))
	mux.HandleFunc("POST /events/clear", s.auth.RequireAuth(s.handleEventsClear))

	// Settings.
	mux.HandleFunc("POST /settings/engine", s.auth.RequireAuth(s.handleSetEngine))
	mux.HandleFunc("POST /settings/hosts-sync", s.auth.RequireAuth(s.handleHostsSync))
	mux.HandleFunc("GET /settings/env", s.auth.RequireAuth(s.handleEnvSettings))
	mux.HandleFunc("POST /settings/env", s.auth.RequireAuth(s.handleEnvSettingsSave))

	// Shared MariaDB (platform service).
	mux.HandleFunc("GET /database", s.auth.RequireAuth(s.handleDatabase))
	mux.HandleFunc("GET /database/{name}", s.auth.RequireAuth(s.handleDatabaseDetail))
	mux.HandleFunc("POST /database/restart", s.auth.RequireAuth(s.handleDBRestart))
	mux.HandleFunc("POST /database/update", s.auth.RequireAuth(s.handleDBUpdate))
	mux.HandleFunc("POST /database/adminer", s.auth.RequireAuth(s.handleAdminer))
	mux.HandleFunc("POST /database/db/create", s.auth.RequireAuth(s.handleDBCreate))
	mux.HandleFunc("POST /database/db/drop", s.auth.RequireAuth(s.handleDBDrop))
	mux.HandleFunc("POST /database/user/create", s.auth.RequireAuth(s.handleDBUserCreate))
	mux.HandleFunc("POST /database/user/password", s.auth.RequireAuth(s.handleDBUserPassword))
	mux.HandleFunc("POST /database/user/drop", s.auth.RequireAuth(s.handleDBUserDrop))

	// Central WordPress plugin/theme pools (shared across all shared-host sites).
	mux.HandleFunc("GET /wordpress", s.auth.RequireAuth(s.handleWordPress))
	mux.HandleFunc("POST /wordpress/pool/upload", s.auth.RequireAuth(s.handleWPPoolUpload))
	mux.HandleFunc("POST /wordpress/pool/remove", s.auth.RequireAuth(s.handleWPPoolRemove))

	// Admins (multi-admin management).
	mux.HandleFunc("GET /admins", s.auth.RequireAuth(s.handleAdmins))
	mux.HandleFunc("POST /admins", s.auth.RequireAuth(s.handleAdminCreate))
	mux.HandleFunc("POST /admins/{id}/password", s.auth.RequireAuth(s.handleAdminPassword))
	mux.HandleFunc("POST /admins/{id}/delete", s.auth.RequireAuth(s.handleAdminDelete))

	// Everything that matched no route above. Same 404 the mux would produce by
	// itself, except it answers JSON to a caller that asked for JSON — a page
	// running an action with fetch would otherwise get plain text and be able to
	// say only "that was not JSON", which hides the actual problem (a path this
	// build does not serve).
	mux.HandleFunc("/", s.handleNotFound)

	s.mux = mux
}

// viewData is the data bag passed to templates.
type viewData map[string]any

// render executes a page template into a buffer first (so template errors don't
// emit half a page) and attaches common fields (user, CSRF token).
// renderFragment writes one named template from a page's set, without the
// layout — for the parts of a page that refresh on their own. The name has to
// be defined somewhere in that page's set (partials.html, or the page itself).
func (s *Server) renderFragment(w http.ResponseWriter, page, name string, data viewData) {
	t, ok := s.tmpl[page]
	if !ok {
		http.Error(w, "unknown page: "+page, http.StatusInternalServerError)
		return
	}
	var buf bytes.Buffer
	if err := t.ExecuteTemplate(&buf, name, data); err != nil {
		log.Printf("render fragment %s/%s: %v", page, name, err)
		http.Error(w, "template error", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	buf.WriteTo(w)
}

func (s *Server) render(w http.ResponseWriter, r *http.Request, page string, data viewData) {
	if data == nil {
		data = viewData{}
	}
	if _, ok := data["Title"]; !ok {
		data["Title"] = "xdev"
	}
	if u, ok := auth.UserFrom(r); ok {
		data["User"] = u
	}
	if sess, ok := auth.SessionFrom(r); ok {
		data["CSRF"] = sess.CSRFToken
	}
	data["Path"] = r.URL.Path
	data["NavLayout"] = "topbar"
	if c, err := r.Cookie("xdev_nav"); err == nil && c.Value == "sidebar" {
		data["NavLayout"] = "sidebar"
	}
	// Engine info backs the sidebar's engine mini-status on every page (cheap —
	// s.engine.Info() is a cached field). Handlers may override for their cards.
	if _, ok := data["Engine"]; !ok {
		data["Engine"] = string(s.engine.Current())
	}
	if _, ok := data["Runtime"]; !ok {
		data["Runtime"] = s.engine.Info()
	}

	t, ok := s.tmpl[page]
	if !ok {
		http.Error(w, "unknown page: "+page, http.StatusInternalServerError)
		return
	}
	var buf bytes.Buffer
	if err := t.ExecuteTemplate(&buf, "layout", data); err != nil {
		log.Printf("render %s: %v", page, err)
		http.Error(w, "template error", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	buf.WriteTo(w)
}

// proxyEnabled reports whether Caddy routing is active (controls whether the UI
// shows https site URLs).
// handleNotFound answers for any path no route claimed.
func (s *Server) handleNotFound(w http.ResponseWriter, r *http.Request) {
	writeProblem(w, r, "no such endpoint: "+r.Method+" "+r.URL.Path, http.StatusNotFound)
}

func (s *Server) proxyEnabled() bool {
	return s.reconciler != nil && s.reconciler.Enabled()
}

// reach answers "where is an app reachable" for the pages that show it. Unlike
// the apps service's copy, this one carries the *live* proxy state: a hostname
// with nothing routing behind it is not an address the UI should be linking to,
// and on a domainless install there is nothing routing at all.
func (s *Server) reach() apps.Reach {
	return apps.Reach{
		PublicHost:   s.cfg.PublicHost,
		ProxyEnabled: s.proxyEnabled(),
		HTTPSPort:    s.httpsPort,
	}
}

// reconcile re-syncs Caddy + the hosts file with the database after a mutation.
// Best-effort: failures are logged, not surfaced to the user.
//
// Deliberately not gated on proxyEnabled: Sync re-probes a proxy that is down,
// so a mutation is one of the things that can bring routing back. Skipping the
// call while disabled is what made that state permanent.
func (s *Server) reconcile() {
	if s.reconciler == nil {
		return
	}
	if err := s.reconciler.Sync(); err != nil {
		log.Printf("reconcile: %v", err)
	}
}

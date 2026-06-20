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
	httpsPort  int // public HTTPS port, for building site URLs
	tmpl       map[string]*template.Template // page name -> parsed template set
	mux        *http.ServeMux
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
	pages := []string{"setup", "login", "dashboard", "project_new", "project", "app_metrics",
		"app_logs", "app_env", "app_backups", "events"}
	s.tmpl = make(map[string]*template.Template, len(pages))
	for _, p := range pages {
		t, err := template.New(p).Funcs(tmplFuncs()).ParseFS(web.TemplatesFS,
			"templates/layout.html", "templates/"+p+".html")
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
	mux.HandleFunc("GET /{$}", s.auth.RequireAuth(s.handleDashboard))

	// Projects.
	mux.HandleFunc("GET /projects/new", s.auth.RequireAuth(s.handleProjectNewForm))
	mux.HandleFunc("POST /projects", s.auth.RequireAuth(s.handleProjectCreate))
	mux.HandleFunc("GET /projects/{slug}", s.auth.RequireAuth(s.handleProjectDetail))
	mux.HandleFunc("POST /projects/{slug}/delete", s.auth.RequireAuth(s.handleProjectDelete))
	mux.HandleFunc("POST /projects/{slug}/apps", s.auth.RequireAuth(s.handleAppCreate))

	// App actions.
	mux.HandleFunc("POST /apps/{id}/start", s.auth.RequireAuth(s.handleAppStart))
	mux.HandleFunc("POST /apps/{id}/stop", s.auth.RequireAuth(s.handleAppStop))
	mux.HandleFunc("POST /apps/{id}/delete", s.auth.RequireAuth(s.handleAppDelete))
	mux.HandleFunc("POST /apps/{id}/refresh", s.auth.RequireAuth(s.handleAppRefresh))

	// App metrics.
	mux.HandleFunc("GET /apps/{id}/metrics", s.auth.RequireAuth(s.handleAppMetrics))
	mux.HandleFunc("GET /apps/{id}/metrics.json", s.auth.RequireAuth(s.handleAppMetricsJSON))

	// App ops: logs, env editor, backups.
	mux.HandleFunc("GET /apps/{id}/logs", s.auth.RequireAuth(s.handleAppLogs))
	mux.HandleFunc("GET /apps/{id}/env", s.auth.RequireAuth(s.handleAppEnvForm))
	mux.HandleFunc("POST /apps/{id}/env", s.auth.RequireAuth(s.handleAppEnvSave))
	mux.HandleFunc("POST /apps/{id}/domain", s.auth.RequireAuth(s.handleAppDomain))
	mux.HandleFunc("POST /apps/{id}/backup", s.auth.RequireAuth(s.handleAppBackupCreate))
	mux.HandleFunc("GET /apps/{id}/backups", s.auth.RequireAuth(s.handleAppBackups))
	mux.HandleFunc("GET /apps/{id}/backups/{name}", s.auth.RequireAuth(s.handleBackupDownload))

	// Activity / audit log.
	mux.HandleFunc("GET /events", s.auth.RequireAuth(s.handleEvents))

	// Settings.
	mux.HandleFunc("POST /settings/engine", s.auth.RequireAuth(s.handleSetEngine))
	mux.HandleFunc("POST /settings/hosts-sync", s.auth.RequireAuth(s.handleHostsSync))

	s.mux = mux
}

// viewData is the data bag passed to templates.
type viewData map[string]any

// render executes a page template into a buffer first (so template errors don't
// emit half a page) and attaches common fields (user, CSRF token).
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
func (s *Server) proxyEnabled() bool {
	return s.reconciler != nil && s.reconciler.Enabled
}

// reconcile re-syncs Caddy + the hosts file with the database after a mutation.
// Best-effort: failures are logged, not surfaced to the user.
func (s *Server) reconcile() {
	if !s.proxyEnabled() {
		return
	}
	if err := s.reconciler.Sync(); err != nil {
		log.Printf("reconcile: %v", err)
	}
}

package server

import (
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"xdev/internal/templates"
)

// handleProjectNewForm shows the create-project form, pre-filling the base
// domain from the optional default_base_domain setting (set by the installer).
func (s *Server) handleProjectNewForm(w http.ResponseWriter, r *http.Request) {
	base, _ := s.store.GetSetting("default_base_domain")
	s.render(w, r, "project_new", viewData{
		"Title":      "New project · xdev",
		"BaseDomain": base,
	})
}

// handleProjectCreate creates a project (dir + network + row) and redirects to
// its detail page. On error it re-renders the form with the entered values.
func (s *Server) handleProjectCreate(w http.ResponseWriter, r *http.Request) {
	name := r.FormValue("name")
	environment := r.FormValue("environment")
	baseDomain := r.FormValue("base_domain")

	proj, err := s.projects.Create(name, baseDomain, environment)
	if err != nil {
		s.render(w, r, "project_new", viewData{
			"Title":       "New project · xdev",
			"Error":       err.Error(),
			"Name":        name,
			"BaseDomain":  baseDomain,
			"Environment": environment,
		})
		return
	}
	s.store.AddEvent(proj.ID, 0, "info", "Created project "+proj.Name)
	http.Redirect(w, r, "/projects/"+proj.Slug, http.StatusSeeOther)
}

// handleProjectDetail shows a project's apps and the new-app form.
func (s *Server) handleProjectDetail(w http.ResponseWriter, r *http.Request) {
	proj, err := s.store.ProjectBySlug(r.PathValue("slug"))
	if err != nil {
		http.NotFound(w, r)
		return
	}
	apps, err := s.store.ListAppsByProject(proj.ID)
	if err != nil {
		http.Error(w, "could not load apps", http.StatusInternalServerError)
		return
	}
	// Local domains that aren't *.localhost need a hosts-file entry to resolve;
	// collect them so the page can show the exact lines to add.
	var candidates []string
	if s.proxyEnabled() && proj.Environment == "local" {
		for _, a := range apps {
			if a.Domain != "" && !strings.HasSuffix(a.Domain, ".localhost") {
				candidates = append(candidates, a.Domain)
			}
		}
	}
	// Only nag about hostnames actually missing from the hosts file, so the
	// banner disappears once they're added.
	var needsHosts []string
	if len(candidates) > 0 {
		needsHosts = s.reconciler.MissingHosts(candidates)
	}
	s.render(w, r, "project", viewData{
		"Title":        proj.Name + " · xdev",
		"Project":      proj,
		"Apps":         apps,
		"Catalog":      templates.Catalog(),
		"Error":        r.URL.Query().Get("error"),
		"ProxyEnabled": s.proxyEnabled(),
		"HTTPSPort":    s.httpsPort,
		"NeedsHosts":   needsHosts,
		"HostsMsg":     r.URL.Query().Get("hosts_msg"),
	})
}

// handleProjectDelete tears down every app in the project, then the project.
func (s *Server) handleProjectDelete(w http.ResponseWriter, r *http.Request) {
	proj, err := s.store.ProjectBySlug(r.PathValue("slug"))
	if err != nil {
		http.NotFound(w, r)
		return
	}
	apps, _ := s.store.ListAppsByProject(proj.ID)
	for _, a := range apps {
		_ = s.apps.Delete(a.ID)
	}
	name := proj.Name
	if err := s.projects.Delete(proj.ID); err != nil {
		http.Error(w, "could not delete project: "+err.Error(), http.StatusInternalServerError)
		return
	}
	s.store.AddEvent(0, 0, "warn", "Deleted project "+name)
	s.reconcile()
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

// handleAppCreate adds an app to a project and starts it.
func (s *Server) handleAppCreate(w http.ResponseWriter, r *http.Request) {
	proj, err := s.store.ProjectBySlug(r.PathValue("slug"))
	if err != nil {
		http.NotFound(w, r)
		return
	}
	name := r.FormValue("name")
	appType := r.FormValue("type")
	domain := r.FormValue("domain")
	cpu, _ := strconv.ParseFloat(r.FormValue("cpu_cores"), 64)
	memMB, _ := strconv.ParseInt(r.FormValue("memory_mb"), 10, 64)
	var memBytes int64
	if memMB > 0 {
		memBytes = memMB * 1024 * 1024
	}

	app, err := s.apps.Create(proj.ID, name, appType, domain, cpu, memBytes)
	if err != nil {
		redirectWithError(w, r, "/projects/"+proj.Slug, err)
		return
	}
	s.store.AddEvent(proj.ID, app.ID, "info", "Created "+app.Type+" app "+app.Name)
	s.reconcile()
	http.Redirect(w, r, "/projects/"+proj.Slug, http.StatusSeeOther)
}

// handleAppStart / Stop / Refresh act on one app and return to its project page.
func (s *Server) handleAppStart(w http.ResponseWriter, r *http.Request) {
	s.appAction(w, r, "Started", s.apps.Start)
}
func (s *Server) handleAppStop(w http.ResponseWriter, r *http.Request) {
	s.appAction(w, r, "Stopped", s.apps.Stop)
}
func (s *Server) handleAppRefresh(w http.ResponseWriter, r *http.Request) {
	s.appAction(w, r, "", func(id int64) error { _, err := s.apps.RefreshStatus(id); return err })
}

// handleAppDelete removes an app and returns to its project page.
func (s *Server) handleAppDelete(w http.ResponseWriter, r *http.Request) {
	s.appAction(w, r, "Deleted", s.apps.Delete)
}

// appAction is the shared plumbing for per-app POST actions: parse the id,
// resolve the project (for the redirect target), run fn, log an event, and
// redirect back. An empty verb skips the audit-log entry.
func (s *Server) appAction(w http.ResponseWriter, r *http.Request, verb string, fn func(int64) error) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		http.Error(w, "bad app id", http.StatusBadRequest)
		return
	}
	app, err := s.store.AppByID(id)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	proj, err := s.store.ProjectByID(app.ProjectID)
	if err != nil {
		http.Error(w, "project not found", http.StatusInternalServerError)
		return
	}
	target := "/projects/" + proj.Slug
	if err := fn(id); err != nil {
		redirectWithError(w, r, target, err)
		return
	}
	if verb != "" {
		// After a delete the app row is gone, so don't reference its id (FK).
		aid := app.ID
		if verb == "Deleted" {
			aid = 0
		}
		s.store.AddEvent(proj.ID, aid, "info", verb+" app "+app.Name)
	}
	s.reconcile()
	http.Redirect(w, r, target, http.StatusSeeOther)
}

// redirectWithError redirects to target with a short ?error= message. Compose
// failures can be long; keep the surfaced message to the first line.
func redirectWithError(w http.ResponseWriter, r *http.Request, target string, err error) {
	msg := firstLine(err.Error())
	http.Redirect(w, r, target+"?error="+url.QueryEscape(msg), http.StatusSeeOther)
}

func firstLine(s string) string {
	for i := 0; i < len(s); i++ {
		if s[i] == '\n' {
			return s[:i]
		}
	}
	return s
}

package server

import (
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"xdev/internal/apps"
	"xdev/internal/store"
	"xdev/internal/templates"
)

// maxComposeDomains is how many domains (and so allocated host ports) a compose
// app may ask for, handed to the add-app form so the input's max and the
// server's check are the same number. Aliased here because the project handler
// keeps its app list in a variable named `apps`, shadowing the package.
const maxComposeDomains = apps.MaxComposeSlots

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
	// Secondary-service hostnames per app: one labelled link for the types that
	// ship a single extra UI (Adminer, the mail console), and the full list for
	// compose apps, which can route a hostname per published port.
	adminerDomains := map[int64]string{}
	serviceDomains := map[int64][]store.ServiceDomain{}
	for _, a := range apps {
		if a.IsCompose() {
			// Compose apps get the full list instead: their extra routes are the
			// user's own ports, not a bundled Adminer/admin console.
			if list, err := s.store.AppServiceDomains(a.ID); err == nil && len(list) > 0 {
				serviceDomains[a.ID] = list
			}
			continue
		}
		if h := s.store.AppServiceDomain(a.ID); h != "" {
			adminerDomains[a.ID] = h
		}
	}
	// Local domains that aren't *.localhost need a hosts-file entry to resolve;
	// collect them (primary + secondary services like Adminer) so the page can
	// show the exact lines to add.
	var candidates []string
	addCandidate := func(h string) {
		if h != "" && !strings.HasSuffix(h, ".localhost") {
			candidates = append(candidates, h)
		}
	}
	if s.proxyEnabled() && proj.Environment == "local" {
		for _, a := range apps {
			addCandidate(a.Domain)
			addCandidate(adminerDomains[a.ID])
			for _, d := range serviceDomains[a.ID] {
				addCandidate(d.Host)
			}
		}
	}
	// Only nag about hostnames actually missing from the hosts file, so the
	// banner disappears once they're added.
	var needsHosts []string
	if len(candidates) > 0 {
		needsHosts = s.reconciler.MissingHosts(candidates)
	}
	running := 0
	for _, a := range apps {
		if a.Status == store.AppRunning {
			running++
		}
	}
	activity, _ := s.store.ListEventsByProject(proj.ID, 8)
	s.render(w, r, "project", viewData{
		"Title":          proj.Name + " · xdev",
		"Project":        proj,
		"Apps":           apps,
		"AppsRunning":    running,
		"Activity":       activity,
		"AdminerDomains": adminerDomains,
		"ServiceDomains": serviceDomains,
		"Catalog":        templates.Catalog(),
		"MaxDomains":     maxComposeDomains,
		"Error":          r.URL.Query().Get("error"),
		"ProxyEnabled":   s.proxyEnabled(),
		"HTTPSPort":      s.httpsPort,
		"NeedsHosts":     needsHosts,
		"HostsMsg":       r.URL.Query().Get("hosts_msg"),
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
		_ = s.apps.Delete(a.ID, s.backupsRoot())
	}
	name := proj.Name
	if err := s.projects.Delete(proj.ID); err != nil {
		http.Error(w, "could not delete project: "+err.Error(), http.StatusInternalServerError)
		return
	}
	s.store.AddEvent(0, 0, "warn", "Deleted project "+name)
	s.reconcile()
	http.Redirect(w, r, "/projects", http.StatusSeeOther)
}

// handleAppCreate adds an app to a project and starts it.
func (s *Server) handleAppCreate(w http.ResponseWriter, r *http.Request) {
	proj, err := s.store.ProjectBySlug(r.PathValue("slug"))
	if err != nil {
		http.NotFound(w, r)
		return
	}
	// An optional backup archive makes this a multipart submit; FormValue still
	// reads the text fields either way.
	archive, closeArchive, err := uploadedArchive(r)
	if err != nil {
		redirectWithError(w, r, "/projects/"+proj.Slug, err)
		return
	}
	defer closeArchive()

	// Compose apps bring their own file (uploaded or pasted); other types ignore it.
	composeFile, err := suppliedComposeFile(r)
	if err != nil {
		redirectWithError(w, r, "/projects/"+proj.Slug, err)
		return
	}

	cpu, _ := strconv.ParseFloat(r.FormValue("cpu_cores"), 64)
	memMB, _ := strconv.ParseInt(r.FormValue("memory_mb"), 10, 64)
	var memBytes int64
	if memMB > 0 {
		memBytes = memMB * 1024 * 1024
	}

	app, err := s.apps.Create(proj.ID, apps.CreateOpts{
		Name:          r.FormValue("name"),
		Type:          r.FormValue("type"),
		Domain:        r.FormValue("domain"),
		CPULimit:      cpu,
		MemLimit:      memBytes,
		ServeMode:     r.FormValue("serve_mode"),
		RootDir:       r.FormValue("root_dir"),
		BuildCmd:      r.FormValue("build_cmd"),
		StartCmd:      r.FormValue("start_cmd"),
		Upstream:      r.FormValue("upstream"),
		ComposeFile:   composeFile,
		ExtraDomains:  extraDomains(r),
		AdminerDomain: r.FormValue("adminer_domain"),
		DBMode:        r.FormValue("db_mode"),
		WPMode:        r.FormValue("wp_mode"),
		Archive:       archive,
	})
	if err != nil {
		redirectWithError(w, r, "/projects/"+proj.Slug, err)
		return
	}
	s.store.AddEvent(proj.ID, app.ID, "info", "Created "+app.Type+" app "+app.Name)
	s.reconcile()
	http.Redirect(w, r, "/projects/"+proj.Slug, http.StatusSeeOther)
}

// extraDomains collects the hostnames for a compose app's domains 2..N. The
// add-app form emits one `extra_domain` field per row after the first, in the
// order shown, and that order is the ${PORT_n} numbering — so they are passed
// through as-is, blanks included, for the apps service to reject by slot.
func extraDomains(r *http.Request) []string {
	vals := r.Form["extra_domain"]
	out := make([]string, 0, len(vals))
	for _, v := range vals {
		out = append(out, strings.TrimSpace(v))
	}
	return out
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

// handleAppDelete removes an app (dumping its shared database into the backups
// dir first, if it has one) and returns to its project page.
func (s *Server) handleAppDelete(w http.ResponseWriter, r *http.Request) {
	s.appAction(w, r, "Deleted", func(id int64) error { return s.apps.Delete(id, s.backupsRoot()) })
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
	json := wantsJSON(r)
	if err := fn(id); err != nil {
		if json {
			// Non-2xx so the client falls back to a native submit and lands on
			// the server-rendered error banner.
			http.Error(w, firstLine(err.Error()), http.StatusInternalServerError)
			return
		}
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
	if json {
		status := "deleted"
		if verb != "Deleted" {
			if fresh, err := s.store.AppByID(id); err == nil {
				status = fresh.Status
			}
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"status":"` + status + `"}`))
		return
	}
	http.Redirect(w, r, target, http.StatusSeeOther)
}

// wantsJSON reports whether the caller (our fetch-based UI) wants an in-place
// JSON reply instead of the classic POST-redirect page reload.
func wantsJSON(r *http.Request) bool {
	return strings.Contains(r.Header.Get("Accept"), "application/json")
}

// redirectWithError redirects to target with a short ?error= message. Compose
// failures can be long; keep the surfaced message to the first line.
func redirectWithError(w http.ResponseWriter, r *http.Request, target string, err error) {
	msg := firstLine(err.Error())
	http.Redirect(w, r, target+"?error="+url.QueryEscape(msg), http.StatusSeeOther)
}

func firstLine(s string) string {
	line, _, _ := strings.Cut(s, "\n")
	return line
}

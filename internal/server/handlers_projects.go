package server

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"os"
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
	// The app's *other* hostnames — an app can be given several (a www. form, a
	// country domain) and they all serve the same thing. a.Domain is the first of
	// them and is already shown as the app's address, so only the rest go here.
	extraHosts := map[int64][]string{}
	for _, a := range apps {
		if hs, err := s.store.AppHostnames(a.ID); err == nil && len(hs) > 1 {
			extraHosts[a.ID] = hs[1:]
		}
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
			for _, h := range extraHosts[a.ID] {
				addCandidate(h)
			}
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
	// Where each app is actually reachable, worked out once here rather than
	// reassembled from domain + proxy state + port in the template. On a
	// domainless install these are the only addresses there are.
	reach := s.reach()
	addresses := make(map[int64]string, len(apps))
	portURLs := make(map[int64]string, len(apps))
	for _, a := range apps {
		addresses[a.ID] = reach.Address(a)
		portURLs[a.ID] = reach.PortAddress(a.Port)
	}
	s.render(w, r, "project", viewData{
		"Title":          proj.Name + " · xdev",
		"Project":        proj,
		"Apps":           apps,
		"AppsRunning":    running,
		"Activity":       activity,
		"AdminerDomains": adminerDomains,
		"ServiceDomains": serviceDomains,
		"ExtraHosts":     extraHosts,
		"Catalog":        templates.Catalog(),
		"MaxDomains":     maxComposeDomains,
		"Error":          r.URL.Query().Get("error"),
		"ProxyEnabled":   s.proxyEnabled(),
		"HTTPSPort":      s.httpsPort,
		"Addresses":      addresses,
		"PortURLs":       portURLs,
		"PortOnly":       reach.PortOnly(),
		"PublicHost":     reach.Host(),
		"NeedsHosts":     needsHosts,
		"HostsMsg":       r.URL.Query().Get("hosts_msg"),
	})
}

// handleProjectRename changes a project's display name.
//
// The slug stays put, so the URL this redirects back to is the one it came
// from: renaming is a label change, not a move of the directory, network and
// containers the slug names.
func (s *Server) handleProjectRename(w http.ResponseWriter, r *http.Request) {
	proj, err := s.store.ProjectBySlug(r.PathValue("slug"))
	if err != nil {
		http.NotFound(w, r)
		return
	}
	target := "/projects/" + proj.Slug
	was := proj.Name

	renamed, err := s.projects.Rename(proj.ID, r.FormValue("name"))
	if err != nil {
		if wantsJSON(r) {
			http.Error(w, firstLine(err.Error()), http.StatusBadRequest)
			return
		}
		redirectWithError(w, r, target, err)
		return
	}
	if renamed.Name != was {
		s.store.AddEvent(proj.ID, 0, "info", "Renamed project "+was+" to "+renamed.Name)
	}
	if wantsJSON(r) {
		writeJSON(w, map[string]string{"name": renamed.Name})
		return
	}
	http.Redirect(w, r, target, http.StatusSeeOther)
}

// handleAppOrder saves the order the project page's cards were dragged into.
//
// The client posts the whole list, form-encoded as repeated `id` fields — not
// JSON, because the CSRF middleware reads its token with r.FormValue and a JSON
// body has no form for it to read.
func (s *Server) handleAppOrder(w http.ResponseWriter, r *http.Request) {
	proj, err := s.store.ProjectBySlug(r.PathValue("slug"))
	if err != nil {
		http.NotFound(w, r)
		return
	}
	// The CSRF middleware has already parsed the form, but a direct caller (a
	// test) has not — and an unparsed form silently reads as an empty list,
	// which SetAppOrder would reject as stale rather than as malformed.
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad form", http.StatusBadRequest)
		return
	}
	raw := r.Form["id"]
	ids := make([]int64, 0, len(raw))
	for _, v := range raw {
		id, err := strconv.ParseInt(v, 10, 64)
		if err != nil {
			http.Error(w, "bad app id", http.StatusBadRequest)
			return
		}
		ids = append(ids, id)
	}

	if err := s.store.SetAppOrder(proj.ID, ids); err != nil {
		// A stale list is the page's fault, not the server's, and the fix is to
		// reload rather than retry — so it gets a 409 the client can act on.
		status := http.StatusInternalServerError
		if errors.Is(err, store.ErrStaleOrder) {
			status = http.StatusConflict
		}
		if wantsJSON(r) {
			http.Error(w, firstLine(err.Error()), status)
			return
		}
		redirectWithError(w, r, "/projects/"+proj.Slug, err)
		return
	}
	// No event log entry: reordering cards changes nothing about what is
	// deployed, and one line per drag would bury the activity feed.
	if wantsJSON(r) {
		writeJSON(w, map[string]string{"status": "ok"})
		return
	}
	http.Redirect(w, r, "/projects/"+proj.Slug, http.StatusSeeOther)
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

	opts := apps.CreateOpts{
		Name:          r.FormValue("name"),
		Type:          r.FormValue("type"),
		Domain:        r.FormValue("domain"),
		CPULimit:      cpu,
		MemLimit:      memBytes,
		ServeMode:     r.FormValue("serve_mode"),
		RootDir:       r.FormValue("root_dir"),
		SourceDir:     r.FormValue("source_dir"),
		Git:           gitOptsFrom(r),
		BuildCmd:      r.FormValue("build_cmd"),
		StartCmd:      r.FormValue("start_cmd"),
		Upstream:      r.FormValue("upstream"),
		ComposeFile:   composeFile,
		ExtraDomains:  extraDomains(r),
		AdminerDomain: r.FormValue("adminer_domain"),
		NoAdminer:     r.FormValue("adminer") == "0",
		DBMode:        r.FormValue("db_mode"),
		WPMode:        r.FormValue("wp_mode"),
		LaravelServer: r.FormValue("laravel_server"),
		Archive:       archive,
	}
	target := "/projects/" + proj.Slug

	// The dialog asked for JSON, so it can render progress: run the create in
	// the background and hand back a job id to poll. Cloning a repository and
	// running composer takes minutes, which is far too long to hold a request
	// open and much too long to show nothing.
	//
	// A native submit (no JS) keeps the old synchronous path — it has nowhere to
	// put progress, and a redirect at the end is the whole interaction.
	if wantsJSON(r) {
		if archive != nil {
			// The multipart file dies with the request, and the goroutine
			// outlives it. Spool it first, or the create reads a closed file.
			spooled, cleanup, err := spoolArchive(archive)
			if err != nil {
				writeJSONError(w, err, http.StatusBadRequest)
				return
			}
			opts.Archive = spooled
			defer cleanup()
			cleanupAfterJob := cleanup
			id, j := s.jobs.start()
			go s.runCreateJob(j, proj, opts, target, cleanupAfterJob)
			writeJSON(w, map[string]string{"job": id})
			return
		}
		id, j := s.jobs.start()
		go s.runCreateJob(j, proj, opts, target, nil)
		writeJSON(w, map[string]string{"job": id})
		return
	}

	app, err := s.apps.Create(proj.ID, opts)
	if err != nil {
		// The add-app dialog posts with Accept: application/json so a rejected
		// form can be answered in place — the modal stays open, holding the
		// pasted compose file and every field, and shows the message. A native
		// submit still gets the redirect + banner.
		redirectWithError(w, r, target, err)
		return
	}
	s.store.AddEvent(proj.ID, app.ID, "info", "Created "+app.Type+" app "+app.Name)
	s.reconcile()
	http.Redirect(w, r, target, http.StatusSeeOther)
}

// runCreateJob performs a create in the background, reporting each step into the
// job the dialog is polling. It owns the whole outcome: the job is the only
// thing left to tell anyone what happened.
func (s *Server) runCreateJob(j *job, proj store.Project, opts apps.CreateOpts, target string, cleanup func()) {
	if cleanup != nil {
		defer cleanup()
	}
	opts.Progress = func(step, out string) { j.step(step, out) }

	app, err := s.apps.Create(proj.ID, opts)
	if err != nil {
		j.finish(err.Error(), "")
		return
	}
	s.store.AddEvent(proj.ID, app.ID, "info", "Created "+app.Type+" app "+app.Name)
	s.reconcile()
	j.finish("", target)
}

// spoolArchive copies an uploaded archive somewhere that outlives the request,
// so a background create can still read it.
func spoolArchive(src io.Reader) (io.Reader, func(), error) {
	f, err := os.CreateTemp("", "xdev-upload-*.tar.gz")
	if err != nil {
		return nil, nil, err
	}
	cleanup := func() { f.Close(); os.Remove(f.Name()) }
	if _, err := io.Copy(f, src); err != nil {
		cleanup()
		return nil, nil, err
	}
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		cleanup()
		return nil, nil, err
	}
	return f, cleanup, nil
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

// handleAppStart / Stop act on one app and return to its project page.
func (s *Server) handleAppStart(w http.ResponseWriter, r *http.Request) {
	s.appAction(w, r, "Started", s.apps.Start)
}
func (s *Server) handleAppStop(w http.ResponseWriter, r *http.Request) {
	s.appAction(w, r, "Stopped", s.apps.Stop)
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
	asJSON := wantsJSON(r)
	if err := fn(id); err != nil {
		if asJSON {
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
	if asJSON {
		status := "deleted"
		if verb != "Deleted" {
			if fresh, err := s.store.AppByID(id); err == nil {
				status = fresh.Status
			}
		}
		writeJSON(w, map[string]string{"status": status})
		return
	}
	http.Redirect(w, r, target, http.StatusSeeOther)
}

// wantsJSON reports whether the caller (our fetch-based UI) wants an in-place
// JSON reply instead of the classic POST-redirect page reload.
func wantsJSON(r *http.Request) bool {
	return strings.Contains(r.Header.Get("Accept"), "application/json")
}

// writeJSONError answers a failed in-place POST. Unlike the redirect path this
// keeps the whole message: it lands in a panel that can wrap and scroll, and a
// compose failure's detail is the useful part.
func writeJSONError(w http.ResponseWriter, err error, status int) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": clampError(err.Error())})
}

// clampError bounds an error before it is shown: `compose up` can fail with
// screens of engine output, and the dialog wants a message, not a log.
func clampError(msg string) string {
	const maxLines, maxBytes = 12, 4000
	if len(msg) > maxBytes {
		msg = msg[:maxBytes] + "\n…"
	}
	if lines := strings.SplitN(msg, "\n", maxLines+1); len(lines) > maxLines {
		msg = strings.Join(lines[:maxLines], "\n") + "\n…"
	}
	return msg
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

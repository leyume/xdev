package server

import (
	"net/http"
	"net/url"
	"path/filepath"
	"strconv"
	"strings"

	xruntime "xdev/internal/runtime"
	"xdev/internal/store"
)

// backupsRoot is where per-app backup archives live.
func (s *Server) backupsRoot() string {
	return filepath.Join(s.cfg.DataDir, "backups")
}

// appAndProject loads an app and its project, or writes a 404/500.
func (s *Server) appAndProject(w http.ResponseWriter, r *http.Request) (store.App, store.Project, bool) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		http.Error(w, "bad app id", http.StatusBadRequest)
		return store.App{}, store.Project{}, false
	}
	app, err := s.store.AppByID(id)
	if err != nil {
		http.NotFound(w, r)
		return store.App{}, store.Project{}, false
	}
	proj, err := s.store.ProjectByID(app.ProjectID)
	if err != nil {
		http.Error(w, "project not found", http.StatusInternalServerError)
		return store.App{}, store.Project{}, false
	}
	return app, proj, true
}

// handleAppLogs shows the tail of an app's container logs.
func (s *Server) handleAppLogs(w http.ResponseWriter, r *http.Request) {
	app, proj, ok := s.appAndProject(w, r)
	if !ok {
		return
	}
	logs, err := s.apps.Logs(app.ID, 300)
	if err != nil {
		logs = "could not read logs: " + err.Error()
	}
	s.render(w, r, "app_logs", viewData{
		"Title": app.Name + " logs · xdev", "App": app, "Project": proj, "Logs": logs,
	})
}

// handleAppEnvForm shows the editable .env for an app.
func (s *Server) handleAppEnvForm(w http.ResponseWriter, r *http.Request) {
	app, proj, ok := s.appAndProject(w, r)
	if !ok {
		return
	}
	content, err := s.apps.ReadEnv(app.ID)
	if err != nil {
		http.Error(w, "could not read .env", http.StatusInternalServerError)
		return
	}
	s.render(w, r, "app_env", viewData{
		"Title": app.Name + " .env · xdev", "App": app, "Project": proj, "Content": content,
		"Saved": r.URL.Query().Get("saved") != "",
	})
}

// handleAppEnvSave writes the .env and restarts the app to apply it.
func (s *Server) handleAppEnvSave(w http.ResponseWriter, r *http.Request) {
	app, proj, ok := s.appAndProject(w, r)
	if !ok {
		return
	}
	if err := s.apps.WriteEnv(app.ID, r.FormValue("content")); err != nil {
		redirectWithError(w, r, "/apps/"+strconv.FormatInt(app.ID, 10)+"/env", err)
		return
	}
	_ = s.apps.Start(app.ID) // restart (idempotent up -d) to pick up new env
	s.store.AddEvent(proj.ID, app.ID, "info", "Updated .env for app "+app.Name)
	http.Redirect(w, r, "/apps/"+strconv.FormatInt(app.ID, 10)+"/env?saved=1", http.StatusSeeOther)
}

// handleAppBackupCreate makes a new backup archive of the app directory.
func (s *Server) handleAppBackupCreate(w http.ResponseWriter, r *http.Request) {
	app, proj, ok := s.appAndProject(w, r)
	if !ok {
		return
	}
	target := "/apps/" + strconv.FormatInt(app.ID, 10) + "/backups"
	if _, err := s.apps.Backup(app.ID, s.backupsRoot()); err != nil {
		redirectWithError(w, r, target, err)
		return
	}
	s.store.AddEvent(proj.ID, app.ID, "info", "Created backup of app "+app.Name)
	http.Redirect(w, r, target, http.StatusSeeOther)
}

// handleAppBackups lists an app's backup archives.
func (s *Server) handleAppBackups(w http.ResponseWriter, r *http.Request) {
	app, proj, ok := s.appAndProject(w, r)
	if !ok {
		return
	}
	backups, err := s.apps.ListBackups(app.ID, s.backupsRoot())
	if err != nil {
		http.Error(w, "could not list backups", http.StatusInternalServerError)
		return
	}
	s.render(w, r, "app_backups", viewData{
		"Title": app.Name + " backups · xdev", "App": app, "Project": proj, "Backups": backups,
	})
}

// handleBackupDownload streams a backup archive to the browser.
func (s *Server) handleBackupDownload(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		http.Error(w, "bad app id", http.StatusBadRequest)
		return
	}
	path, err := s.apps.BackupPath(id, s.backupsRoot(), r.PathValue("name"))
	if err != nil {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Disposition", "attachment; filename="+url.PathEscape(filepath.Base(path)))
	http.ServeFile(w, r, path)
}

// handleAppDomain changes the hostname an app is served at.
func (s *Server) handleAppDomain(w http.ResponseWriter, r *http.Request) {
	app, proj, ok := s.appAndProject(w, r)
	if !ok {
		return
	}
	target := "/projects/" + proj.Slug
	if err := s.apps.SetDomain(app.ID, r.FormValue("domain")); err != nil {
		redirectWithError(w, r, target, err)
		return
	}
	s.store.AddEvent(proj.ID, app.ID, "info", "Changed domain of app "+app.Name)
	s.reconcile()
	http.Redirect(w, r, target, http.StatusSeeOther)
}

// handleSetEngine switches the default container engine for new projects.
// Existing projects/apps keep the engine they were created with.
func (s *Server) handleSetEngine(w http.ResponseWriter, r *http.Request) {
	eng := xruntime.Engine(r.FormValue("engine"))
	if err := s.engine.Set(eng); err != nil {
		http.Redirect(w, r, "/?engine_msg="+url.QueryEscape(err.Error()), http.StatusSeeOther)
		return
	}
	s.store.SetSetting("engine", string(eng))
	s.store.AddEvent(0, 0, "info", "Default engine set to "+string(eng))
	http.Redirect(w, r, "/?engine_msg="+url.QueryEscape("Default engine is now "+string(eng)+" (applies to new projects)"), http.StatusSeeOther)
}

// handleHostsSync writes the local domains into the hosts file, elevating via
// the OS prompt if needed. Returns to the page the button was clicked from.
func (s *Server) handleHostsSync(w http.ResponseWriter, r *http.Request) {
	msg := "Local domains added to your hosts file."
	if err := s.reconciler.WriteHostsElevated(); err != nil {
		msg = "Could not update hosts file: " + firstLine(err.Error())
	}
	ref := r.Header.Get("Referer")
	if ref == "" {
		ref = "/"
	}
	sep := "?"
	if strings.Contains(ref, "?") {
		sep = "&"
	}
	http.Redirect(w, r, ref+sep+"hosts_msg="+url.QueryEscape(msg), http.StatusSeeOther)
}

// handleEvents shows the audit log.
func (s *Server) handleEvents(w http.ResponseWriter, r *http.Request) {
	events, err := s.store.ListEvents(200)
	if err != nil {
		http.Error(w, "could not load events", http.StatusInternalServerError)
		return
	}
	s.render(w, r, "events", viewData{"Title": "Activity · xdev", "Events": events})
}

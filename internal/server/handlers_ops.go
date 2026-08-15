package server

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"net/url"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"xdev/internal/apps"
	"xdev/internal/auth"
	xruntime "xdev/internal/runtime"
	"xdev/internal/store"
)

// backupsRoot is where per-app backup archives live.
func (s *Server) backupsRoot() string {
	return filepath.Join(s.cfg.DataDir, "backups")
}

// backupStats scans the backups root (across all apps) and returns the total
// archive count and the most-recent archive modtime (zero if none exist).
func backupStats(root string) (int, time.Time) {
	var count int
	var latest time.Time
	filepath.WalkDir(root, func(_ string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil // missing root or unreadable entry: just skip
		}
		info, err := d.Info()
		if err != nil {
			return nil
		}
		count++
		if info.ModTime().After(latest) {
			latest = info.ModTime()
		}
		return nil
	})
	return count, latest
}

// humanizeSince renders how long ago t was, coarsely ("just now", "5m ago",
// "3h ago", "2d ago").
func humanizeSince(t time.Time) string {
	d := time.Since(t)
	switch {
	case d < time.Minute:
		return "just now"
	case d < time.Hour:
		return fmt.Sprintf("%dm ago", int(d.Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf("%dh ago", int(d.Hours()))
	default:
		return fmt.Sprintf("%dd ago", int(d.Hours())/24)
	}
}

// appAndProject loads an app and its project, or writes a 404/500.
//
// The failures answer JSON to a caller that asked for it. Pages on this site
// run actions with fetch, and a plain-text 404 from here reaches them as "the
// answer was not JSON" — which the page can only report as something generic,
// hiding the one useful fact: that this app id no longer exists.
func (s *Server) appAndProject(w http.ResponseWriter, r *http.Request) (store.App, store.Project, bool) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		writeProblem(w, r, "bad app id", http.StatusBadRequest)
		return store.App{}, store.Project{}, false
	}
	app, err := s.store.AppByID(id)
	if err != nil {
		writeProblem(w, r, fmt.Sprintf("no app with id %d — it may have been deleted; reload the page", id),
			http.StatusNotFound)
		return store.App{}, store.Project{}, false
	}
	proj, err := s.store.ProjectByID(app.ProjectID)
	if err != nil {
		writeProblem(w, r, "project not found", http.StatusInternalServerError)
		return store.App{}, store.Project{}, false
	}
	return app, proj, true
}

// writeProblem reports a request-level failure in whichever form the caller can
// read: JSON for the pages that use fetch, plain text for a browser navigation.
func writeProblem(w http.ResponseWriter, r *http.Request, msg string, status int) {
	if wantsJSON(r) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		json.NewEncoder(w).Encode(map[string]string{"error": msg})
		return
	}
	http.Error(w, msg, status)
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
		"CanClear": apps.CanClearLogs(app),
		"Cleared":  r.URL.Query().Get("cleared") != "",
		"Error":    r.URL.Query().Get("error"),
	})
}

// handleAppLogsClear empties a host-process app's log file.
func (s *Server) handleAppLogsClear(w http.ResponseWriter, r *http.Request) {
	app, proj, ok := s.appAndProject(w, r)
	if !ok {
		return
	}
	target := "/apps/" + strconv.FormatInt(app.ID, 10) + "/logs"
	if err := s.apps.ClearLogs(app.ID); err != nil {
		redirectWithError(w, r, target, err)
		return
	}
	s.store.AddEvent(proj.ID, app.ID, "info", "Cleared the log for app "+app.Name)
	http.Redirect(w, r, target+"?cleared=1", http.StatusSeeOther)
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
		"EnvPath": s.apps.EnvLocation(app.ID),
		"Saved":   r.URL.Query().Get("saved") != "",
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

// uploadedArchive returns the "archive" file from a multipart submit, or a nil
// reader when none was chosen (the field is optional on app create). The caller
// must call the returned cleanup. A non-multipart form is not an error. The
// body is already size-capped by the auth middleware (auth.MaxRequestBody).
func uploadedArchive(r *http.Request) (io.Reader, func(), error) {
	noop := func() {}
	if ct := r.Header.Get("Content-Type"); !strings.HasPrefix(ct, "multipart/form-data") {
		return nil, noop, nil
	}
	if err := r.ParseMultipartForm(32 << 20); err != nil {
		return nil, noop, fmt.Errorf("upload too large (max %d MB) or malformed", auth.MaxRequestBody>>20)
	}
	file, hdr, err := r.FormFile("archive")
	if err != nil || hdr.Size == 0 {
		if file != nil {
			file.Close()
		}
		return nil, noop, nil // no file chosen — not an import
	}
	if !strings.HasSuffix(strings.ToLower(hdr.Filename), ".tar.gz") &&
		!strings.HasSuffix(strings.ToLower(hdr.Filename), ".tgz") {
		file.Close()
		return nil, noop, errors.New("import needs a .tar.gz backup archive")
	}
	return file, func() { file.Close() }, nil
}

// maxComposeUpload caps an uploaded compose file. Compose files are a few KB;
// the apps service applies the same limit to pasted ones.
const maxComposeUpload = 256 << 10

// suppliedComposeFile returns the compose file a "Compose" app was created with:
// the uploaded compose_file if one was chosen, else the pasted compose_yaml
// textarea. "" means none was supplied — the app gets the starter compose file.
// Callers run after uploadedArchive, which has already parsed a multipart form.
func suppliedComposeFile(r *http.Request) (string, error) {
	if r.MultipartForm == nil {
		return r.FormValue("compose_yaml"), nil
	}
	file, hdr, err := r.FormFile("compose_file")
	if err != nil || hdr.Size == 0 {
		if file != nil {
			file.Close()
		}
		return r.FormValue("compose_yaml"), nil // no file chosen — use the textarea
	}
	defer file.Close()
	if name := strings.ToLower(hdr.Filename); !strings.HasSuffix(name, ".yml") && !strings.HasSuffix(name, ".yaml") {
		return "", errors.New("compose file must be a .yml / .yaml file (docker-compose.yml or compose.yml)")
	}
	if hdr.Size > maxComposeUpload {
		return "", fmt.Errorf("compose file is too large (max %d KB)", maxComposeUpload>>10)
	}
	b, err := io.ReadAll(io.LimitReader(file, maxComposeUpload+1))
	if err != nil {
		return "", fmt.Errorf("read compose file: %w", err)
	}
	if len(b) > maxComposeUpload {
		return "", fmt.Errorf("compose file is too large (max %d KB)", maxComposeUpload>>10)
	}
	return string(b), nil
}

// handleAppImport overwrites an existing app's files from an uploaded backup
// archive, restarting it afterwards.
func (s *Server) handleAppImport(w http.ResponseWriter, r *http.Request) {
	app, proj, ok := s.appAndProject(w, r)
	if !ok {
		return
	}
	target := "/apps/" + strconv.FormatInt(app.ID, 10) + "/backups"
	archive, closeArchive, err := uploadedArchive(r)
	if err != nil {
		redirectWithError(w, r, target, err)
		return
	}
	defer closeArchive()
	if archive == nil {
		redirectWithError(w, r, target, errors.New("choose a .tar.gz backup to import"))
		return
	}
	if err := s.apps.Import(app.ID, archive); err != nil {
		redirectWithError(w, r, target, err)
		return
	}
	s.store.AddEvent(proj.ID, app.ID, "warn", "Imported backup over app "+app.Name)
	s.reconcile()
	http.Redirect(w, r, target, http.StatusSeeOther)
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

// handleSetEngine switches the default container engine for new projects.
// Existing projects/apps keep the engine they were created with. The engine
// card appears on both the Dashboard and Projects pages, so return to
// whichever one the request came from.
func (s *Server) handleSetEngine(w http.ResponseWriter, r *http.Request) {
	ref := r.Header.Get("Referer")
	if ref == "" {
		ref = "/"
	}
	base, _, _ := strings.Cut(ref, "?")

	eng := xruntime.Engine(r.FormValue("engine"))
	if err := s.engine.Set(eng); err != nil {
		http.Redirect(w, r, base+"?engine_msg="+url.QueryEscape(err.Error()), http.StatusSeeOther)
		return
	}
	s.store.SetSetting("engine", string(eng))
	s.store.AddEvent(0, 0, "info", "Default engine set to "+string(eng))
	http.Redirect(w, r, base+"?engine_msg="+url.QueryEscape("Default engine is now "+string(eng)+" (applies to new projects)"), http.StatusSeeOther)
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

// handleEventsClear empties the activity log, all of it or one project's.
//
// The scope comes from a form field rather than two routes, because the two
// buttons differ only in what they cover and the page already knows which it
// is showing. A project id that no longer exists simply matches nothing.
func (s *Server) handleEventsClear(w http.ResponseWriter, r *http.Request) {
	projectID, _ := strconv.ParseInt(r.FormValue("project_id"), 10, 64)
	target := "/events"
	if projectID > 0 {
		target = "/projects/" + strconv.FormatInt(projectID, 10)
	}
	n, err := s.store.ClearEvents(projectID)
	if err != nil {
		redirectWithError(w, r, target, err)
		return
	}
	// The first entry in the newly-empty log says who emptied it. Written after
	// the delete on purpose — written before, it would delete itself.
	s.store.AddEvent(projectID, 0, "info", fmt.Sprintf("Cleared %d activity entries", n))
	http.Redirect(w, r, target+"?cleared="+strconv.FormatInt(n, 10), http.StatusSeeOther)
}

// handleEvents shows the audit log.
func (s *Server) handleEvents(w http.ResponseWriter, r *http.Request) {
	events, err := s.store.ListEvents(200)
	if err != nil {
		http.Error(w, "could not load events", http.StatusInternalServerError)
		return
	}
	cleared, _ := strconv.Atoi(r.URL.Query().Get("cleared"))
	s.render(w, r, "events", viewData{
		"Title": "Activity · xdev", "Events": events, "Cleared": cleared,
	})
}

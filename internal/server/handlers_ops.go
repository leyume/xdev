package server

import (
	"encoding/csv"
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

// opsReturn is where an app's logs/env/backups action lands: back on the
// settings tab it was posted from (?from=settings on the form's action), or
// on the action's own page.
func opsReturn(r *http.Request, app store.App, page, tab string) (string, bool) {
	id := strconv.FormatInt(app.ID, 10)
	if r.URL.Query().Get("from") == "settings" {
		return "/apps/" + id + "/settings?tab=" + tab, true
	}
	return "/apps/" + id + "/" + page, false
}

// withQuery adds k=v to target, which may already have a query.
func withQuery(target, k, v string) string {
	sep := "?"
	if strings.Contains(target, "?") {
		sep = "&"
	}
	return target + sep + k + "=" + url.QueryEscape(v)
}

// opsFail sends an action's error back to where it came from.
func opsFail(w http.ResponseWriter, r *http.Request, target string, err error) {
	http.Redirect(w, r, withQuery(target, "error", firstLine(err.Error())), http.StatusSeeOther)
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
	target, settings := opsReturn(r, app, "logs", "logs")
	if err := s.apps.ClearLogs(app.ID); err != nil {
		opsFail(w, r, target, err)
		return
	}
	s.store.AddEvent(proj.ID, app.ID, "info", "Cleared the log for app "+app.Name)
	if settings {
		target = withQuery(target, "msg", "Log cleared.")
	} else {
		target = withQuery(target, "cleared", "1")
	}
	http.Redirect(w, r, target, http.StatusSeeOther)
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
	target, settings := opsReturn(r, app, "env", "env")
	if err := s.apps.WriteEnv(app.ID, r.FormValue("content")); err != nil {
		opsFail(w, r, target, err)
		return
	}
	_ = s.apps.Start(app.ID) // restart (idempotent up -d) to pick up new env
	s.store.AddEvent(proj.ID, app.ID, "info", "Updated .env for app "+app.Name)
	if settings {
		target = withQuery(target, "msg", "Saved .env and restarted the app.")
	} else {
		target = withQuery(target, "saved", "1")
	}
	http.Redirect(w, r, target, http.StatusSeeOther)
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

// uploadedCode returns the "code_archive" file from a multipart submit — a
// .zip or .tar.gz of the user's own code, offered as one of the sources a
// static app can be created from. A nil reader means none was chosen, which is
// not an error: the field is optional, and most apps come from somewhere else.
// Callers run after uploadedArchive, which has already parsed the form.
func uploadedCode(r *http.Request) (io.Reader, func(), error) {
	noop := func() {}
	if r.MultipartForm == nil {
		return nil, noop, nil
	}
	file, hdr, err := r.FormFile("code_archive")
	if err != nil || hdr.Size == 0 {
		if file != nil {
			file.Close()
		}
		return nil, noop, nil
	}
	// The extension is checked for the error message's sake — it is what the
	// user can see and fix. What the file actually is gets decided from its
	// first bytes when it is unpacked.
	name := strings.ToLower(hdr.Filename)
	if !strings.HasSuffix(name, ".zip") && !strings.HasSuffix(name, ".tar.gz") && !strings.HasSuffix(name, ".tgz") {
		file.Close()
		return nil, noop, errors.New("upload your code as a .zip or .tar.gz archive")
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
	target, settings := opsReturn(r, app, "backups", "backups")
	archive, closeArchive, err := uploadedArchive(r)
	if err != nil {
		opsFail(w, r, target, err)
		return
	}
	defer closeArchive()
	if archive == nil {
		opsFail(w, r, target, errors.New("choose a .tar.gz backup to import"))
		return
	}
	if err := s.apps.Import(app.ID, archive); err != nil {
		opsFail(w, r, target, err)
		return
	}
	s.store.AddEvent(proj.ID, app.ID, "warn", "Imported backup over app "+app.Name)
	s.reconcile()
	if settings {
		target = withQuery(target, "msg", "Imported the backup over "+app.Name+".")
	}
	http.Redirect(w, r, target, http.StatusSeeOther)
}

// handleAppBackupCreate makes a new backup archive of the app directory.
func (s *Server) handleAppBackupCreate(w http.ResponseWriter, r *http.Request) {
	app, proj, ok := s.appAndProject(w, r)
	if !ok {
		return
	}
	target, settings := opsReturn(r, app, "backups", "backups")
	name, err := s.apps.Backup(app.ID, s.backupsRoot())
	if err != nil {
		opsFail(w, r, target, err)
		return
	}
	s.store.AddEvent(proj.ID, app.ID, "info", "Created backup of app "+app.Name)
	if settings {
		target = withQuery(target, "msg", "Backed up to "+filepath.Base(name)+".")
	}
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

// activityPageSize is how many entries one page of the activity log shows.
const activityPageSize = 25

// activityTab is one tab on the activity page: a project (or All / Other), or
// one of the chosen project's apps.
type activityTab struct {
	Label, Href string
	Count       int
	Active      bool
}

// activityRow is one log entry as the page shows it: local time, a kind that
// picks its icon, and a day heading on the first entry of each day.
type activityRow struct {
	store.Activity
	Day, Time, Full, Short, Kind string
}

// handleEvents shows the activity log, paged, with a tab per project and,
// inside a project, per app. ?project=<slug>|other, ?app=<id>, ?page=<n>.
func (s *Server) handleEvents(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	projectID, appID, proj, ok := s.activityFilter(r)
	if !ok {
		http.NotFound(w, r)
		return
	}
	page, _ := strconv.Atoi(q.Get("page"))
	if page < 1 {
		page = 1
	}
	// Page size: the 10/25/50 presets or any custom number, within reason.
	per, _ := strconv.Atoi(q.Get("per"))
	if per < 1 {
		per = activityPageSize
	}
	per = min(per, 500)
	rows, total, err := s.store.ActivityPage(projectID, appID, per, (page-1)*per)
	byProject, byApp, cerr := s.store.ActivityCounts()
	if err != nil || cerr != nil {
		http.Error(w, "could not load events", http.StatusInternalServerError)
		return
	}
	pages := max(1, (total+per-1)/per)

	href := func(project string, app int64, page int) string {
		v := url.Values{}
		if per != activityPageSize {
			v.Set("per", strconv.Itoa(per))
		}
		if project != "" {
			v.Set("project", project)
		}
		if app > 0 {
			v.Set("app", strconv.FormatInt(app, 10))
		}
		if page > 1 {
			v.Set("page", strconv.Itoa(page))
		}
		if e := v.Encode(); e != "" {
			return "/events?" + e
		}
		return "/events"
	}

	// Project tabs: every project with activity, in the sidebar's order.
	all := 0
	for _, n := range byProject {
		all += n
	}
	tabs := []activityTab{{Label: "All", Href: href("", 0, 1), Count: all, Active: projectID == 0}}
	projects, _ := s.store.ListProjects()
	for _, p := range projects {
		if n := byProject[p.ID]; n > 0 || p.ID == projectID {
			tabs = append(tabs, activityTab{Label: p.Name, Href: href(p.Slug, 0, 1), Count: n, Active: p.ID == projectID})
		}
	}
	if n := byProject[store.SystemProject]; n > 0 {
		tabs = append(tabs, activityTab{Label: "Other", Href: href("other", 0, 1), Count: n, Active: projectID == store.SystemProject})
	}
	// App tabs, inside a project.
	var appTabs []activityTab
	if proj != nil {
		appTabs = append(appTabs, activityTab{Label: "All apps", Href: href(proj.Slug, 0, 1), Count: byProject[proj.ID], Active: appID == 0})
		apps, _ := s.store.ListAppsByProject(proj.ID)
		for _, a := range apps {
			if n := byApp[a.ID]; n > 0 || a.ID == appID {
				appTabs = append(appTabs, activityTab{Label: a.Name, Href: href(proj.Slug, a.ID, 1), Count: n, Active: a.ID == appID})
			}
		}
	}

	slug := ""
	if proj != nil {
		slug = proj.Slug
	} else if projectID == store.SystemProject {
		slug = "other"
	}
	view := make([]activityRow, 0, len(rows))
	lastDay := ""
	now := time.Now()
	for _, a := range rows {
		row := activityRow{Activity: a, Kind: activityKind(a.Level, a.Message)}
		if t, err := time.ParseInLocation("2006-01-02 15:04:05", a.TS, time.UTC); err == nil {
			t = t.Local()
			row.Time, row.Full, row.Short = t.Format("15:04"), t.Format("Mon 2 Jan 2006, 15:04:05"), t.Format("2 Jan 2006 15:04")
			if d := dayLabel(t, now); d != lastDay {
				row.Day, lastDay = d, d
			}
		} else {
			row.Time, row.Full = a.TS, a.TS
		}
		view = append(view, row)
	}

	title := "Activity · xdev"
	if proj != nil {
		title = "Activity · " + proj.Name + " · xdev"
	}
	data := viewData{
		"Title": title, "Events": view, "Project": proj, "Tabs": tabs, "AppTabs": appTabs,
		"Total": total, "Page": page, "Pages": pages, "Per": per,
		"From": (page-1)*per + 1, "To": (page-1)*per + len(view),
		"Filter": strings.TrimPrefix(href(slug, appID, 1), "/events"),
		"Slug":   slug, "AppID": appID, "PerPreset": per == 10 || per == 25 || per == 50,
	}
	if page > 1 {
		data["PrevHref"] = href(slug, appID, page-1)
	}
	if page < pages {
		data["NextHref"] = href(slug, appID, page+1)
	}
	if n, _ := strconv.Atoi(q.Get("cleared")); n > 0 {
		data["Msg"] = fmt.Sprintf("Activity log cleared (%d entries).", n)
	}
	s.render(w, r, "events", data)
}

// activityFilter reads the activity page's ?project=<slug>|other and ?app=<id>.
// ok is false for a project slug that doesn't exist.
func (s *Server) activityFilter(r *http.Request) (projectID, appID int64, proj *store.Project, ok bool) {
	q := r.URL.Query()
	switch slug := q.Get("project"); slug {
	case "":
	case "other":
		projectID = store.SystemProject
	default:
		p, err := s.store.ProjectBySlug(slug)
		if err != nil {
			return 0, 0, nil, false
		}
		proj, projectID = &p, p.ID
	}
	if proj != nil { // app tabs only exist inside a project
		appID, _ = strconv.ParseInt(q.Get("app"), 10, 64)
	}
	return projectID, appID, proj, true
}

// handleEventsExport downloads every entry matching the activity page's tabs
// (not just the page on screen) as CSV, or JSON with ?format=json.
func (s *Server) handleEventsExport(w http.ResponseWriter, r *http.Request) {
	projectID, appID, proj, ok := s.activityFilter(r)
	if !ok {
		http.NotFound(w, r)
		return
	}
	rows, _, err := s.store.ActivityPage(projectID, appID, -1, 0) // -1: SQLite for no limit
	if err != nil {
		http.Error(w, "could not load events", http.StatusInternalServerError)
		return
	}
	name := "xdev-activity"
	if proj != nil {
		name += "-" + proj.Slug
	}
	name += "-" + time.Now().Format("2006-01-02")
	type entry struct {
		Time    string `json:"time"`
		Level   string `json:"level"`
		Project string `json:"project,omitempty"`
		App     string `json:"app,omitempty"`
		Message string `json:"message"`
	}
	out := make([]entry, 0, len(rows))
	for _, a := range rows {
		ts := a.TS
		if t, err := time.ParseInLocation("2006-01-02 15:04:05", a.TS, time.UTC); err == nil {
			ts = t.Format(time.RFC3339)
		}
		out = append(out, entry{ts, a.Level, a.ProjectName, a.AppName, a.Message})
	}
	if r.URL.Query().Get("format") == "json" {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Content-Disposition", `attachment; filename="`+name+`.json"`)
		enc := json.NewEncoder(w)
		enc.SetIndent("", "  ")
		_ = enc.Encode(out)
		return
	}
	w.Header().Set("Content-Type", "text/csv; charset=utf-8")
	w.Header().Set("Content-Disposition", `attachment; filename="`+name+`.csv"`)
	cw := csv.NewWriter(w)
	_ = cw.Write([]string{"time", "level", "project", "app", "message"})
	for _, e := range out {
		_ = cw.Write([]string{e.Time, e.Level, e.Project, e.App, e.Message})
	}
	cw.Flush()
}

// activityKind sorts an entry into the handful of kinds the page gives an icon
// and colour: what happened, read off the message's leading verb.
func activityKind(level, msg string) string {
	switch level {
	case "error":
		return "error"
	case "warn":
		return "warn"
	}
	m := strings.ToLower(msg)
	switch {
	case strings.HasPrefix(m, "started"), strings.HasPrefix(m, "restarted"):
		return "start"
	case strings.HasPrefix(m, "stopped"):
		return "stop"
	case strings.Contains(m, "deploy"):
		return "deploy"
	case strings.HasPrefix(m, "deleted"), strings.HasPrefix(m, "removed"),
		strings.HasPrefix(m, "dropped"), strings.HasPrefix(m, "cleared"):
		return "delete"
	case strings.HasPrefix(m, "created"), strings.HasPrefix(m, "added"):
		return "create"
	}
	return "info"
}

// dayLabel heads a day's entries: Today, Yesterday, or the date.
func dayLabel(t, now time.Time) string {
	y1, m1, d1 := t.Date()
	y2, m2, d2 := now.Date()
	switch {
	case y1 == y2 && m1 == m2 && d1 == d2:
		return "Today"
	case t.Format("2006-01-02") == now.AddDate(0, 0, -1).Format("2006-01-02"):
		return "Yesterday"
	case y1 == y2:
		return t.Format("Monday 2 January")
	}
	return t.Format("2 January 2006")
}

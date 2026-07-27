package server

import (
	"html"
	"net/http"
	"strings"
)

// These endpoints return small HTML fragments (not full layout pages) for the
// lazily-loaded Logs/Env/Backups tabs on the project detail page. They mirror
// the full-page handlers in handlers_ops.go but emit just the inner content.
//
// All app-derived content (logs, env, backup names) is UNTRUSTED and escaped
// with html.EscapeString before it reaches the response — this is an injection
// boundary, so nothing here writes app content unescaped.

func writeFragment(w http.ResponseWriter, s string) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Write([]byte(s))
}

// handleAppLogsPartial returns the tail of an app's logs as a <pre>.
func (s *Server) handleAppLogsPartial(w http.ResponseWriter, r *http.Request) {
	app, _, ok := s.appAndProject(w, r)
	if !ok {
		return
	}
	logs, err := s.apps.Logs(app.ID, 300)
	if err != nil {
		logs = "could not read logs: " + err.Error()
	}
	writeFragment(w, `<pre class="logs">`+html.EscapeString(logs)+`</pre>`)
}

// handleAppEnvPartial returns a read-only preview of the app's .env as a <pre>.
func (s *Server) handleAppEnvPartial(w http.ResponseWriter, r *http.Request) {
	app, _, ok := s.appAndProject(w, r)
	if !ok {
		return
	}
	content, err := s.apps.ReadEnv(app.ID)
	if err != nil {
		content = "could not read .env: " + err.Error()
	}
	if strings.TrimSpace(content) == "" {
		content = "(empty)"
	}
	writeFragment(w, `<pre class="logs">`+html.EscapeString(content)+`</pre>`)
}

// handleAppBackupsPartial returns the backup list as table rows (read-only).
func (s *Server) handleAppBackupsPartial(w http.ResponseWriter, r *http.Request) {
	app, _, ok := s.appAndProject(w, r)
	if !ok {
		return
	}
	backups, err := s.apps.ListBackups(app.ID, s.backupsRoot())
	if err != nil {
		writeFragment(w, `<div class="muted small">could not list backups: `+html.EscapeString(err.Error())+`</div>`)
		return
	}
	if len(backups) == 0 {
		writeFragment(w, `<div class="muted small">No backups yet.</div>`)
		return
	}
	var b strings.Builder
	b.WriteString(`<table class="table">`)
	for _, name := range backups {
		b.WriteString(`<tr><td class="muted small">` + html.EscapeString(name) + `</td></tr>`)
	}
	b.WriteString(`</table>`)
	writeFragment(w, b.String())
}

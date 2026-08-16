package server

import (
	"context"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// handleDatabase renders the shared-MariaDB management page: status, per-app
// databases, and the restart/update/Adminer controls.
func (s *Server) handleDatabase(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 20*time.Second)
	defer cancel()
	info, err := s.apps.SharedDBInfo(ctx)
	errMsg := r.URL.Query().Get("err")
	if err != nil && errMsg == "" {
		errMsg = firstLine(err.Error())
	}
	usage, _ := s.store.LatestDBMetrics(time.Now().Add(-20 * time.Second))
	s.render(w, r, "database", viewData{
		"Title":     "Database · xdev",
		"DB":        info,
		"Stats":     s.apps.SharedDBStats(ctx),
		"Usage":     usage,
		"Users":     s.apps.SharedUsers(ctx),
		"Dedicated": s.apps.DedicatedDatabases(),
		"Msg":       r.URL.Query().Get("msg"),
		"Err":       errMsg,
	})
}

// handleDatabaseDetail renders one database's detail view (overview + tables).
func (s *Server) handleDatabaseDetail(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	ctx, cancel := context.WithTimeout(r.Context(), 20*time.Second)
	defer cancel()
	d, err := s.apps.SharedDatabaseDetail(ctx, name)
	if err != nil {
		s.redirectDB(w, r, "", firstLine(err.Error()))
		return
	}
	if !d.Found {
		s.redirectDB(w, r, "", "Database "+name+" not found.")
		return
	}
	s.render(w, r, "database_detail", viewData{
		"Title": name + " · Database · xdev",
		"D":     d,
	})
}

// dbAction runs one shared-DB admin action and redirects back with a flash.
func (s *Server) dbAction(w http.ResponseWriter, r *http.Request, okMsg string, fn func(context.Context) error) {
	ctx, cancel := context.WithTimeout(r.Context(), 120*time.Second)
	defer cancel()
	if err := fn(ctx); err != nil {
		s.redirectDB(w, r, "", firstLine(err.Error()))
		return
	}
	s.redirectDB(w, r, okMsg, "")
}

// handleDBCreate creates an empty database.
func (s *Server) handleDBCreate(w http.ResponseWriter, r *http.Request) {
	name := strings.TrimSpace(r.FormValue("name"))
	s.dbAction(w, r, "Database "+name+" created.", func(ctx context.Context) error {
		return s.apps.CreateSharedDatabase(ctx, name)
	})
}

// handleDBDrop drops a database.
func (s *Server) handleDBDrop(w http.ResponseWriter, r *http.Request) {
	name := strings.TrimSpace(r.FormValue("name"))
	s.dbAction(w, r, "Database "+name+" dropped.", func(ctx context.Context) error {
		return s.apps.DropSharedDatabase(ctx, name)
	})
}

// handleDBUserCreate creates a user, optionally granting it a database.
func (s *Server) handleDBUserCreate(w http.ResponseWriter, r *http.Request) {
	name := strings.TrimSpace(r.FormValue("name"))
	s.dbAction(w, r, "User "+name+" created.", func(ctx context.Context) error {
		return s.apps.CreateSharedUser(ctx, name, r.FormValue("password"), r.FormValue("grant"))
	})
}

// handleDBUserPassword resets a user's password.
func (s *Server) handleDBUserPassword(w http.ResponseWriter, r *http.Request) {
	name := strings.TrimSpace(r.FormValue("name"))
	s.dbAction(w, r, "Password reset for "+name+".", func(ctx context.Context) error {
		return s.apps.SetSharedUserPassword(ctx, name, r.FormValue("host"), r.FormValue("password"))
	})
}

// handleDBUserDrop removes a user.
func (s *Server) handleDBUserDrop(w http.ResponseWriter, r *http.Request) {
	name := strings.TrimSpace(r.FormValue("name"))
	s.dbAction(w, r, "User "+name+" dropped.", func(ctx context.Context) error {
		return s.apps.DropSharedUser(ctx, name, r.FormValue("host"))
	})
}

// handleDBRestart restarts (or first-time starts) the shared server.
func (s *Server) handleDBRestart(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 120*time.Second)
	defer cancel()
	if err := s.apps.SharedDBRestart(ctx); err != nil {
		s.redirectDB(w, r, "", firstLine(err.Error()))
		return
	}
	s.store.AddEvent(0, 0, "info", "Restarted shared MariaDB")
	s.redirectDB(w, r, "Shared MariaDB is up.", "")
}

// handleDBUpdate pulls the latest image and recreates the shared server.
func (s *Server) handleDBUpdate(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 300*time.Second)
	defer cancel()
	if err := s.apps.SharedDBUpdate(ctx); err != nil {
		s.redirectDB(w, r, "", firstLine(err.Error()))
		return
	}
	s.store.AddEvent(0, 0, "info", "Updated shared MariaDB image")
	s.redirectDB(w, r, "Shared MariaDB updated to the latest image.", "")
}

// handleAdminer starts or stops the Adminer helper (form field action=stop).
func (s *Server) handleAdminer(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 120*time.Second)
	defer cancel()
	if r.FormValue("action") == "stop" {
		if err := s.apps.AdminerStop(ctx); err != nil {
			s.redirectDB(w, r, "", firstLine(err.Error()))
			return
		}
		s.redirectDB(w, r, "Adminer stopped.", "")
		return
	}
	if _, err := s.apps.AdminerStart(ctx); err != nil {
		s.redirectDB(w, r, "", firstLine(err.Error()))
		return
	}
	s.redirectDB(w, r, "Adminer is running.", "")
}

// redirectDB returns to /database with a flash message.
func (s *Server) redirectDB(w http.ResponseWriter, r *http.Request, msg, errMsg string) {
	q := url.Values{}
	if msg != "" {
		q.Set("msg", msg)
	}
	if errMsg != "" {
		q.Set("err", errMsg)
	}
	dest := "/database"
	if e := q.Encode(); e != "" {
		dest += "?" + e
	}
	http.Redirect(w, r, dest, http.StatusSeeOther)
}

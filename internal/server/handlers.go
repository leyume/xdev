package server

import (
	"net/http"

	"xdev/internal/metrics"
)

// handleSetupForm shows the first-run admin creation form. If an admin already
// exists, there's nothing to set up — send the user to login.
func (s *Server) handleSetupForm(w http.ResponseWriter, r *http.Request) {
	need, err := s.auth.NeedsSetup()
	if err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	if !need {
		http.Redirect(w, r, "/login", http.StatusSeeOther)
		return
	}
	s.render(w, r, "setup", viewData{"Title": "Set up xdev"})
}

// handleSetupSubmit creates the admin account and logs them straight in.
func (s *Server) handleSetupSubmit(w http.ResponseWriter, r *http.Request) {
	email := r.FormValue("email")
	password := r.FormValue("password")

	user, err := s.auth.CreateAdmin(email, password)
	if err != nil {
		s.render(w, r, "setup", viewData{
			"Title": "Set up xdev",
			"Error": err.Error(),
			"Email": email,
		})
		return
	}
	if _, err := s.auth.StartSession(w, user.ID); err != nil {
		http.Error(w, "could not start session", http.StatusInternalServerError)
		return
	}
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

// handleLoginForm shows the login page. Redirects to setup on first run, or to
// the dashboard if already authenticated.
func (s *Server) handleLoginForm(w http.ResponseWriter, r *http.Request) {
	if need, _ := s.auth.NeedsSetup(); need {
		http.Redirect(w, r, "/setup", http.StatusSeeOther)
		return
	}
	if _, ok := s.currentUser(r); ok {
		http.Redirect(w, r, "/", http.StatusSeeOther)
		return
	}
	s.render(w, r, "login", viewData{"Title": "Sign in · xdev"})
}

// handleLoginSubmit verifies credentials and starts a session.
func (s *Server) handleLoginSubmit(w http.ResponseWriter, r *http.Request) {
	email := r.FormValue("email")
	password := r.FormValue("password")

	user, err := s.auth.Authenticate(email, password)
	if err != nil {
		s.render(w, r, "login", viewData{
			"Title": "Sign in · xdev",
			"Error": "Invalid email or password.",
			"Email": email,
		})
		return
	}
	if _, err := s.auth.StartSession(w, user.ID); err != nil {
		http.Error(w, "could not start session", http.StatusInternalServerError)
		return
	}
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

// handleLogout ends the session and returns to the login page.
func (s *Server) handleLogout(w http.ResponseWriter, r *http.Request) {
	s.auth.EndSession(w, r)
	http.Redirect(w, r, "/login", http.StatusSeeOther)
}

// handleDashboard renders the Projects list (empty state in Phase 0) plus the
// detected container-runtime status.
func (s *Server) handleDashboard(w http.ResponseWriter, r *http.Request) {
	projects, err := s.store.ListProjects()
	if err != nil {
		http.Error(w, "could not load projects", http.StatusInternalServerError)
		return
	}
	s.render(w, r, "dashboard", viewData{
		"Title":    "Projects · xdev",
		"Projects": projects,
		"Runtime":  s.engine.Info(),
		"Engine":   string(s.engine.Current()),
		"Host":     metrics.HostSnapshot(),
		"EngineMsg": r.URL.Query().Get("engine_msg"),
	})
}

// currentUser is a small helper to check whether a request is already
// authenticated, without going through RequireAuth (used on the login page to
// redirect already-signed-in visitors away).
func (s *Server) currentUser(r *http.Request) (any, bool) {
	c, err := r.Cookie("xdev_session")
	if err != nil {
		return nil, false
	}
	sess, err := s.store.SessionByToken(c.Value)
	if err != nil {
		return nil, false
	}
	u, err := s.store.UserByID(sess.UserID)
	if err != nil {
		return nil, false
	}
	return u, true
}

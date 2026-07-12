package server

import (
	"net/http"
	"time"

	"xdev/internal/metrics"
	"xdev/internal/store"
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

// appRow is one row in a cross-project apps listing: an app plus the project
// it belongs to and (if available) its most recent resource sample.
type appRow struct {
	store.App
	ProjectSlug string
	ProjectName string
	HasMetrics  bool
	CPUPct      float64
	MemMiB      int64
}

// allAppRows loads every app across every project, newest metric sample
// attached where one exists within the last 20s (roughly one collector tick).
func (s *Server) allAppRows() ([]appRow, error) {
	projects, err := s.store.ListProjects()
	if err != nil {
		return nil, err
	}
	since := time.Now().Add(-20 * time.Second)
	var out []appRow
	for _, p := range projects {
		apps, err := s.store.ListAppsByProject(p.ID)
		if err != nil {
			return nil, err
		}
		for _, a := range apps {
			row := appRow{App: a, ProjectSlug: p.Slug, ProjectName: p.Name}
			if samples, err := s.store.RecentMetrics(a.ID, since); err == nil && len(samples) > 0 {
				last := samples[len(samples)-1]
				row.HasMetrics = true
				row.CPUPct = last.CPUPct
				row.MemMiB = last.MemBytes / 1024 / 1024
			}
			out = append(out, row)
		}
	}
	return out, nil
}

// handleHomeDashboard renders the home Dashboard: KPI overview, host
// resources, apps needing attention, and recent activity across every
// project. Some KPI cells (containers/certificates/backups counts, the
// utilization chart) aren't backed by real aggregates yet — see PLANx.md's
// feature list.
func (s *Server) handleHomeDashboard(w http.ResponseWriter, r *http.Request) {
	projects, err := s.store.ListProjects()
	if err != nil {
		http.Error(w, "could not load projects", http.StatusInternalServerError)
		return
	}
	appRows, err := s.allAppRows()
	if err != nil {
		http.Error(w, "could not load apps", http.StatusInternalServerError)
		return
	}
	totalApps, running := 0, 0
	var firstStopped string
	for _, a := range appRows {
		totalApps++
		if a.Status == store.AppRunning {
			running++
		} else if firstStopped == "" {
			firstStopped = a.ProjectSlug + "/" + a.Slug
		}
	}
	events, _ := s.store.ListEvents(6)
	s.render(w, r, "dashboard", viewData{
		"Title":        "Dashboard · xdev",
		"Projects":     projects,
		"Apps":         appRows,
		"TotalApps":    totalApps,
		"Running":      running,
		"Stopped":      totalApps - running,
		"FirstStopped": firstStopped,
		"Events":       events,
		"Runtime":      s.engine.Info(),
		"Engine":       string(s.engine.Current()),
		"Host":         metrics.HostSnapshot(),
		"EngineMsg":    r.URL.Query().Get("engine_msg"),
	})
}

// handleProjectsList renders the Projects overview: host resources, engine
// status, and one card per project with its apps.
func (s *Server) handleProjectsList(w http.ResponseWriter, r *http.Request) {
	projects, err := s.store.ListProjects()
	if err != nil {
		http.Error(w, "could not load projects", http.StatusInternalServerError)
		return
	}
	type projectCard struct {
		store.Project
		Apps    []store.App
		Stopped int
	}
	cards := make([]projectCard, 0, len(projects))
	totalApps, running := 0, 0
	for _, p := range projects {
		apps, err := s.store.ListAppsByProject(p.ID)
		if err != nil {
			http.Error(w, "could not load apps", http.StatusInternalServerError)
			return
		}
		stopped := 0
		for _, a := range apps {
			totalApps++
			if a.Status == store.AppRunning {
				running++
			} else {
				stopped++
			}
		}
		cards = append(cards, projectCard{Project: p, Apps: apps, Stopped: stopped})
	}
	events, _ := s.store.ListEvents(8)
	s.render(w, r, "projects", viewData{
		"Title":     "Projects · xdev",
		"Projects":  cards,
		"TotalApps": totalApps,
		"Running":   running,
		"Events":    events,
		"Runtime":   s.engine.Info(),
		"Engine":    string(s.engine.Current()),
		"Host":      metrics.HostSnapshot(),
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

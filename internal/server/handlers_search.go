package server

import (
	"encoding/json"
	"net/http"
	"strings"
)

// searchResult is one topbar-search hit: a project or an app deep-linked to its
// project page (there is no per-app page).
type searchResult struct {
	Label string `json:"label"`
	Sub   string `json:"sub"`
	URL   string `json:"url"`
}

// handleSearch serves GET /search?q= as JSON: projects and apps whose name or
// slug contains q (case-insensitive), capped at ~10.
//
// ponytail: in-memory filter over the already-cheap project/app lists — no SQL
// LIKE plumbing. Fine at this scale (tens of projects); revisit if listings grow.
func (s *Server) handleSearch(w http.ResponseWriter, r *http.Request) {
	q := strings.ToLower(strings.TrimSpace(r.URL.Query().Get("q")))
	out := []searchResult{}
	if q == "" {
		writeJSON(w, out)
		return
	}
	projects, err := s.store.ListProjects()
	if err != nil {
		http.Error(w, "could not load projects", http.StatusInternalServerError)
		return
	}
	const max = 10
	for _, p := range projects {
		if len(out) >= max {
			break
		}
		if strings.Contains(strings.ToLower(p.Name), q) || strings.Contains(strings.ToLower(p.Slug), q) {
			out = append(out, searchResult{Label: p.Name, Sub: "Project", URL: "/projects/" + p.Slug})
		}
		apps, err := s.store.ListAppsByProject(p.ID)
		if err != nil {
			continue
		}
		for _, a := range apps {
			if len(out) >= max {
				break
			}
			if strings.Contains(strings.ToLower(a.Name), q) || strings.Contains(strings.ToLower(a.Slug), q) {
				out = append(out, searchResult{Label: a.Name, Sub: "App · " + p.Name, URL: "/projects/" + p.Slug})
			}
		}
	}
	writeJSON(w, out)
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(v)
}

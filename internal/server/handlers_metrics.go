package server

import (
	"encoding/json"
	"net/http"
	"strconv"
	"time"
)

// handleAppMetrics renders the per-app metrics chart page.
func (s *Server) handleAppMetrics(w http.ResponseWriter, r *http.Request) {
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
	s.render(w, r, "app_metrics", viewData{
		"Title":   app.Name + " metrics · xdev",
		"App":     app,
		"Project": proj,
	})
}

// handleAppMetricsJSON returns recent samples for the chart as parallel arrays:
// t (unix seconds), cpu (percent), mem (MiB).
func (s *Server) handleAppMetricsJSON(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		http.Error(w, "bad app id", http.StatusBadRequest)
		return
	}
	rows, err := s.store.RecentMetrics(id, time.Now().Add(-3*time.Hour))
	if err != nil {
		http.Error(w, "could not load metrics", http.StatusInternalServerError)
		return
	}
	var payload struct {
		T   []int64   `json:"t"`
		CPU []float64 `json:"cpu"`
		Mem []float64 `json:"mem"`
	}
	for _, m := range rows {
		ts, err := time.Parse(time.RFC3339, m.TS)
		if err != nil {
			continue
		}
		payload.T = append(payload.T, ts.Unix())
		payload.CPU = append(payload.CPU, m.CPUPct)
		payload.Mem = append(payload.Mem, float64(m.MemBytes)/1024/1024)
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(payload)
}

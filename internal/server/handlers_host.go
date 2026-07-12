package server

import (
	"encoding/json"
	"net/http"
	"time"
)

// handleHostMetricsJSON returns recent host samples for the dashboard chart as
// parallel arrays: t (unix seconds), cpu (percent), mem (percent). The range
// query param is 1h|24h|7d (default 24h); 7d is capped by the collector's 24h
// retention today, so it just returns whatever exists.
func (s *Server) handleHostMetricsJSON(w http.ResponseWriter, r *http.Request) {
	dur := 24 * time.Hour
	switch r.URL.Query().Get("range") {
	case "1h":
		dur = time.Hour
	case "7d":
		dur = 7 * 24 * time.Hour
	}
	rows, err := s.store.RecentHostMetrics(time.Now().Add(-dur))
	if err != nil {
		http.Error(w, "could not load host metrics", http.StatusInternalServerError)
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
		payload.Mem = append(payload.Mem, m.MemPct)
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(payload)
}

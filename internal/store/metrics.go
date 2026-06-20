package store

import "time"

// Metric is one time-series sample for an app (aggregated across its containers).
type Metric struct {
	TS       string  // RFC3339
	CPUPct   float64 // total CPU percent across the app's containers
	MemBytes int64   // total memory in use
	MemLimit int64   // configured memory limit (0 = unlimited)
}

// AppPrefix maps an app id to the container-name prefix its services share
// (<project-slug>_<app-slug>), used to attribute `stats` rows to apps.
type AppPrefix struct {
	ID       int64
	Prefix   string
	MemLimit int64
}

// InsertMetric records one sample.
func (s *Store) InsertMetric(appID int64, ts time.Time, cpu float64, mem, memLimit int64) error {
	_, err := s.db.Exec(
		`INSERT INTO metrics (app_id, ts, cpu_pct, mem_bytes, mem_limit) VALUES (?, ?, ?, ?, ?)`,
		appID, ts.UTC().Format(time.RFC3339), cpu, mem, memLimit)
	return err
}

// RecentMetrics returns an app's samples at or after since, oldest first.
func (s *Store) RecentMetrics(appID int64, since time.Time) ([]Metric, error) {
	rows, err := s.db.Query(
		`SELECT ts, cpu_pct, mem_bytes, mem_limit FROM metrics
		 WHERE app_id = ? AND ts >= ? ORDER BY ts ASC`,
		appID, since.UTC().Format(time.RFC3339))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Metric
	for rows.Next() {
		var m Metric
		if err := rows.Scan(&m.TS, &m.CPUPct, &m.MemBytes, &m.MemLimit); err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

// PruneMetricsBefore deletes samples older than t (retention).
func (s *Store) PruneMetricsBefore(t time.Time) error {
	_, err := s.db.Exec(`DELETE FROM metrics WHERE ts < ?`, t.UTC().Format(time.RFC3339))
	return err
}

// AppPrefixes lists every app's container-name prefix for metric attribution.
func (s *Store) AppPrefixes() ([]AppPrefix, error) {
	rows, err := s.db.Query(
		`SELECT a.id, p.slug || '_' || a.slug, a.mem_limit
		 FROM apps a JOIN projects p ON p.id = a.project_id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []AppPrefix
	for rows.Next() {
		var ap AppPrefix
		if err := rows.Scan(&ap.ID, &ap.Prefix, &ap.MemLimit); err != nil {
			return nil, err
		}
		out = append(out, ap)
	}
	return out, rows.Err()
}

package store

import "time"

// HostMetric is one host-level resource sample (whole-machine CPU/memory).
type HostMetric struct {
	TS     string  // RFC3339
	CPUPct float64 // overall host CPU percent
	MemPct float64 // overall host memory percent
}

// InsertHostMetric records one host sample.
func (s *Store) InsertHostMetric(ts time.Time, cpu, mem float64) error {
	_, err := s.db.Exec(
		`INSERT INTO host_metrics (ts, cpu_pct, mem_pct) VALUES (?, ?, ?)`,
		ts.UTC().Format(time.RFC3339), cpu, mem)
	return err
}

// RecentHostMetrics returns host samples at or after since, oldest first.
func (s *Store) RecentHostMetrics(since time.Time) ([]HostMetric, error) {
	rows, err := s.db.Query(
		`SELECT ts, cpu_pct, mem_pct FROM host_metrics
		 WHERE ts >= ? ORDER BY ts ASC`,
		since.UTC().Format(time.RFC3339))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []HostMetric
	for rows.Next() {
		var m HostMetric
		if err := rows.Scan(&m.TS, &m.CPUPct, &m.MemPct); err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

// PruneHostMetricsBefore deletes host samples older than t (retention).
func (s *Store) PruneHostMetricsBefore(t time.Time) error {
	_, err := s.db.Exec(`DELETE FROM host_metrics WHERE ts < ?`, t.UTC().Format(time.RFC3339))
	return err
}

// CountTLSDomains counts hostnames served over TLS (every domain gets a cert
// from Caddy, internal-CA or ACME), used for the dashboard Certificates KPI.
func (s *Store) CountTLSDomains() (int, error) {
	var n int
	err := s.db.QueryRow(`SELECT COUNT(*) FROM domains WHERE ssl_mode != ''`).Scan(&n)
	return n, err
}

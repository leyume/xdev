package store

import "time"

// DBMetric is one resource sample for a database container.
type DBMetric struct {
	Container string
	TS        string  // RFC3339
	CPUPct    float64 // container CPU percent
	MemBytes  int64   // container memory in use
}

// InsertDBMetric records one database-container sample.
func (s *Store) InsertDBMetric(container string, ts time.Time, cpu float64, mem int64) error {
	_, err := s.db.Exec(
		`INSERT INTO db_metrics (container, ts, cpu_pct, mem_bytes) VALUES (?, ?, ?, ?)`,
		container, ts.UTC().Format(time.RFC3339), cpu, mem)
	return err
}

// RecentDBMetrics returns one container's samples at or after since, oldest
// first — the series behind a chart.
func (s *Store) RecentDBMetrics(container string, since time.Time) ([]DBMetric, error) {
	rows, err := s.db.Query(
		`SELECT container, ts, cpu_pct, mem_bytes FROM db_metrics
		 WHERE container = ? AND ts >= ? ORDER BY ts ASC`,
		container, since.UTC().Format(time.RFC3339))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanDBMetrics(rows)
}

// LatestDBMetrics returns the newest sample per container within since, keyed
// by container name. One query for the whole page, rather than one per row.
func (s *Store) LatestDBMetrics(since time.Time) (map[string]DBMetric, error) {
	rows, err := s.db.Query(
		`SELECT container, ts, cpu_pct, mem_bytes FROM db_metrics
		 WHERE ts >= ? ORDER BY container ASC, ts ASC`,
		since.UTC().Format(time.RFC3339))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	list, err := scanDBMetrics(rows)
	if err != nil {
		return nil, err
	}
	// Ordered oldest-first per container, so the last write for each key wins.
	out := make(map[string]DBMetric, len(list))
	for _, m := range list {
		out[m.Container] = m
	}
	return out, nil
}

func scanDBMetrics(rows interface {
	Next() bool
	Scan(...any) error
	Err() error
}) ([]DBMetric, error) {
	var out []DBMetric
	for rows.Next() {
		var m DBMetric
		if err := rows.Scan(&m.Container, &m.TS, &m.CPUPct, &m.MemBytes); err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

// PruneDBMetricsBefore deletes database samples older than t (retention).
func (s *Store) PruneDBMetricsBefore(t time.Time) error {
	_, err := s.db.Exec(`DELETE FROM db_metrics WHERE ts < ?`, t.UTC().Format(time.RFC3339))
	return err
}

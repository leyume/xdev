package store

import "database/sql"

// Event is one entry in the audit log.
type Event struct {
	TS      string
	Level   string
	Message string
}

// AddEvent appends an audit-log entry. projectID/appID of 0 are stored as NULL.
func (s *Store) AddEvent(projectID, appID int64, level, message string) error {
	var pid, aid any
	if projectID > 0 {
		pid = projectID
	}
	if appID > 0 {
		aid = appID
	}
	_, err := s.db.Exec(
		`INSERT INTO events (project_id, app_id, level, message) VALUES (?, ?, ?, ?)`,
		pid, aid, level, message)
	return err
}

// ListEvents returns the most recent events, newest first.
func (s *Store) ListEvents(limit int) ([]Event, error) {
	rows, err := s.db.Query(
		`SELECT ts, level, message FROM events ORDER BY id DESC LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Event
	for rows.Next() {
		var e Event
		var level sql.NullString
		if err := rows.Scan(&e.TS, &level, &e.Message); err != nil {
			return nil, err
		}
		e.Level = level.String
		out = append(out, e)
	}
	return out, rows.Err()
}

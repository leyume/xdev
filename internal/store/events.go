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
	return scanEvents(s.db.Query(
		`SELECT ts, level, message FROM events ORDER BY id DESC LIMIT ?`, limit))
}

// ListEventsByProject returns the most recent events for one project, newest first.
func (s *Store) ListEventsByProject(projectID int64, limit int) ([]Event, error) {
	return scanEvents(s.db.Query(
		`SELECT ts, level, message FROM events WHERE project_id = ? ORDER BY id DESC LIMIT ?`,
		projectID, limit))
}

// ClearEvents deletes the activity log — every row, or one project's when
// projectID is non-zero. Returns how many rows went, which is what the page
// says afterwards: "cleared" with no number reads the same whether it deleted
// two hundred rows or silently matched none.
//
// The log is a record of what happened, not state anything reads back, so
// clearing it is safe in a way that clearing most tables is not. Rows for a
// deleted project are only reachable through the all-events view, so an
// unscoped clear is the only thing that can ever remove them.
func (s *Store) ClearEvents(projectID int64) (int64, error) {
	query, args := `DELETE FROM events`, []any(nil)
	if projectID > 0 {
		query += ` WHERE project_id = ?`
		args = append(args, projectID)
	}
	res, err := s.db.Exec(query, args...)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

func scanEvents(rows *sql.Rows, err error) ([]Event, error) {
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

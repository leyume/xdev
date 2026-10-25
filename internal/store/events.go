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

// Activity is one event with the names it belongs to, for the activity page.
// ProjectName/AppName are "" when the event had none, or its project or app
// has since been deleted.
type Activity struct {
	TS, Level, Message                string
	ProjectID, AppID                  int64
	ProjectName, ProjectSlug, AppName string
}

// SystemProject selects events that belong to no project (the shared
// database, the WordPress pool, a full clear) in ActivityPage and the counts.
const SystemProject int64 = -1

// ActivityPage returns one page of events, newest first, and how many match
// in all. projectID 0 means every project, SystemProject the project-less
// ones; appID 0 means every app.
func (s *Store) ActivityPage(projectID, appID int64, limit, offset int) ([]Activity, int, error) {
	where, args := `1=1`, []any{}
	switch {
	case projectID == SystemProject:
		where += ` AND e.project_id IS NULL`
	case projectID > 0:
		where += ` AND e.project_id = ?`
		args = append(args, projectID)
	}
	if appID > 0 {
		where += ` AND e.app_id = ?`
		args = append(args, appID)
	}
	var total int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM events e WHERE `+where, args...).Scan(&total); err != nil {
		return nil, 0, err
	}
	rows, err := s.db.Query(`SELECT e.ts, COALESCE(e.level,''), e.message,
			COALESCE(e.project_id,0), COALESCE(e.app_id,0),
			COALESCE(p.name,''), COALESCE(p.slug,''), COALESCE(a.name,'')
		FROM events e
		LEFT JOIN projects p ON p.id = e.project_id
		LEFT JOIN apps a ON a.id = e.app_id
		WHERE `+where+` ORDER BY e.id DESC LIMIT ? OFFSET ?`, append(args, limit, offset)...)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()
	var out []Activity
	for rows.Next() {
		var a Activity
		if err := rows.Scan(&a.TS, &a.Level, &a.Message, &a.ProjectID, &a.AppID,
			&a.ProjectName, &a.ProjectSlug, &a.AppName); err != nil {
			return nil, 0, err
		}
		out = append(out, a)
	}
	return out, total, rows.Err()
}

// ActivityCounts counts events per project (SystemProject for project-less
// ones) and per app, for the activity page's tabs.
func (s *Store) ActivityCounts() (byProject, byApp map[int64]int, err error) {
	byProject, byApp = map[int64]int{}, map[int64]int{}
	rows, err := s.db.Query(`SELECT COALESCE(project_id, ?), COALESCE(app_id, 0), COUNT(*)
		FROM events GROUP BY project_id, app_id`, SystemProject)
	if err != nil {
		return nil, nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var pid, aid int64
		var n int
		if err := rows.Scan(&pid, &aid, &n); err != nil {
			return nil, nil, err
		}
		byProject[pid] += n
		if aid > 0 {
			byApp[aid] += n
		}
	}
	return byProject, byApp, rows.Err()
}

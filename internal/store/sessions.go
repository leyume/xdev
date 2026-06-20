package store

import (
	"database/sql"
	"errors"
	"time"
)

// Session is a server-side login session keyed by an opaque cookie token.
type Session struct {
	Token     string
	UserID    int64
	CSRFToken string
	ExpiresAt time.Time
}

// CreateSession stores a new session row.
func (s *Store) CreateSession(token string, userID int64, csrf string, expires time.Time) error {
	_, err := s.db.Exec(
		`INSERT INTO sessions (token, user_id, csrf_token, expires_at) VALUES (?, ?, ?, ?)`,
		token, userID, csrf, expires.UTC().Format(time.RFC3339),
	)
	return err
}

// SessionByToken returns a non-expired session, or ErrNotFound.
func (s *Store) SessionByToken(token string) (Session, error) {
	var sess Session
	var expires string
	err := s.db.QueryRow(
		`SELECT token, user_id, csrf_token, expires_at FROM sessions WHERE token = ?`, token,
	).Scan(&sess.Token, &sess.UserID, &sess.CSRFToken, &expires)
	if errors.Is(err, sql.ErrNoRows) {
		return Session{}, ErrNotFound
	}
	if err != nil {
		return Session{}, err
	}
	sess.ExpiresAt, _ = time.Parse(time.RFC3339, expires)
	if time.Now().After(sess.ExpiresAt) {
		// Expired: clean it up and report as missing.
		_ = s.DeleteSession(token)
		return Session{}, ErrNotFound
	}
	return sess, nil
}

// DeleteSession removes a session (logout).
func (s *Store) DeleteSession(token string) error {
	_, err := s.db.Exec(`DELETE FROM sessions WHERE token = ?`, token)
	return err
}

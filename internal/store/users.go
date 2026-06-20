package store

import (
	"database/sql"
	"errors"
)

// User is an account that can log into xdev. v1 has exactly one (the admin).
type User struct {
	ID           int64
	Email        string
	PasswordHash string
	CreatedAt    string
}

// ErrNotFound is returned by lookups when no row matches.
var ErrNotFound = errors.New("not found")

// UserCount returns how many users exist. Used to decide whether the
// first-run admin-setup flow should be shown.
func (s *Store) UserCount() (int, error) {
	var n int
	err := s.db.QueryRow(`SELECT COUNT(*) FROM users`).Scan(&n)
	return n, err
}

// CreateUser inserts a new user with an already-hashed password.
func (s *Store) CreateUser(email, passwordHash string) (User, error) {
	res, err := s.db.Exec(
		`INSERT INTO users (email, password_hash) VALUES (?, ?)`,
		email, passwordHash,
	)
	if err != nil {
		return User{}, err
	}
	id, _ := res.LastInsertId()
	return s.UserByID(id)
}

// UserByEmail looks up a user for login.
func (s *Store) UserByEmail(email string) (User, error) {
	return s.scanUser(s.db.QueryRow(
		`SELECT id, email, password_hash, created_at FROM users WHERE email = ?`, email))
}

// ListUsers returns all accounts (admins), oldest first.
func (s *Store) ListUsers() ([]User, error) {
	rows, err := s.db.Query(`SELECT id, email, password_hash, created_at FROM users ORDER BY created_at, id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var users []User
	for rows.Next() {
		var u User
		if err := rows.Scan(&u.ID, &u.Email, &u.PasswordHash, &u.CreatedAt); err != nil {
			return nil, err
		}
		users = append(users, u)
	}
	return users, rows.Err()
}

// UpdatePassword sets a new (already-hashed) password for the user with email.
// Returns ErrNotFound if no such user exists.
func (s *Store) UpdatePassword(email, passwordHash string) error {
	res, err := s.db.Exec(`UPDATE users SET password_hash = ? WHERE email = ?`, passwordHash, email)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// DeleteUser removes an account by id. Their sessions cascade away (FK ON DELETE
// CASCADE), logging them out everywhere. Returns ErrNotFound if no row matched.
func (s *Store) DeleteUser(id int64) error {
	res, err := s.db.Exec(`DELETE FROM users WHERE id = ?`, id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// UserByID looks up a user by id (used to resolve the current session's user).
func (s *Store) UserByID(id int64) (User, error) {
	return s.scanUser(s.db.QueryRow(
		`SELECT id, email, password_hash, created_at FROM users WHERE id = ?`, id))
}

func (s *Store) scanUser(row *sql.Row) (User, error) {
	var u User
	err := row.Scan(&u.ID, &u.Email, &u.PasswordHash, &u.CreatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return User{}, ErrNotFound
	}
	return u, err
}

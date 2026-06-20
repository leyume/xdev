// Package store owns the sqlite database: opening it, running embedded
// migrations, and providing small, readable query helpers. Everything in xdev
// persists here — users, sessions, projects, apps, metrics, and settings.
//
// We use modernc.org/sqlite, a pure-Go driver, so xdev cross-compiles to a
// Linux server with no C toolchain (CGO_ENABLED=0).
package store

import (
	"database/sql"
	"embed"
	"fmt"
	"sort"

	_ "modernc.org/sqlite"
)

//go:embed migrations/*.sql
var migrationsFS embed.FS

// Store wraps the database handle. Query helpers hang off this type.
type Store struct {
	db *sql.DB
}

// Open opens (creating if needed) the sqlite database at path and applies any
// pending migrations.
func Open(path string) (*Store, error) {
	// _pragma options: WAL for better concurrency, foreign_keys to enforce the
	// ON DELETE CASCADE relationships in the schema, busy_timeout to avoid
	// "database is locked" under brief contention.
	dsn := path + "?_pragma=journal_mode(WAL)&_pragma=foreign_keys(ON)&_pragma=busy_timeout(5000)"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("open sqlite: %w", err)
	}
	if err := db.Ping(); err != nil {
		return nil, fmt.Errorf("ping sqlite: %w", err)
	}

	s := &Store{db: db}
	if err := s.migrate(); err != nil {
		db.Close()
		return nil, err
	}
	return s, nil
}

// Close releases the database handle.
func (s *Store) Close() error { return s.db.Close() }

// migrate applies every embedded migration file that hasn't run yet, in
// filename order, recording each in schema_migrations.
func (s *Store) migrate() error {
	if _, err := s.db.Exec(`CREATE TABLE IF NOT EXISTS schema_migrations (
		name TEXT PRIMARY KEY,
		applied_at TEXT NOT NULL DEFAULT (datetime('now'))
	)`); err != nil {
		return fmt.Errorf("create schema_migrations: %w", err)
	}

	entries, err := migrationsFS.ReadDir("migrations")
	if err != nil {
		return err
	}
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		if !e.IsDir() {
			names = append(names, e.Name())
		}
	}
	sort.Strings(names)

	for _, name := range names {
		var exists bool
		if err := s.db.QueryRow(`SELECT 1 FROM schema_migrations WHERE name = ?`, name).Scan(&exists); err == nil {
			continue // already applied
		} else if err != sql.ErrNoRows {
			return fmt.Errorf("check migration %s: %w", name, err)
		}

		sqlBytes, err := migrationsFS.ReadFile("migrations/" + name)
		if err != nil {
			return err
		}
		// Each migration runs in its own transaction so a failure leaves the
		// database in a clean, half-unapplied-free state.
		tx, err := s.db.Begin()
		if err != nil {
			return err
		}
		if _, err := tx.Exec(string(sqlBytes)); err != nil {
			tx.Rollback()
			return fmt.Errorf("apply migration %s: %w", name, err)
		}
		if _, err := tx.Exec(`INSERT INTO schema_migrations (name) VALUES (?)`, name); err != nil {
			tx.Rollback()
			return fmt.Errorf("record migration %s: %w", name, err)
		}
		if err := tx.Commit(); err != nil {
			return err
		}
	}
	return nil
}

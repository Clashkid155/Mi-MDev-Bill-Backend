// Package platform is what every part of the backend shares, so that no feature has to depend on
// another: opening the database, admin tokens and the guard that checks them, the audit log of
// what admins changed, events (what happened, redacted, for the admin app), and who is calling.
// The bills package and the notifications package both build on it; it depends on neither.
package platform

import (
	"database/sql"
	_ "embed"
)

//go:embed schema.sql
var schema string

// Store holds the shared tables: admin tokens, the audit log and events.
type Store struct{ db *sql.DB }

// NewStore creates the shared tables (if they are not there) in db.
func NewStore(db *sql.DB) (*Store, error) {
	if _, err := db.Exec(schema); err != nil {
		return nil, err
	}
	return &Store{db: db}, nil
}

// DB is the shared database, for the other packages' own tables.
func (s *Store) DB() *sql.DB { return s.db }

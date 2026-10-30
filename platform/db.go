package platform

import (
	"database/sql"
	"errors"
	"fmt"
	"os"
	"strings"

	_ "modernc.org/sqlite"
)

// OpenDB opens the one SQLite database every part of the backend shares: owner-only file, WAL,
// foreign keys on, a single connection (SQLite allows one writer; this avoids "database is
// locked"), and a refusal to run on a damaged file. Each package then adds its own tables.
func OpenDB(path string) (*sql.DB, error) {
	if err := KeepPrivate(path); err != nil {
		return nil, err
	}
	// foreign_keys: nothing refers to anything yet, but a reference added later
	// should be enforced from its first day.
	db, err := sql.Open("sqlite", path+"?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)&_pragma=foreign_keys(1)")
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	// Refuse to run on a damaged file: better a stopped server than silently wrong money.
	var check string
	if err := db.QueryRow(`PRAGMA quick_check`).Scan(&check); err != nil || check != "ok" {
		db.Close()
		return nil, fmt.Errorf("the database file %s is damaged (restore the latest backup): %v %s", path, err, check)
	}
	// SQLite gives the -wal and -shm files the main file's permissions.
	_ = KeepPrivate(path)
	return db, nil
}

// KeepPrivate makes the database file readable by its owner only: it holds customers' emails and
// phone numbers and the electricity tokens we sold. A missing file is created empty with those
// permissions first, so there is never a moment when it is open to others.
func KeepPrivate(path string) error {
	if path == "" || path == ":memory:" || strings.HasPrefix(path, "file:") {
		return nil
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return err
	}
	f.Close()
	if err := os.Chmod(path, 0o600); err != nil {
		return err
	}
	for _, suffix := range []string{"-wal", "-shm"} {
		if err := os.Chmod(path+suffix, 0o600); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
	}
	return nil
}

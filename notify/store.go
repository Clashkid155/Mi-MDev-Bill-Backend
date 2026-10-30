// Package notify is the backend's in-app notices: maintenance announcements, news, and a way to
// tell an old app version to update. An app asks for the notices that apply to it when it opens;
// an admin writes them. A notice can also stop new orders for a while (maintenance), which the
// bills code asks about through platform.Gate, so it does not depend on this package.
package notify

import (
	"context"
	"database/sql"
	_ "embed"
	"errors"
	"strings"
	"time"
)

//go:embed schema.sql
var schema string

// Store holds the notices.
type Store struct{ db *sql.DB }

// NewStore creates the notices table (if it is not there) in db.
func NewStore(db *sql.DB) (*Store, error) {
	if _, err := db.Exec(schema); err != nil {
		return nil, err
	}
	return &Store{db: db}, nil
}

var (
	// ErrNotFound means there is no notice with that id.
	ErrNotFound = errors.New("notify: no such notice")
	// ErrEnded means the notice was already ended, so it cannot be changed or ended again.
	ErrEnded = errors.New("notify: that notice has already ended")
)

const columns = `id, kind, lang, title, body, link_url, link_label, starts_at, ends_at, ended_at, dismissible, blocking,
	block_from, block_until, apps, platforms, min_version, max_version, created_by, created_at, updated_at`

func scan(row interface{ Scan(...any) error }) (Notice, error) {
	var n Notice
	var apps, platforms string
	var dismissible, blocking int
	err := row.Scan(&n.ID, &n.Kind, &n.Lang, &n.Title, &n.Body, &n.LinkURL, &n.LinkLabel, &n.StartsAt, &n.EndsAt, &n.EndedAt,
		&dismissible, &blocking, &n.BlockFrom, &n.BlockUntil, &apps, &platforms, &n.MinVersion, &n.MaxVersion,
		&n.CreatedBy, &n.CreatedAt, &n.UpdatedAt)
	n.Dismissible, n.Blocking = dismissible != 0, blocking != 0
	n.Apps, n.Platforms = splitList(apps), splitList(platforms)
	return n, err
}

func splitList(s string) []string {
	if s == "" {
		return []string{}
	}
	return strings.Split(s, ",")
}

func b2i(b bool) int {
	if b {
		return 1
	}
	return 0
}

// Get returns one notice.
func (s *Store) Get(ctx context.Context, id int64) (Notice, error) {
	n, err := scan(s.db.QueryRowContext(ctx, `SELECT `+columns+` FROM notices WHERE id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return Notice{}, ErrNotFound
	}
	return n, err
}

// Create saves a validated notice.
func (s *Store) Create(ctx context.Context, n Notice, now time.Time) (Notice, error) {
	res, err := s.db.ExecContext(ctx, `INSERT INTO notices (kind, lang, title, body, link_url, link_label, starts_at, ends_at, ended_at, dismissible, blocking,
		block_from, block_until, apps, platforms, min_version, max_version, created_by, created_at, updated_at)
		VALUES (?,?,?,?,?,?,?,?,0,?,?,?,?,?,?,?,?,?,?,?)`,
		n.Kind, n.Lang, n.Title, n.Body, n.LinkURL, n.LinkLabel, n.StartsAt, n.EndsAt, b2i(n.Dismissible), b2i(n.Blocking),
		n.BlockFrom, n.BlockUntil, strings.Join(n.Apps, ","), strings.Join(n.Platforms, ","), n.MinVersion, n.MaxVersion,
		n.CreatedBy, now.Unix(), now.Unix())
	if err != nil {
		return Notice{}, err
	}
	id, _ := res.LastInsertId()
	return s.Get(ctx, id)
}

// Update replaces a notice's content. An ended notice cannot be changed.
func (s *Store) Update(ctx context.Context, id int64, n Notice, now time.Time) (Notice, error) {
	res, err := s.db.ExecContext(ctx, `UPDATE notices SET kind=?, lang=?, title=?, body=?, link_url=?, link_label=?, starts_at=?, ends_at=?, dismissible=?, blocking=?,
		block_from=?, block_until=?, apps=?, platforms=?, min_version=?, max_version=?, updated_at=?
		WHERE id = ? AND (ended_at = 0 OR ended_at > ?)`,
		n.Kind, n.Lang, n.Title, n.Body, n.LinkURL, n.LinkLabel, n.StartsAt, n.EndsAt, b2i(n.Dismissible), b2i(n.Blocking),
		n.BlockFrom, n.BlockUntil, strings.Join(n.Apps, ","), strings.Join(n.Platforms, ","), n.MinVersion, n.MaxVersion, now.Unix(),
		id, now.Unix())
	if err != nil {
		return Notice{}, err
	}
	if changed, _ := res.RowsAffected(); changed == 0 {
		if _, err := s.Get(ctx, id); err != nil {
			return Notice{}, err
		}
		return Notice{}, ErrEnded
	}
	return s.Get(ctx, id)
}

// End stops a notice showing, and blocking, from now on. The row stays, for the history.
func (s *Store) End(ctx context.Context, id int64, now time.Time) (Notice, error) {
	res, err := s.db.ExecContext(ctx, `UPDATE notices SET ended_at = ?, updated_at = ? WHERE id = ? AND (ended_at = 0 OR ended_at > ?)`,
		now.Unix(), now.Unix(), id, now.Unix())
	if err != nil {
		return Notice{}, err
	}
	if changed, _ := res.RowsAffected(); changed == 0 {
		if _, err := s.Get(ctx, id); err != nil {
			return Notice{}, err
		}
		return Notice{}, ErrEnded
	}
	return s.Get(ctx, id)
}

// Showing returns the notices users should see at now: started, and neither run out nor ended.
func (s *Store) Showing(ctx context.Context, now time.Time) ([]Notice, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+columns+` FROM notices
		WHERE starts_at <= ? AND (ends_at = 0 OR ends_at > ?) AND (ended_at = 0 OR ended_at > ?) ORDER BY id DESC LIMIT 200`,
		now.Unix(), now.Unix(), now.Unix())
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Notice
	for rows.Next() {
		n, err := scan(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, n)
	}
	return out, rows.Err()
}

// All returns the newest notices, ended and upcoming ones included, for the admin list.
func (s *Store) All(ctx context.Context, limit int) ([]Notice, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+columns+` FROM notices ORDER BY id DESC LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Notice
	for rows.Next() {
		n, err := scan(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, n)
	}
	return out, rows.Err()
}

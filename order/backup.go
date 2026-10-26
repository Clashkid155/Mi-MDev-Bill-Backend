package order

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// Snapshots of the database. The server writes one every hour to BACKUP_DIR and
// keeps the newest keepRecent of them, plus the last one of each day for
// keepDays days. deploy/ shows how to copy them off the server.
const (
	keepRecent = 48
	keepDays   = 30

	snapshotPrefix = "bills-"
	snapshotSuffix = ".db"
	snapshotLayout = "20060102T150405Z"

	backupAlertEvery = 6 * time.Hour
)

// BackupSummary is what a snapshot holds, for a person to compare with what
// they expect after a restore.
type BackupSummary struct {
	Orders         int
	Expenses       int
	Settings       int
	LatestOrderAt  time.Time // zero when there are no orders
	LatestWebhook  time.Time
	IntegrityCheck string
}

// Snapshot writes a consistent copy of the database to dir, checks that the copy
// opens and is intact, and returns its path. VACUUM INTO is SQLite's own online
// copy: safe while the server runs, and the copy needs no -wal file.
func (s *Store) Snapshot(ctx context.Context, dir string, now time.Time) (string, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	final := filepath.Join(dir, snapshotPrefix+now.UTC().Format(snapshotLayout)+snapshotSuffix)
	tmp := final + ".partial"
	_ = os.Remove(tmp)
	if _, err := s.db.ExecContext(ctx, `VACUUM INTO ?`, tmp); err != nil {
		_ = os.Remove(tmp)
		return "", fmt.Errorf("copy the database: %w", err)
	}
	if err := os.Chmod(tmp, 0o600); err != nil {
		_ = os.Remove(tmp)
		return "", err
	}
	if _, err := VerifyBackup(ctx, tmp); err != nil {
		_ = os.Remove(tmp)
		return "", fmt.Errorf("the copy is not good: %w", err)
	}
	if err := os.Rename(tmp, final); err != nil {
		_ = os.Remove(tmp)
		return "", err
	}
	return final, nil
}

// VerifyBackup opens a snapshot (read only in effect) and checks it the way you
// would after a restore: SQLite's own integrity check, then the tables the
// server needs, then a count of what is in them.
func VerifyBackup(ctx context.Context, path string) (BackupSummary, error) {
	var sum BackupSummary
	if _, err := os.Stat(path); err != nil {
		return sum, err
	}
	db, err := sql.Open("sqlite", path+"?_pragma=query_only(1)")
	if err != nil {
		return sum, err
	}
	defer db.Close()
	db.SetMaxOpenConns(1)

	if err := db.QueryRowContext(ctx, `PRAGMA integrity_check`).Scan(&sum.IntegrityCheck); err != nil {
		return sum, err
	}
	if sum.IntegrityCheck != "ok" {
		return sum, fmt.Errorf("integrity check failed: %s", sum.IntegrityCheck)
	}
	count := func(into *int, table string) error {
		return db.QueryRowContext(ctx, `SELECT COUNT(*) FROM `+table).Scan(into)
	}
	for table, into := range map[string]*int{"orders": &sum.Orders, "expenses": &sum.Expenses, "settings": &sum.Settings} {
		if err := count(into, table); err != nil {
			return sum, fmt.Errorf("table %s: %w", table, err)
		}
	}
	var latest, latestHook sql.NullInt64
	if err := db.QueryRowContext(ctx, `SELECT MAX(created_at) FROM orders`).Scan(&latest); err != nil {
		return sum, err
	}
	if err := db.QueryRowContext(ctx, `SELECT MAX(received_at) FROM webhook_events`).Scan(&latestHook); err != nil {
		return sum, err
	}
	if latest.Valid {
		sum.LatestOrderAt = time.Unix(latest.Int64, 0)
	}
	if latestHook.Valid {
		sum.LatestWebhook = time.Unix(latestHook.Int64, 0)
	}
	return sum, nil
}

// PruneSnapshots deletes old snapshots from dir, keeping the newest keepRecent,
// and from older ones the last of each day for keepDays days. It returns how many
// it removed. Only files named like our snapshots are touched.
func PruneSnapshots(dir string, now time.Time) (int, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return 0, err
	}
	type snap struct {
		name string
		at   time.Time
	}
	var snaps []snap
	for _, e := range entries {
		name := e.Name()
		if strings.HasSuffix(name, ".partial") && strings.HasPrefix(name, snapshotPrefix) {
			_ = os.Remove(filepath.Join(dir, name)) // left by a crash mid-copy
			continue
		}
		if !strings.HasPrefix(name, snapshotPrefix) || !strings.HasSuffix(name, snapshotSuffix) {
			continue
		}
		stamp := strings.TrimSuffix(strings.TrimPrefix(name, snapshotPrefix), snapshotSuffix)
		at, err := time.Parse(snapshotLayout, stamp)
		if err != nil {
			continue
		}
		snaps = append(snaps, snap{name, at})
	}
	sort.Slice(snaps, func(i, j int) bool { return snaps[i].at.After(snaps[j].at) }) // newest first

	keep := map[string]bool{}
	lastOfDay := map[string]bool{}
	cutoff := now.UTC().AddDate(0, 0, -keepDays)
	for i, sn := range snaps {
		if i < keepRecent {
			keep[sn.name] = true
			continue
		}
		day := sn.at.Format("2006-01-02")
		if sn.at.After(cutoff) && !lastOfDay[day] { // newest first, so the first seen is the day's last
			lastOfDay[day] = true
			keep[sn.name] = true
		}
	}
	removed := 0
	for _, sn := range snaps {
		if keep[sn.name] {
			continue
		}
		if err := os.Remove(filepath.Join(dir, sn.name)); err != nil && !errors.Is(err, os.ErrNotExist) {
			return removed, err
		}
		removed++
	}
	return removed, nil
}

// WatchBackups saves a snapshot now and then every `every` until ctx ends. A
// failed snapshot is emailed to the owner (at most every few hours): a backup
// that quietly stopped working is the worst kind.
func (a *API) WatchBackups(ctx context.Context, dir string, every time.Duration) {
	a.vending.Add(1)
	defer a.vending.Done()
	ticker := time.NewTicker(every)
	defer ticker.Stop()
	for {
		a.backupOnce(ctx, dir)
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

func (a *API) backupOnce(ctx context.Context, dir string) {
	path, err := a.Store.Snapshot(ctx, dir, time.Now())
	if err == nil {
		_, err = PruneSnapshots(dir, time.Now())
	}
	if err != nil {
		if ctx.Err() != nil {
			return // shutting down, not a failed backup
		}
		a.Log.Error("Backup - Store", "detail", err.Error())
		a.alert("backup", backupAlertEvery, "The database backup failed",
			"The server could not save its hourly database snapshot to "+dir+".\n\n"+err.Error()+
				"\n\nUntil this is fixed, a lost disk loses everything since the last good backup.")
		return
	}
	a.Log.Info("Backup - Store", "detail", "saved "+filepath.Base(path))
}

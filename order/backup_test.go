package order

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestSnapshotHoldsEverythingAndIsPrivate(t *testing.T) {
	f := newFakes(t)
	ctx := context.Background()
	if _, err := f.svc.Create(ctx, airtimeRequest); err != nil {
		t.Fatal(err)
	}
	if err := f.svc.Store.LogWebhook(ctx, "kora", "k1", []byte(`{"event":"x"}`)); err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(t.TempDir(), "backups") // does not exist yet

	path, err := f.svc.Store.Snapshot(ctx, dir, time.Date(2026, 10, 26, 9, 30, 0, 0, time.UTC))
	if err != nil {
		t.Fatal(err)
	}
	if filepath.Base(path) != "bills-20261026T093000Z.db" {
		t.Errorf("unexpected name %s", path)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Errorf("a snapshot holds customers' details and must be private, mode %v", info.Mode().Perm())
	}
	sum, err := VerifyBackup(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	if sum.Orders != 1 || sum.LatestOrderAt.IsZero() || sum.LatestWebhook.IsZero() {
		t.Errorf("the snapshot should hold the order and webhook: %+v", sum)
	}
	if leftovers, _ := filepath.Glob(filepath.Join(dir, "*.partial")); len(leftovers) != 0 {
		t.Errorf("temporary files left behind: %v", leftovers)
	}
}

func TestVerifyBackupRejectsABrokenFile(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()

	if _, err := VerifyBackup(ctx, filepath.Join(dir, "missing.db")); err == nil {
		t.Error("a missing file must not verify")
	}
	garbage := filepath.Join(dir, "garbage.db")
	os.WriteFile(garbage, []byte(strings.Repeat("this is not a database ", 500)), 0o600)
	if _, err := VerifyBackup(ctx, garbage); err == nil {
		t.Error("a file that is not a database must not verify")
	}
	empty := filepath.Join(dir, "empty.db") // a database with none of our tables, like a wrong file
	os.WriteFile(empty, nil, 0o600)
	if _, err := VerifyBackup(ctx, empty); err == nil {
		t.Error("a database without our tables must not verify")
	}
}

func TestPruneKeepsRecentAndOneADay(t *testing.T) {
	dir := t.TempDir()
	now := time.Date(2026, 10, 26, 12, 0, 0, 0, time.UTC)
	touch := func(at time.Time) string {
		name := snapshotPrefix + at.Format(snapshotLayout) + snapshotSuffix
		os.WriteFile(filepath.Join(dir, name), []byte("x"), 0o600)
		return name
	}
	// 100 hourly snapshots: the newest 48 stay, and older ones thin to one a day.
	var all []string
	for h := 0; h < 100; h++ {
		all = append(all, touch(now.Add(-time.Duration(h)*time.Hour)))
	}
	// One far older than the daily window, a stranger's file, and a crashed copy.
	ancient := touch(now.AddDate(0, 0, -60))
	os.WriteFile(filepath.Join(dir, "notes.txt"), []byte("mine"), 0o600)
	os.WriteFile(filepath.Join(dir, snapshotPrefix+"20260101T000000Z.db.partial"), []byte("x"), 0o600)

	removed, err := PruneSnapshots(dir, now)
	if err != nil {
		t.Fatal(err)
	}
	exists := func(name string) bool { _, err := os.Stat(filepath.Join(dir, name)); return err == nil }

	for _, name := range all[:keepRecent] {
		if !exists(name) {
			t.Fatalf("recent snapshot %s was removed", name)
		}
	}
	if exists(ancient) {
		t.Error("a snapshot past the daily window should go")
	}
	if !exists("notes.txt") {
		t.Error("files that are not snapshots must never be touched")
	}
	if exists(snapshotPrefix + "20260101T000000Z.db.partial") {
		t.Error("a crashed partial copy should be cleaned up")
	}
	// Hours 48..99 span 26 Oct 2026 12:00 minus 48h..99h: days 24, 23, 22 Oct. One of each is kept.
	left, _ := filepath.Glob(filepath.Join(dir, snapshotPrefix+"*"+snapshotSuffix))
	if len(left) != keepRecent+removedKeptDays(all[keepRecent:]) {
		t.Errorf("%d snapshots left, want %d recent plus one per older day", len(left), keepRecent+removedKeptDays(all[keepRecent:]))
	}
	if removed == 0 {
		t.Error("nothing was pruned")
	}
}

// removedKeptDays counts the distinct days among the given snapshot names.
func removedKeptDays(names []string) int {
	days := map[string]bool{}
	for _, n := range names {
		days[strings.TrimPrefix(n, snapshotPrefix)[:8]] = true
	}
	return len(days)
}

func TestAFailingBackupAlertsTheOwner(t *testing.T) {
	f := newFakes(t)
	_, sent := alertsOn(t, f)
	api := &API{Service: f.svc}

	blocker := filepath.Join(t.TempDir(), "file")
	os.WriteFile(blocker, []byte("x"), 0o600)
	api.backupOnce(context.Background(), filepath.Join(blocker, "inside")) // a directory cannot be made under a file
	api.backupOnce(context.Background(), filepath.Join(blocker, "inside"))
	got := sent()
	if len(got) != 1 || !strings.Contains(got[0], "database backup failed") {
		t.Fatalf("want one alert, got %q", got)
	}

	api.backupOnce(context.Background(), filepath.Join(t.TempDir(), "good")) // and a good one stays quiet
	if got := sent(); len(got) != 1 {
		t.Errorf("a good backup must not alert: %q", got)
	}
}

func TestOldWebhooksArePurgedButRecentOnesStay(t *testing.T) {
	f := newFakes(t)
	ctx := context.Background()
	st := f.svc.Store
	st.LogWebhook(ctx, "kora", "old", []byte("{}"))
	st.LogWebhook(ctx, "kora", "recent", []byte("{}"))
	st.db.Exec(`UPDATE webhook_events SET received_at = ? WHERE event_key = 'old'`, time.Now().AddDate(0, 0, -100).Unix())

	api := &API{Service: f.svc, WebhookRetention: 90 * 24 * time.Hour}
	api.purgeWebhooks(ctx)
	var keys []string
	rows, _ := st.db.Query(`SELECT event_key FROM webhook_events`)
	for rows.Next() {
		var k string
		rows.Scan(&k)
		keys = append(keys, k)
	}
	rows.Close()
	if len(keys) != 1 || keys[0] != "recent" {
		t.Fatalf("kept %v, want only the recent one", keys)
	}

	// Retention 0 keeps everything forever.
	st.db.Exec(`UPDATE webhook_events SET received_at = 1`)
	(&API{Service: f.svc}).purgeWebhooks(ctx)
	var n int
	st.db.QueryRow(`SELECT COUNT(*) FROM webhook_events`).Scan(&n)
	if n != 1 {
		t.Errorf("retention 0 must delete nothing, %d left", n)
	}
}

func TestTheDatabaseFileIsPrivateAndChecked(t *testing.T) {
	path := filepath.Join(t.TempDir(), "private.db")
	os.WriteFile(path, nil, 0o644) // an old file made with open permissions
	s, err := OpenStore(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	for _, p := range []string{path, path + "-wal", path + "-shm"} {
		info, err := os.Stat(p)
		if err != nil {
			continue // a -wal or -shm that does not exist cannot leak
		}
		if info.Mode().Perm()&0o077 != 0 {
			t.Errorf("%s is readable by others: %v", filepath.Base(p), info.Mode().Perm())
		}
	}
	var fk int
	s.db.QueryRow(`PRAGMA foreign_keys`).Scan(&fk)
	if fk != 1 {
		t.Error("foreign keys should be enforced")
	}

	// A damaged file stops the server instead of running on it.
	bad := filepath.Join(t.TempDir(), "bad.db")
	os.WriteFile(bad, []byte(strings.Repeat("garbage ", 1000)), 0o600)
	if _, err := OpenStore(bad); err == nil {
		t.Error("opening a file that is not a database must fail")
	}
}

func TestTheBackupLoopSavesSnapshotsAndStopsWhenAsked(t *testing.T) {
	f := newFakes(t)
	dir := t.TempDir()
	api := &API{Service: f.svc}
	ctx, stop := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { api.WatchBackups(ctx, dir, 1100*time.Millisecond); close(done) }()

	// One at once on start, and another a tick later (names carry the second, so they differ).
	deadline := time.Now().Add(10 * time.Second)
	var found []string
	for time.Now().Before(deadline) {
		found, _ = filepath.Glob(filepath.Join(dir, "bills-*.db"))
		if len(found) >= 2 {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	stop()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("the backup loop did not stop when its context ended")
	}
	if len(found) < 2 {
		t.Fatalf("expected a snapshot at start and another after the interval, found %v", found)
	}
	for _, p := range found {
		if _, err := VerifyBackup(context.Background(), p); err != nil {
			t.Errorf("%s: %v", p, err)
		}
	}
}

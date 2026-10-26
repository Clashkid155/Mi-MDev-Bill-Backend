package main

import (
	"bytes"
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"Bills-Backend/order"
)

func TestVerifyBackupCommand(t *testing.T) {
	store, err := order.OpenStore(filepath.Join(t.TempDir(), "live.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	snap, err := store.Snapshot(context.Background(), t.TempDir(), time.Now())
	if err != nil {
		t.Fatal(err)
	}

	var out, errOut bytes.Buffer
	if code := verifyBackup([]string{snap}, &out, &errOut); code != 0 || !strings.Contains(out.String(), "backup ok: 0 orders") {
		t.Errorf("a good snapshot: exit %d, out %q, err %q", code, out.String(), errOut.String())
	}
	bad := filepath.Join(t.TempDir(), "bad.db")
	writeFile(t, bad, strings.Repeat("junk ", 4000))
	out.Reset()
	errOut.Reset()
	if code := verifyBackup([]string{bad}, &out, &errOut); code != 1 || !strings.Contains(errOut.String(), "BAD BACKUP") {
		t.Errorf("a broken file: exit %d, err %q", code, errOut.String())
	}
	if code := verifyBackup(nil, io.Discard, io.Discard); code != 2 {
		t.Errorf("no file given: exit %d, want 2", code)
	}
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

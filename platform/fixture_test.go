package platform

import (
	"database/sql"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"Bills-Backend/ratelimit"
	"Bills-Backend/reply"
)

const ownerToken = "owner-secret-token"

// alertRecorder is an Alerter that remembers what it was told.
type alertRecorder struct {
	mu   sync.Mutex
	sent []string
}

func (r *alertRecorder) Alert(key string, throttle time.Duration, subject, body string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.sent = append(r.sent, subject)
}

func (r *alertRecorder) subjects() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string{}, r.sent...)
}

// fixture is a real database, the real guard and the shared admin endpoints, plus stand-ins for
// a feature's own endpoints (reads and changes), so the guard's rules can be tested on their own.
type fixture struct {
	db     *sql.DB
	store  *Store
	admin  *Admin
	alerts *alertRecorder
	log    *slog.Logger
	writes atomic.Int64 // how many changes the stand-in endpoints actually performed
}

func newFixture(t testing.TB) *fixture {
	t.Helper()
	db, err := OpenDB(filepath.Join(t.TempDir(), "platform.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	store, err := NewStore(db)
	if err != nil {
		t.Fatal(err)
	}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	f := &fixture{db: db, store: store, alerts: &alertRecorder{}, log: log}
	f.admin = &Admin{Store: store, Log: log, Alerts: f.alerts}
	return f
}

// adminServer serves the shared admin endpoints and the stand-ins behind the guard.
func adminServer(t testing.TB, owner string) (*httptest.Server, *fixture, *AdminGuard) {
	t.Helper()
	f := newFixture(t)
	mux := http.NewServeMux()
	f.admin.Register(mux)
	ok := func(w http.ResponseWriter, r *http.Request) { reply.OK(w, http.StatusOK, map[string]any{}) }
	change := func(w http.ResponseWriter, r *http.Request) {
		io.Copy(io.Discard, r.Body)
		f.writes.Add(1)
		reply.OK(w, http.StatusOK, map[string]any{})
	}
	for _, p := range []string{"/admin/orders", "/admin/stats", "/admin/expenses", "/admin/webhooks", "/admin/settings"} {
		mux.HandleFunc("GET "+p, ok)
	}
	mux.HandleFunc("POST /admin/expenses", change)
	mux.HandleFunc("DELETE /admin/expenses/{id}", change)
	mux.HandleFunc("PUT /admin/settings/{key}", change)

	guard := &AdminGuard{Store: f.store, Owner: owner, Fails: ratelimit.NewLockout(5, 15*time.Minute), Log: f.log}
	srv := httptest.NewServer(guard.Wrap(mux))
	t.Cleanup(srv.Close)
	return srv, f, guard
}

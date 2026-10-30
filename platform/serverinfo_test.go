package platform

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// serve puts the fixture's shared admin endpoints behind a guard with the owner token.
func serve(t *testing.T, f *fixture) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	f.admin.Register(mux)
	srv := httptest.NewServer((&AdminGuard{Store: f.store, Owner: ownerToken}).Wrap(mux))
	t.Cleanup(srv.Close)
	return srv
}

func TestServerInfoSaysWhichBuildAndWhichModeToEveryAdminToken(t *testing.T) {
	f := newFixture(t)
	f.admin.Info = ServerInfo{
		Version: "8a439d2", GoVersion: "go1.27.3", Env: "production", PaymentProvider: "kora",
		ABSSandbox: false, DirectTestRoutes: false, StartedAt: time.Now().Add(-90 * time.Second),
	}
	srv := serve(t, f)
	viewer, _ := makeToken(t, srv, "dashboard", RoleViewer)
	admin, _ := makeToken(t, srv, "phone", RoleAdmin)

	for name, tok := range map[string]string{"owner": ownerToken, "admin": admin, "viewer": viewer} {
		code, data := call(t, srv, "GET", "/admin/server", tok, nil)
		if code != 200 {
			t.Fatalf("%s: answered %d", name, code)
		}
		if data["version"] != "8a439d2" || data["go_version"] != "go1.27.3" || data["env"] != "production" ||
			data["payment_provider"] != "kora" || data["abs_sandbox"] != false || data["direct_test_routes"] != false {
			t.Errorf("%s: %v", name, data)
		}
		if up := data["uptime_seconds"].(float64); up < 89 || up > 120 {
			t.Errorf("%s: uptime %v, want about 90", name, up)
		}
		if started := data["started_at"].(float64); started < 1e9 {
			t.Errorf("%s: started_at %v", name, started)
		}
	}
	if code, _ := call(t, srv, "GET", "/admin/server", "", nil); code != 401 {
		t.Errorf("without a token: %d, want 401", code)
	}
}

func TestServerInfoIsHonestWhenNothingWasSet(t *testing.T) {
	srv := serve(t, newFixture(t))
	code, data := call(t, srv, "GET", "/admin/server", ownerToken, nil)
	if code != 200 || data["uptime_seconds"] != 0.0 || data["version"] != "" {
		t.Errorf("an unset server should report empty values, not invented ones: %d %v", code, data)
	}
}

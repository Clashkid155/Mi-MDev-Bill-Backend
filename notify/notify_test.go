package notify

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"Bills-Backend/platform"
)

const owner = "owner-secret-token"

// ---- the rules ----

func TestVersionsAreComparedByNumber(t *testing.T) {
	cases := []struct {
		a, b string
		want int
	}{
		{"1.4.2", "1.4.2", 0},
		{"1.4.2", "1.4.10", -1}, // 2 is older than 10: compared as numbers, not text
		{"1.10", "1.9.9", 1},
		{"2", "1.99.99", 1},
		{"1.4", "1.4.0", 0},
		{"1.4.2+7", "1.4.2", 0}, // a build number is ignored
		{"v1.4.2", "1.4.2", 0},
		{"1.4.2-beta", "1.4.2", 0},
		{"", "0.0.1", -1}, // unknown is the oldest
		{"banana", "0.0.1", -1},
		{"1.0.0.5", "1.0.0.4", 1},
	}
	for _, c := range cases {
		if got := compareVersions(c.a, c.b); got != c.want {
			t.Errorf("compareVersions(%q, %q) = %d, want %d", c.a, c.b, got, c.want)
		}
	}
}

func TestWhoAReaderIs(t *testing.T) {
	c := func(app, plat, ver string) platform.Caller {
		return platform.Caller{App: app, Platform: plat, Version: ver}
	}
	cases := []struct {
		name   string
		notice Notice
		caller platform.Caller
		want   bool
	}{
		{"for everyone, a known caller", Notice{}, c("mi-mdev", "android", "1.4.2"), true},
		{"for everyone, an unknown caller", Notice{}, c("", "", ""), true},
		{"for one app, that app", Notice{Apps: []string{"mi-mdev"}}, c("mi-mdev", "android", "1"), true},
		{"for one app, another app", Notice{Apps: []string{"mi-mdev"}}, c("dashboard", "android", "1"), false},
		{"for one app, an unnamed caller", Notice{Apps: []string{"mi-mdev"}}, c("", "android", "1"), false},
		{"for two apps", Notice{Apps: []string{"mi-mdev", "dashboard"}}, c("dashboard", "ios", "1"), true},
		{"for android, android", Notice{Platforms: []string{"android"}}, c("a", "android", "1"), true},
		{"for android, ios", Notice{Platforms: []string{"android"}}, c("a", "ios", "1"), false},
		{"for android, no platform", Notice{Platforms: []string{"android"}}, c("a", "", "1"), false},
		{"from 1.4, a newer app", Notice{MinVersion: "1.4"}, c("a", "android", "1.4.2"), true},
		{"from 1.4, exactly 1.4", Notice{MinVersion: "1.4"}, c("a", "android", "1.4"), true},
		{"from 1.4, an older app", Notice{MinVersion: "1.4"}, c("a", "android", "1.3.9"), false},
		{"from 1.4, an unknown version", Notice{MinVersion: "1.4"}, c("a", "android", ""), false},
		{"up to 1.3.9, an older app", Notice{MaxVersion: "1.3.9"}, c("a", "android", "1.2.0"), true},
		{"up to 1.3.9, exactly that", Notice{MaxVersion: "1.3.9"}, c("a", "android", "1.3.9"), true},
		{"up to 1.3.9, a newer app", Notice{MaxVersion: "1.3.9"}, c("a", "android", "1.4.0"), false},
		{"up to 1.3.9, an unknown version (an old app that does not say)", Notice{MaxVersion: "1.3.9"}, c("a", "android", ""), true},
		{"a range, inside", Notice{MinVersion: "1.2", MaxVersion: "1.4"}, c("a", "android", "1.3"), true},
		{"a range, outside", Notice{MinVersion: "1.2", MaxVersion: "1.4"}, c("a", "android", "1.5"), false},
		{"everything combined", Notice{Apps: []string{"mi-mdev"}, Platforms: []string{"android"}, MaxVersion: "1.3.9"}, c("mi-mdev", "android", "1.3.0"), true},
		{"everything combined, wrong platform", Notice{Apps: []string{"mi-mdev"}, Platforms: []string{"android"}, MaxVersion: "1.3.9"}, c("mi-mdev", "ios", "1.3.0"), false},
	}
	for _, tc := range cases {
		if got := tc.notice.matches(tc.caller); got != tc.want {
			t.Errorf("%s: matches = %t, want %t", tc.name, got, tc.want)
		}
	}
}

func TestWhenANoticeShowsAndWhenItBlocks(t *testing.T) {
	const start, end = 1000, 2000
	n := Notice{StartsAt: start, EndsAt: end, Blocking: true}
	for at, want := range map[int64][3]any{ // showing, blocking, status
		999:  {false, false, "upcoming"},
		1000: {true, true, "active"},
		1999: {true, true, "active"},
		2000: {false, false, "ended"},
	} {
		if got := [3]any{n.showingAt(at), n.blockingAt(at), n.status(at)}; got != want {
			t.Errorf("at %d: %v, want %v", at, got, want)
		}
	}

	// A heads-up now, the block only for the maintenance window.
	heads := Notice{StartsAt: 1000, EndsAt: 5000, Blocking: true, BlockFrom: 3000, BlockUntil: 4000}
	for at, want := range map[int64]bool{1500: false, 2999: false, 3000: true, 3999: true, 4000: false, 4500: false} {
		if got := heads.blockingAt(at); got != want {
			t.Errorf("heads-up, at %d: blocking %t, want %t", at, got, want)
		}
	}
	if !heads.showingAt(4500) {
		t.Error("the heads-up keeps showing after the block ends, until its own end")
	}

	// Ended by hand: it stops showing and blocking at once, even inside its window.
	stopped := Notice{StartsAt: 1000, EndsAt: 5000, EndedAt: 3500, Blocking: true, BlockFrom: 3000, BlockUntil: 4000}
	if !stopped.blockingAt(3400) || stopped.blockingAt(3500) || stopped.showingAt(3500) || stopped.status(3500) != "ended" {
		t.Error("ending a notice by hand must stop it showing and blocking from that moment")
	}

	// No end at all: blocks until someone ends it.
	open := Notice{StartsAt: 1000, Blocking: true}
	if !open.blockingAt(1_000_000_000) {
		t.Error("a notice with no end blocks until it is ended")
	}
	if (Notice{StartsAt: 1000, Blocking: false}).blockingAt(2000) {
		t.Error("a notice that does not block never blocks")
	}
}

func TestInputIsCheckedAndDefaultsAreFilledIn(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	good := Input{Kind: "maintenance", Title: "Back at 3am", Body: "We are upgrading.", Blocking: true, Apps: []string{" Mi-MDev ", "mi-mdev", "Dashboard"}}
	n, err := good.build(now)
	if err != nil {
		t.Fatal(err)
	}
	if n.StartsAt != now.Unix() || n.Lang != "en" || n.Dismissible || len(n.Apps) != 2 || n.Apps[0] != "mi-mdev" || n.Apps[1] != "dashboard" {
		t.Errorf("defaults: %+v", n)
	}
	info, _ := Input{Kind: "info", Title: "Hello"}.build(now)
	if !info.Dismissible || info.Blocking {
		t.Errorf("a plain notice can be dismissed: %+v", info)
	}
	no := false
	forced, _ := Input{Kind: "info", Title: "x", Dismissible: &no}.build(now)
	if forced.Dismissible {
		t.Error("dismissible=false must be respected")
	}
	yes := true
	stillBlocks, _ := Input{Kind: "maintenance", Title: "x", Blocking: true, Dismissible: &yes}.build(now)
	if stillBlocks.Dismissible {
		t.Error("a notice that blocks orders can never be dismissed")
	}

	bads := map[string]Input{
		"no kind":                      {Title: "x"},
		"unknown kind":                 {Kind: "promo", Title: "x"},
		"no title":                     {Kind: "info"},
		"title too long":               {Kind: "info", Title: strings.Repeat("a", 81)},
		"body too long":                {Kind: "info", Title: "x", Body: strings.Repeat("a", 1001)},
		"another language":             {Kind: "info", Title: "x", Lang: "yo"},
		"http link":                    {Kind: "info", Title: "x", LinkURL: "http://example.com", LinkLabel: "Open"},
		"link without label":           {Kind: "info", Title: "x", LinkURL: "https://example.com"},
		"label without link":           {Kind: "info", Title: "x", LinkLabel: "Open"},
		"javascript link":              {Kind: "info", Title: "x", LinkURL: "javascript:alert(1)", LinkLabel: "Open"},
		"ends before it starts":        {Kind: "info", Title: "x", StartsAt: 2000, EndsAt: 1000},
		"block times without blocking": {Kind: "info", Title: "x", BlockFrom: 5},
		"block before start":           {Kind: "maintenance", Title: "x", Blocking: true, StartsAt: 2000, BlockFrom: 1000},
		"block ends before it begins":  {Kind: "maintenance", Title: "x", Blocking: true, BlockFrom: When(now.Unix() + 100), BlockUntil: When(now.Unix() + 50)},
		"bad app name":                 {Kind: "info", Title: "x", Apps: []string{"My App!"}},
		"too many apps":                {Kind: "info", Title: "x", Apps: []string{"a", "b", "c", "d", "e", "f", "g", "h", "i", "j", "k"}},
		"bad version":                  {Kind: "info", Title: "x", MinVersion: "latest"},
		"min above max":                {Kind: "info", Title: "x", MinVersion: "2.0", MaxVersion: "1.0"},
	}
	for name, in := range bads {
		if _, err := in.build(now); err == nil {
			t.Errorf("%s was accepted", name)
		}
	}
}

func TestTimesCanBeWrittenThreeWays(t *testing.T) {
	var in Input
	body := `{"starts_at":"2026-11-03T01:00:00+01:00","ends_at":1793668800,"block_from":"2026-11-03","block_until":null}`
	if err := json.Unmarshal([]byte(body), &in); err != nil {
		t.Fatal(err)
	}
	if in.StartsAt != 1793664000 || in.EndsAt != 1793668800 || in.BlockUntil != 0 {
		t.Errorf("RFC 3339, unix seconds and null: %d %d %d", in.StartsAt, in.EndsAt, in.BlockUntil)
	}
	want := time.Date(2026, 11, 3, 0, 0, 0, 0, lagos).Unix()
	if int64(in.BlockFrom) != want {
		t.Errorf("a day is midnight Nigerian time: %d, want %d", in.BlockFrom, want)
	}
	if err := json.Unmarshal([]byte(`{"starts_at":"next tuesday"}`), &in); err == nil {
		t.Error("an unreadable time must be refused")
	}
}

// ---- the service, through the real guard ----

type fixture struct {
	srv    *httptest.Server
	svc    *Service
	db     interface{ Close() error }
	events *platform.EventRecorder
	shared *platform.Store
	clock  *clock
}

type clock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *clock) now() time.Time { c.mu.Lock(); defer c.mu.Unlock(); return c.t }
func (c *clock) add(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	db, err := platform.OpenDB(filepath.Join(t.TempDir(), "notify.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	shared, err := platform.NewStore(db)
	if err != nil {
		t.Fatal(err)
	}
	store, err := NewStore(db)
	if err != nil {
		t.Fatal(err)
	}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	events := platform.NewEventRecorder(shared, log)
	t.Cleanup(events.Close)
	clk := &clock{t: time.Unix(1_800_000_000, 0)}
	svc := &Service{Store: store, Log: log, Events: events, Now: clk.now}

	adminMux := http.NewServeMux()
	svc.RegisterAdmin(adminMux)
	(&platform.Admin{Store: shared, Log: log}).Register(adminMux)
	guard := &platform.AdminGuard{Store: shared, Owner: owner, Log: log}
	mux := http.NewServeMux()
	svc.RegisterPublic(mux)
	mux.Handle("/admin/", guard.Wrap(adminMux))
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return &fixture{srv: srv, svc: svc, db: db, events: events, shared: shared, clock: clk}
}

func (f *fixture) do(t *testing.T, method, path, token string, body any, headers ...string) (int, map[string]any, http.Header) {
	t.Helper()
	var rd io.Reader
	switch b := body.(type) {
	case nil:
	case string:
		rd = strings.NewReader(b)
	default:
		raw, _ := json.Marshal(b)
		rd = bytes.NewReader(raw)
	}
	req, _ := http.NewRequest(method, f.srv.URL+path, rd)
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	for i := 0; i+1 < len(headers); i += 2 {
		req.Header.Set(headers[i], headers[i+1])
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	var env struct {
		Data map[string]any `json:"data"`
	}
	_ = json.Unmarshal(raw, &env)
	return resp.StatusCode, env.Data, resp.Header
}

func (f *fixture) token(t *testing.T, name, role string) string {
	t.Helper()
	code, data, _ := f.do(t, "POST", "/admin/tokens", owner, map[string]any{"name": name, "role": role})
	if code != 200 {
		t.Fatalf("making a token answered %d", code)
	}
	return data["token"].(string)
}

func (f *fixture) publish(t *testing.T, body map[string]any) map[string]any {
	t.Helper()
	code, data, _ := f.do(t, "POST", "/admin/notifications", owner, body)
	if code != 200 {
		t.Fatalf("publishing %v answered %d: %v", body, code, data)
	}
	return data
}

func titles(data map[string]any) []string {
	var out []string
	for _, n := range data["notifications"].([]any) {
		out = append(out, n.(map[string]any)["title"].(string))
	}
	return out
}

func TestTheAdminLifecycle(t *testing.T) {
	f := newFixture(t)
	admin := f.token(t, "phone", "admin")
	viewer := f.token(t, "dashboard", "viewer")

	made := f.publish(t, map[string]any{"kind": "maintenance", "title": "Back at 3am", "body": "Upgrading.", "blocking": true,
		"ends_at": f.clock.now().Add(2 * time.Hour).Unix(), "apps": []string{"mi-mdev"}})
	id := int64(made["id"].(float64))
	if made["status"] != "active" || made["blocking_now"] != true || made["dismissible"] != false || made["created_by"] != "owner" {
		t.Errorf("a notice made now: %v", made)
	}

	// Read by any admin token; changed only by owner and admin tokens.
	for name, tok := range map[string]string{"owner": owner, "admin": admin, "viewer": viewer} {
		if code, data, _ := f.do(t, "GET", "/admin/notifications", tok, nil); code != 200 || len(data["notifications"].([]any)) != 1 {
			t.Errorf("%s listing: %d %v", name, code, data)
		}
	}
	if code, _, _ := f.do(t, "POST", "/admin/notifications", viewer, map[string]any{"kind": "info", "title": "x"}); code != 403 {
		t.Errorf("a viewer making a notice: %d, want 403", code)
	}
	if code, _, _ := f.do(t, "DELETE", fmt.Sprintf("/admin/notifications/%d", id), viewer, nil); code != 403 {
		t.Errorf("a viewer ending a notice: %d, want 403", code)
	}
	if code, _, _ := f.do(t, "POST", "/admin/notifications", "", map[string]any{"kind": "info", "title": "x"}); code != 401 {
		t.Errorf("without a token: %d, want 401", code)
	}

	// An admin token can change it.
	code, changed, _ := f.do(t, "PUT", fmt.Sprintf("/admin/notifications/%d", id), admin, map[string]any{"kind": "maintenance", "title": "Back at 4am", "blocking": true, "ends_at": f.clock.now().Add(3 * time.Hour).Unix()})
	if code != 200 || changed["title"] != "Back at 4am" || changed["created_by"] != "owner" {
		t.Errorf("changing: %d %v", code, changed)
	}
	if code, _, _ := f.do(t, "PUT", "/admin/notifications/999", owner, map[string]any{"kind": "info", "title": "x"}); code != 404 {
		t.Errorf("changing a notice that does not exist: %d, want 404", code)
	}
	if code, _, _ := f.do(t, "PUT", fmt.Sprintf("/admin/notifications/%d", id), owner, map[string]any{"kind": "bogus", "title": "x"}); code != 400 {
		t.Errorf("a bad change: %d, want 400", code)
	}

	// Ending it stops it at once, keeps it in the list, and cannot be done twice.
	code, ended, _ := f.do(t, "DELETE", fmt.Sprintf("/admin/notifications/%d", id), admin, nil)
	if code != 200 || ended["status"] != "ended" || ended["blocking_now"] != false {
		t.Errorf("ending: %d %v", code, ended)
	}
	if code, _, _ := f.do(t, "DELETE", fmt.Sprintf("/admin/notifications/%d", id), owner, nil); code != 409 {
		t.Errorf("ending twice: %d, want 409", code)
	}
	if code, _, _ := f.do(t, "PUT", fmt.Sprintf("/admin/notifications/%d", id), owner, map[string]any{"kind": "info", "title": "x"}); code != 409 {
		t.Errorf("changing an ended notice: %d, want 409", code)
	}
	if code, _, _ := f.do(t, "DELETE", "/admin/notifications/999", owner, nil); code != 404 {
		t.Errorf("ending one that does not exist: %d, want 404", code)
	}
	if code, _, _ := f.do(t, "DELETE", "/admin/notifications/abc", owner, nil); code != 400 {
		t.Errorf("a bad id: %d, want 400", code)
	}
	_, ended2, _ := f.do(t, "GET", "/admin/notifications?status=ended", owner, nil)
	_, active2, _ := f.do(t, "GET", "/admin/notifications?status=active", owner, nil)
	if len(ended2["notifications"].([]any)) != 1 || len(active2["notifications"].([]any)) != 0 {
		t.Errorf("filtering by status: ended %v, active %v", ended2, active2)
	}
	for _, bad := range []string{"?status=gone", "?limit=0", "?limit=999", "?limit=x"} {
		if code, _, _ := f.do(t, "GET", "/admin/notifications"+bad, owner, nil); code != 400 {
			t.Errorf("%s answered %d, want 400", bad, code)
		}
	}

	// Everything an admin did is in the audit log and the events.
	f.events.Flush()
	_, audit, _ := f.do(t, "GET", "/admin/audit?limit=100", owner, nil)
	raw, _ := json.Marshal(audit)
	for _, want := range []string{`POST /admin/notifications`, `PUT /admin/notifications/{id}`, `DELETE /admin/notifications/{id}`, `"actor":"phone"`} {
		if !strings.Contains(string(raw), want) {
			t.Errorf("the audit log is missing %q", want)
		}
	}
	_, events, _ := f.do(t, "GET", "/admin/events?kind=notice.", owner, nil)
	evRaw, _ := json.Marshal(events)
	for _, want := range []string{"notice.created", "notice.updated", "notice.ended", "stops new orders"} {
		if !strings.Contains(string(evRaw), want) {
			t.Errorf("the events are missing %q: %s", want, evRaw)
		}
	}
}

func TestWhatAnAppSeesWithoutLoggingIn(t *testing.T) {
	f := newFixture(t)
	now := f.clock.now()
	f.publish(t, map[string]any{"kind": "info", "title": "Everyone"})
	f.publish(t, map[string]any{"kind": "info", "title": "Customers only", "apps": []string{"mi-mdev"}})
	f.publish(t, map[string]any{"kind": "info", "title": "Dashboard only", "apps": []string{"dashboard"}})
	f.publish(t, map[string]any{"kind": "info", "title": "Android only", "platforms": []string{"android"}})
	f.publish(t, map[string]any{"kind": "warning", "title": "Update please", "max_version": "1.3.9", "blocking": true, "apps": []string{"mi-mdev"}})
	f.publish(t, map[string]any{"kind": "info", "title": "Later", "starts_at": now.Add(time.Hour).Unix()})
	f.publish(t, map[string]any{"kind": "info", "title": "Gone", "starts_at": now.Add(-2 * time.Hour).Unix(), "ends_at": now.Add(-time.Hour).Unix()})

	see := func(headers ...string) []string {
		code, data, _ := f.do(t, "GET", "/v1/notifications", "", nil, headers...)
		if code != 200 {
			t.Fatalf("answered %d", code)
		}
		return titles(data)
	}
	same := func(got []string, want ...string) {
		t.Helper()
		if strings.Join(got, "|") != strings.Join(want, "|") {
			t.Errorf("got %v, want %v", got, want)
		}
	}

	// An unnamed caller sees only what is for everyone. Newest first within a kind.
	same(see(), "Everyone")
	// The customer app on android, an up to date version.
	same(see("X-App", "mi-mdev", "X-Platform", "android", "X-App-Version", "1.4.2"), "Android only", "Customers only", "Everyone")
	// The same app on ios, an old version: it is told to update, first.
	same(see("X-App", "mi-mdev", "X-Platform", "ios", "X-App-Version", "1.3.0"), "Update please", "Customers only", "Everyone")
	// An old app that does not report a version yet counts as the oldest.
	same(see("X-App", "mi-mdev", "X-Platform", "ios"), "Update please", "Customers only", "Everyone")
	// The dashboard.
	same(see("X-App", "Dashboard", "X-Platform", "ios", "X-App-Version", "2.0"), "Dashboard only", "Everyone")

	// A plain query string works too, and headers win over it.
	code, data, _ := f.do(t, "GET", "/v1/notifications?app=mi-mdev&platform=ios&version=1.3.0", "", nil)
	same(titles(data), "Update please", "Customers only", "Everyone")
	_, data, _ = f.do(t, "GET", "/v1/notifications?app=mi-mdev", "", nil, "X-App", "dashboard")
	same(titles(data), "Dashboard only", "Everyone")
	if code != 200 || data["server_time"] != float64(now.Unix()) {
		t.Errorf("the server's time is part of the answer: %v", data["server_time"])
	}

	// The notice that blocks orders cannot be dismissed, and says so for itself.
	_, data, _ = f.do(t, "GET", "/v1/notifications", "", nil, "X-App", "mi-mdev", "X-App-Version", "1.0")
	first := data["notifications"].([]any)[0].(map[string]any)
	if first["title"] != "Update please" || first["blocking_now"] != true || first["dismissible"] != false || first["updated_at"].(float64) < 1e9 {
		t.Errorf("the blocking notice: %v", first)
	}
	// Nothing about who made it or who it is for leaks into what an app sees.
	raw, _ := json.Marshal(first)
	for _, secret := range []string{"created_by", "apps", "platforms", "max_version"} {
		if strings.Contains(string(raw), secret) {
			t.Errorf("an app must not see %q: %s", secret, raw)
		}
	}

	// Time passes: the heads-up appears, the old one is already gone.
	f.clock.add(2 * time.Hour)
	if got := see(); len(got) != 2 {
		t.Errorf("after two hours: %v", got)
	}
}

func TestAtMostTwentyNoticesAreSentAndTheMostUrgentComeFirst(t *testing.T) {
	f := newFixture(t)
	for i := 0; i < 25; i++ {
		f.publish(t, map[string]any{"kind": "info", "title": fmt.Sprint("news ", i)})
	}
	f.publish(t, map[string]any{"kind": "maintenance", "title": "Down", "blocking": true})
	_, data, _ := f.do(t, "GET", "/v1/notifications", "", nil)
	got := titles(data)
	if len(got) != 20 || got[0] != "Down" {
		t.Errorf("%d notices, first %q; want 20 with the one that blocks orders first", len(got), got[0])
	}
}

func TestTheMaintenanceGate(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	mi := platform.Caller{App: "mi-mdev", Platform: "android", Version: "1.4.2"}

	if _, blocked := f.svc.OrdersBlocked(ctx, mi); blocked {
		t.Fatal("nothing blocks orders when there are no notices")
	}
	f.publish(t, map[string]any{"kind": "info", "title": "Just news"})
	if _, blocked := f.svc.OrdersBlocked(ctx, mi); blocked {
		t.Error("a notice that does not block must not stop orders")
	}

	// A heads-up now, the block only inside the window.
	now := f.clock.now()
	made := f.publish(t, map[string]any{"kind": "maintenance", "title": "Upgrade tonight", "body": "Orders pause from 1am to 3am.", "blocking": true,
		"ends_at": now.Add(5 * time.Hour).Unix(), "block_from": now.Add(time.Hour).Unix(), "block_until": now.Add(3 * time.Hour).Unix(), "apps": []string{"mi-mdev"}})
	if _, blocked := f.svc.OrdersBlocked(ctx, mi); blocked {
		t.Error("before the block window opens, orders go through")
	}
	f.clock.add(90 * time.Minute)
	block, blocked := f.svc.OrdersBlocked(ctx, mi)
	if !blocked || block.Title != "Upgrade tonight" || block.Message != "Orders pause from 1am to 3am." || block.Until != now.Add(3*time.Hour).Unix() || block.NoticeID != int64(made["id"].(float64)) {
		t.Errorf("inside the window: %+v %t", block, blocked)
	}
	// Only the audience it names.
	if _, blocked := f.svc.OrdersBlocked(ctx, platform.Caller{App: "dashboard", Platform: "ios", Version: "2"}); blocked {
		t.Error("a notice for the customer app must not stop the dashboard")
	}
	if _, blocked := f.svc.OrdersBlocked(ctx, platform.Caller{}); blocked {
		t.Error("an unnamed caller is not in a notice that names an app")
	}
	// Ended by hand: orders flow again at once.
	f.do(t, "DELETE", fmt.Sprintf("/admin/notifications/%d", int64(made["id"].(float64))), owner, nil)
	if _, blocked := f.svc.OrdersBlocked(ctx, mi); blocked {
		t.Error("ending the notice must lift the block at once")
	}

	// Everyone, with no end: blocks the unnamed callers too, with the title when there is no body.
	f.publish(t, map[string]any{"kind": "maintenance", "title": "Back soon", "blocking": true})
	block, blocked = f.svc.OrdersBlocked(ctx, platform.Caller{})
	if !blocked || block.Message != "Back soon" || block.Until != 0 {
		t.Errorf("a block for everyone with no end: %+v %t", block, blocked)
	}
	// When several apply, the one that lasts longest (no end counts as longest).
	f.publish(t, map[string]any{"kind": "maintenance", "title": "Short", "body": "short one", "blocking": true, "ends_at": f.clock.now().Add(time.Hour).Unix()})
	if block, _ := f.svc.OrdersBlocked(ctx, mi); block.Title != "Back soon" {
		t.Errorf("the longest block is reported: %+v", block)
	}
}

func TestAnUnreadableNoticeTableNeverStopsOrders(t *testing.T) {
	f := newFixture(t)
	f.publish(t, map[string]any{"kind": "maintenance", "title": "Down", "blocking": true})
	if _, blocked := f.svc.OrdersBlocked(context.Background(), platform.Caller{}); !blocked {
		t.Fatal("the notice should block before the database goes away")
	}
	f.db.Close() // the notices can no longer be read
	if _, blocked := f.svc.OrdersBlocked(context.Background(), platform.Caller{}); blocked {
		t.Error("when the notices cannot be read, orders must NOT be stopped: a broken table is not a reason to lose sales")
	}
}

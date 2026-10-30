package platform

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

// call makes an admin request and returns the status and the "data" of the answer.
func call(t *testing.T, srv *httptest.Server, method, path, token string, body any) (int, map[string]any) {
	t.Helper()
	var rd io.Reader
	switch b := body.(type) {
	case nil:
	case string:
		rd = strings.NewReader(b)
	case []byte:
		rd = bytes.NewReader(b)
	default:
		raw, _ := json.Marshal(b)
		rd = bytes.NewReader(raw)
	}
	req, _ := http.NewRequest(method, srv.URL+path, rd)
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
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
	return resp.StatusCode, env.Data
}

// makeToken has the owner make a token and returns it and its id.
func makeToken(t *testing.T, srv *httptest.Server, name string, role Role) (string, int64) {
	t.Helper()
	code, data := call(t, srv, "POST", "/admin/tokens", ownerToken, map[string]any{"name": name, "role": role})
	if code != 200 {
		t.Fatalf("making token %q answered %d", name, code)
	}
	return data["token"].(string), int64(data["id"].(float64))
}

func TestTheOwnerMakesATokenThatWorks(t *testing.T) {
	srv, f, _ := adminServer(t, ownerToken)

	code, data := call(t, srv, "POST", "/admin/tokens", ownerToken, map[string]any{"name": "dizz phone", "role": "admin"})
	token, _ := data["token"].(string)
	if code != 200 || !strings.HasPrefix(token, tokenPrefix) || len(token) < 40 {
		t.Fatalf("make: %d %v", code, data)
	}
	if data["note"] == nil || data["hint"] != token[len(token)-4:] {
		t.Errorf("the answer should say to copy it now and show its last 4 characters: %v", data)
	}

	// Only a hash is kept: the token itself must not be in the database.
	var stored string
	f.db.QueryRow(`SELECT token_hash FROM admin_tokens`).Scan(&stored)
	if stored != hashToken(token) || strings.Contains(stored, token) || len(stored) != 64 {
		t.Errorf("stored %q; want only the sha256 of the token", stored)
	}
	var all string
	f.db.QueryRow(`SELECT group_concat(name || role || token_hash || hint || created_by) FROM admin_tokens`).Scan(&all)
	if strings.Contains(all, token) {
		t.Error("the token is stored in clear somewhere")
	}

	if code, _ := call(t, srv, "GET", "/admin/orders", token, nil); code != 200 {
		t.Errorf("the new token answered %d on /admin/orders, want 200", code)
	}
	code, who := call(t, srv, "GET", "/admin/whoami", token, nil)
	if code != 200 || who["name"] != "dizz phone" || who["role"] != "admin" {
		t.Errorf("whoami: %d %v", code, who)
	}
	if _, who := call(t, srv, "GET", "/admin/whoami", ownerToken, nil); who["name"] != "owner" || who["role"] != "owner" {
		t.Errorf("the owner token should be the owner: %v", who)
	}

	// The list shows the token's details and never the token.
	code, listed := call(t, srv, "GET", "/admin/tokens", ownerToken, nil)
	raw, _ := json.Marshal(listed)
	if code != 200 || !strings.Contains(string(raw), "dizz phone") || strings.Contains(string(raw), token) || strings.Contains(string(raw), stored) {
		t.Errorf("list: %d %s", code, raw)
	}
}

func TestOnlyTheOwnerManagesTokens(t *testing.T) {
	srv, _, _ := adminServer(t, ownerToken)
	adminTok, id := makeToken(t, srv, "an admin", RoleAdmin)
	viewerTok, _ := makeToken(t, srv, "a viewer", RoleViewer)

	for name, tok := range map[string]string{"admin": adminTok, "viewer": viewerTok} {
		if code, _ := call(t, srv, "POST", "/admin/tokens", tok, map[string]any{"name": "sneaky", "role": "admin"}); code != 403 {
			t.Errorf("a %s token made a token: %d", name, code)
		}
		if code, _ := call(t, srv, "GET", "/admin/tokens", tok, nil); code != 403 {
			t.Errorf("a %s token listed the tokens: %d", name, code)
		}
		if code, _ := call(t, srv, "DELETE", fmt.Sprintf("/admin/tokens/%d", id), tok, nil); code != 403 {
			t.Errorf("a %s token revoked a token: %d", name, code)
		}
	}
}

func TestAViewerCanReadButNotChange(t *testing.T) {
	srv, f, _ := adminServer(t, ownerToken)
	viewer, _ := makeToken(t, srv, "dashboard", RoleViewer)

	for _, path := range []string{"/admin/orders", "/admin/stats", "/admin/expenses", "/admin/webhooks", "/admin/whoami"} {
		if code, _ := call(t, srv, "GET", path, viewer, nil); code != 200 {
			t.Errorf("a viewer got %d reading %s", code, path)
		}
	}
	for method, path := range map[string]string{"POST": "/admin/expenses", "DELETE": "/admin/expenses/1", "PUT": "/admin/settings/markup.mtn"} {
		if code, _ := call(t, srv, method, path, viewer, `{"category":"x","label":"x","amount_kobo":1}`); code != 403 {
			t.Errorf("a viewer was allowed %s %s: %d", method, path, code)
		}
	}
	if code, _ := call(t, srv, "GET", "/admin/audit", viewer, nil); code != 403 {
		t.Errorf("a viewer read the audit log: %d", code)
	}
	if f.writes.Load() != 0 {
		t.Error("a viewer changed something")
	}
	// Even the refused attempts are recorded.
	_, audit := call(t, srv, "GET", "/admin/audit?actor=dashboard", ownerToken, nil)
	raw, _ := json.Marshal(audit)
	if !strings.Contains(string(raw), `"route":"POST /admin/expenses"`) || !strings.Contains(string(raw), `"route":"PUT /admin/settings/markup.mtn"`) {
		t.Errorf("a refused attempt should name what was tried: %s", raw)
	}
	if !strings.Contains(string(raw), `"status":403`) || !strings.Contains(string(raw), "viewer token cannot change") {
		t.Errorf("the refused attempts should be in the audit log: %s", raw)
	}
}

func TestARevokedTokenStopsAtOnce(t *testing.T) {
	srv, _, _ := adminServer(t, ownerToken)
	tok, id := makeToken(t, srv, "lost phone", RoleAdmin)
	if code, _ := call(t, srv, "GET", "/admin/orders", tok, nil); code != 200 {
		t.Fatalf("it should work before: %d", code)
	}
	if code, _ := call(t, srv, "DELETE", fmt.Sprintf("/admin/tokens/%d", id), ownerToken, nil); code != 200 {
		t.Fatalf("revoke answered %d", code)
	}
	if code, _ := call(t, srv, "GET", "/admin/orders", tok, nil); code != 401 {
		t.Errorf("a revoked token answered %d, want 401", code)
	}
	if code, _ := call(t, srv, "DELETE", fmt.Sprintf("/admin/tokens/%d", id), ownerToken, nil); code != 404 {
		t.Errorf("revoking twice answered %d, want 404", code)
	}
	if code, _ := call(t, srv, "DELETE", "/admin/tokens/abc", ownerToken, nil); code != 400 {
		t.Errorf("a bad id answered %d, want 400", code)
	}
	_, listed := call(t, srv, "GET", "/admin/tokens", ownerToken, nil)
	raw, _ := json.Marshal(listed)
	if !strings.Contains(string(raw), `"revoked_at":1`) {
		t.Errorf("the list should show it as revoked: %s", raw)
	}
	// The name is free again once it is revoked; the old token stays dead.
	tok2, _ := makeToken(t, srv, "lost phone", RoleAdmin)
	if tok2 == tok {
		t.Fatal("a new token must be different")
	}
	if code, _ := call(t, srv, "GET", "/admin/orders", tok, nil); code != 401 {
		t.Error("the old token must stay revoked")
	}
}

func TestTheAuditLogSaysWhoDidWhat(t *testing.T) {
	srv, _, _ := adminServer(t, ownerToken)
	phone, _ := makeToken(t, srv, "phone", RoleAdmin)

	call(t, srv, "POST", "/admin/expenses", phone, map[string]any{"category": "hosting", "label": "server", "amount_kobo": 600000})
	call(t, srv, "POST", "/admin/expenses", ownerToken, map[string]any{"category": "domain", "label": "dizztech", "amount_kobo": 1500000})
	call(t, srv, "GET", "/admin/orders", phone, nil) // reads are not logged
	call(t, srv, "GET", "/admin/stats", ownerToken, nil)

	code, data := call(t, srv, "GET", "/admin/audit", ownerToken, nil)
	if code != 200 {
		t.Fatalf("audit answered %d", code)
	}
	entries := data["entries"].([]any)
	// Newest first: the owner's expense, the phone's expense, the token being made.
	if len(entries) != 3 {
		t.Fatalf("want 3 entries (token made, 2 expenses; reads are not logged), got %d: %v", len(entries), entries)
	}
	first, second, third := entries[0].(map[string]any), entries[1].(map[string]any), entries[2].(map[string]any)
	if first["actor"] != "owner" || first["method"] != "POST" || first["route"] != "POST /admin/expenses" || first["status"] != 200.0 || !strings.Contains(first["detail"].(string), "dizztech") {
		t.Errorf("newest entry: %v", first)
	}
	if second["actor"] != "phone" || second["role"] != "admin" || second["actor_id"] == 0.0 || !strings.Contains(second["detail"].(string), "hosting") {
		t.Errorf("second entry: %v", second)
	}
	if third["actor"] != "owner" || third["route"] != "POST /admin/tokens" {
		t.Errorf("the token being made should be logged: %v", third)
	}
	if first["ip"] == "" || first["at"].(float64) < 1e9 {
		t.Errorf("the address and time should be recorded: %v", first)
	}

	// Filter, limit and paging.
	_, byPhone := call(t, srv, "GET", "/admin/audit?actor=phone", ownerToken, nil)
	if got := byPhone["entries"].([]any); len(got) != 1 || got[0].(map[string]any)["actor"] != "phone" {
		t.Errorf("filter by actor: %v", got)
	}
	_, page1 := call(t, srv, "GET", "/admin/audit?limit=2", ownerToken, nil)
	last := page1["entries"].([]any)[1].(map[string]any)["id"].(float64)
	_, page2 := call(t, srv, "GET", fmt.Sprintf("/admin/audit?limit=2&before_id=%d", int64(last)), ownerToken, nil)
	if len(page1["entries"].([]any)) != 2 || len(page2["entries"].([]any)) != 1 {
		t.Errorf("paging: %d then %d entries", len(page1["entries"].([]any)), len(page2["entries"].([]any)))
	}
	for _, bad := range []string{"limit=0", "limit=500", "limit=x", "before_id=0", "before_id=x"} {
		if code, _ := call(t, srv, "GET", "/admin/audit?"+bad, ownerToken, nil); code != 400 {
			t.Errorf("?%s answered %d, want 400", bad, code)
		}
	}
	// An admin token can read the log too.
	if code, _ := call(t, srv, "GET", "/admin/audit", phone, nil); code != 200 {
		t.Errorf("an admin token reading the audit log: %d", code)
	}
}

func TestTheAuditLogNeverKeepsASecret(t *testing.T) {
	srv, _, _ := adminServer(t, ownerToken)
	call(t, srv, "POST", "/admin/expenses", ownerToken, `{"category":"x","label":"x","amount_kobo":1,"api_key":"SUPERSECRETVALUE","Token":"ANOTHERSECRET","note":"fine"}`)
	call(t, srv, "POST", "/admin/expenses", ownerToken, "this is not json at all")
	call(t, srv, "POST", "/admin/expenses", ownerToken, `{"label":"`+strings.Repeat("a", 5000)+`"}`)

	_, data := call(t, srv, "GET", "/admin/audit", ownerToken, nil)
	raw, _ := json.Marshal(data)
	text := string(raw)
	if strings.Contains(text, "SUPERSECRETVALUE") || strings.Contains(text, "ANOTHERSECRET") {
		t.Fatalf("a secret reached the audit log: %s", text)
	}
	for _, want := range []string{`[hidden]`, `not JSON`, `too large to keep`, `fine`} {
		if !strings.Contains(text, want) {
			t.Errorf("missing %q in %s", want, text)
		}
	}
}

func TestTokenNamesAndRolesAreChecked(t *testing.T) {
	srv, _, _ := adminServer(t, ownerToken)
	for name, body := range map[string]any{
		"empty name":      map[string]any{"name": "", "role": "admin"},
		"leading space":   map[string]any{"name": " x", "role": "admin"},
		"too long":        map[string]any{"name": strings.Repeat("a", 41), "role": "admin"},
		"odd characters":  map[string]any{"name": "a<b>", "role": "admin"},
		"reserved owner":  map[string]any{"name": "Owner", "role": "admin"},
		"reserved open":   map[string]any{"name": "open", "role": "admin"},
		"role owner":      map[string]any{"name": "x", "role": "owner"},
		"no role":         map[string]any{"name": "x"},
		"not json":        "{",
		"role is a thing": map[string]any{"name": "x", "role": "root"},
	} {
		if code, _ := call(t, srv, "POST", "/admin/tokens", ownerToken, body); code != 400 {
			t.Errorf("%s answered %d, want 400", name, code)
		}
	}
	makeToken(t, srv, "taken", RoleViewer)
	if code, _ := call(t, srv, "POST", "/admin/tokens", ownerToken, map[string]any{"name": "taken", "role": "admin"}); code != 409 {
		t.Errorf("a duplicate name answered %d, want 409", code)
	}
	if code, _ := call(t, srv, "POST", "/admin/tokens", ownerToken, map[string]any{"name": "ok name.1_2-3", "role": "viewer"}); code != 200 {
		t.Errorf("a fine name answered %d", code)
	}
}

func TestUnknownTokensAreRefusedAndCountTowardTheLockout(t *testing.T) {
	srv, _, _ := adminServer(t, ownerToken)
	good, _ := makeToken(t, srv, "phone", RoleAdmin)

	for name, header := range map[string]string{
		"garbage with the prefix": "Bearer " + tokenPrefix + "notarealtoken",
		"no prefix":               "Bearer notarealtoken",
		"not a bearer":            good,
		"empty":                   "Bearer ",
		"another wrong one":       "Bearer " + tokenPrefix + "x",
	} {
		req, _ := http.NewRequest("GET", srv.URL+"/admin/orders", nil)
		req.Header.Set("Authorization", header)
		resp, _ := http.DefaultClient.Do(req)
		resp.Body.Close()
		if resp.StatusCode != 401 && resp.StatusCode != 429 {
			t.Errorf("%s answered %d, want 401", name, resp.StatusCode)
		}
	}
	// By now this address has used up its wrong tries: even the right token waits.
	if code, _ := call(t, srv, "GET", "/admin/orders", good, nil); code != 429 {
		t.Errorf("after repeated wrong tokens even a good token answered %d, want 429", code)
	}
}

func TestLastUseIsRecorded(t *testing.T) {
	srv, _, _ := adminServer(t, ownerToken)
	tok, _ := makeToken(t, srv, "phone", RoleAdmin)
	_, before := call(t, srv, "GET", "/admin/tokens", ownerToken, nil)
	if before["tokens"].([]any)[0].(map[string]any)["last_used_at"] != 0.0 {
		t.Error("a token never used should say so")
	}
	call(t, srv, "GET", "/admin/orders", tok, nil)
	_, after := call(t, srv, "GET", "/admin/tokens", ownerToken, nil)
	used := after["tokens"].([]any)[0].(map[string]any)
	if used["last_used_at"].(float64) < 1e9 || used["last_used_ip"] == "" {
		t.Errorf("last use not recorded: %v", used)
	}
}

func TestWithoutAnOwnerTokenTheRoutesAreOpenAsTheOpenUser(t *testing.T) {
	srv, _, _ := adminServer(t, "") // sandbox only
	code, who := call(t, srv, "GET", "/admin/whoami", "", nil)
	if code != 200 || who["name"] != "open" || who["role"] != "owner" {
		t.Errorf("open mode: %d %v", code, who)
	}
	call(t, srv, "POST", "/admin/expenses", "", map[string]any{"category": "x", "label": "x", "amount_kobo": 1})
	_, data := call(t, srv, "GET", "/admin/audit", "", nil)
	if got := data["entries"].([]any); len(got) != 1 || got[0].(map[string]any)["actor"] != "open" {
		t.Errorf("changes in open mode are still logged: %v", got)
	}
}

func TestOwnersAreToldWhenATokenIsMadeOrRevoked(t *testing.T) {
	srv, f, _ := adminServer(t, ownerToken)
	_, id := makeToken(t, srv, "new phone", RoleAdmin)
	call(t, srv, "DELETE", fmt.Sprintf("/admin/tokens/%d", id), ownerToken, nil)
	got := f.alerts.subjects()
	if len(got) != 2 || !strings.Contains(got[0], "new admin token was made") || !strings.Contains(got[1], "token was revoked") {
		t.Errorf("alerts: %q", got)
	}
}

// Many admins at once: no change is lost from the audit log and the database never says "locked".
func TestManyAdminChangesAtOnceAreAllLogged(t *testing.T) {
	srv, f, _ := adminServer(t, ownerToken)
	tok, _ := makeToken(t, srv, "busy", RoleAdmin)
	var wg sync.WaitGroup
	const n = 30
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			call(t, srv, "POST", "/admin/expenses", tok, map[string]any{"category": "x", "label": fmt.Sprint("e", i), "amount_kobo": 100})
		}(i)
	}
	wg.Wait()
	var logged int
	f.db.QueryRow(`SELECT COUNT(*) FROM audit_log WHERE actor = 'busy'`).Scan(&logged)
	if logged != n {
		t.Errorf("%d of %d changes are in the audit log", logged, n)
	}
}

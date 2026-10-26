package order

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// defaultSettings are what .env would give: the agreed numbers.
func defaultSettings() map[string]string {
	return map[string]string{
		"markup.mtn": "2.5", "markup.mtn_large": "2.2", "markup.large_from_naira": "20000", "markup.airtel": "4", "markup.glo": "5",
		"markup.9mobile": "5", "markup.default": "4", "hidden_plans": "HIDE1,HIDE2",
		"electricity_fee.percent": "1.8", "electricity_fee.min_naira": "50", "payment_fee_warn_percent": "1.7",
		"alerts.email": ownerEmail, "alerts.low_balance_naira": "0", "alerts.check_minutes": "15", "mail.sandbox": "false",
	}
}

var quiet = slog.New(slog.NewTextHandler(io.Discard, nil))

// withSettings puts live settings on the fakes and serves the app and admin routes.
func withSettings(t *testing.T, f *fakes) (*Settings, *httptest.Server) {
	t.Helper()
	s, err := NewSettings(context.Background(), f.svc.Store, defaultSettings(), quiet)
	if err != nil {
		t.Fatal(err)
	}
	s.SetCapability("mail", true)
	f.svc.Settings = s
	api := &API{Service: f.svc}
	mux := http.NewServeMux()
	mux.Handle("/admin/", api.AdminRoutes())
	mux.Handle("/", api.Routes())
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return s, srv
}

func send(t *testing.T, srv *httptest.Server, method, path, body string) (int, map[string]any) {
	t.Helper()
	req, _ := http.NewRequest(method, srv.URL+path, bytes.NewReader([]byte(body)))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var reply map[string]any
	json.NewDecoder(resp.Body).Decode(&reply)
	return resp.StatusCode, reply
}

func put(t *testing.T, srv *httptest.Server, key, value string) (int, map[string]any) {
	return send(t, srv, "PUT", "/admin/settings/"+key, `{"value":`+value+`}`)
}

func TestSettingsListShowsValuesDefaultsAndSource(t *testing.T) {
	f := newFakes(t)
	_, srv := withSettings(t, f)
	put(t, srv, "markup.airtel", "4.5")
	code, reply := send(t, srv, "GET", "/admin/settings", "")
	if code != 200 {
		t.Fatalf("%d %v", code, reply)
	}
	byKey := map[string]map[string]any{}
	for _, item := range reply["data"].(map[string]any)["settings"].([]any) {
		m := item.(map[string]any)
		byKey[m["key"].(string)] = m
	}
	if len(byKey) != len(settingDefs) {
		t.Fatalf("%d settings listed, %d defined", len(byKey), len(settingDefs))
	}
	if m := byKey["markup.airtel"]; m["value"] != 4.5 || m["default"] != float64(4) || m["source"] != "api" || m["available"] != true {
		t.Errorf("airtel: %v", m)
	}
	if m := byKey["markup.glo"]; m["value"] != float64(5) || m["source"] != "default" {
		t.Errorf("glo: %v", m)
	}
	if m := byKey["hidden_plans"]; len(m["value"].([]any)) != 2 || m["kind"] != "list" {
		t.Errorf("hidden: %v", m)
	}
	if m := byKey["mail.sandbox"]; m["value"] != false || m["kind"] != "bool" {
		t.Errorf("sandbox: %v", m)
	}
	if m := byKey["markup.large_from_naira"]; m["value"] != float64(20000) {
		t.Errorf("threshold: %v", m)
	}
}

// A change applies to the next order and the next catalog fetch; orders already made keep their price.
func TestChangingAMarkupAppliesToTheNextOrderOnly(t *testing.T) {
	f := newFakes(t)
	_, srv := withSettings(t, f)
	ctx := context.Background()
	fibre := CreateRequest{IdempotencyKey: "s1", Kind: KindData, PlanID: "F1", Recipient: "FN123456", Email: "a@b.co"} // costs ₦29,000

	before, err := f.svc.Create(ctx, fibre)
	if err != nil {
		t.Fatal(err)
	}
	if before.AmountKobo != 29_640_00 { // 29,000 x 1.022 = 29,638, up to 29,640
		t.Fatalf("before: %d", before.AmountKobo)
	}
	if code, reply := put(t, srv, "markup.mtn_large", "3"); code != 200 {
		t.Fatalf("%d %v", code, reply)
	}
	fibre.IdempotencyKey = "s2"
	after, err := f.svc.Create(ctx, fibre)
	if err != nil {
		t.Fatal(err)
	}
	if after.AmountKobo != 29_870_00 {
		t.Errorf("after: %d, want 2987000", after.AmountKobo)
	}
	if again, _ := f.svc.Store.ByID(ctx, before.ID); again.AmountKobo != 29_640_00 {
		t.Errorf("the earlier order changed price: %d", again.AmountKobo)
	}
	// the catalog shows the new price too
	catalog, _ := f.svc.SellCatalog(ctx)
	if _, plan, _ := catalog.FindDataPlan("F1"); plan.PriceKobo != 29_870_00 {
		t.Errorf("catalog: %d", plan.PriceKobo)
	}
}

func TestSettingsRefuseWhatWouldLoseMoneyOrMakeNoSense(t *testing.T) {
	f := newFakes(t)
	s, srv := withSettings(t, f)
	cases := map[string]struct{ key, value string }{
		"a markup under 2%":             {"markup.glo", "1.9"},
		"a large markup under 2%":       {"markup.mtn_large", "0"},
		"an electricity fee of 1.5%":    {"electricity_fee.percent", "1.5"},
		"a markup that is not a number": {"markup.mtn", `"lots"`},
		"a negative markup":             {"markup.airtel", "-4"},
		"a markup above 100%":           {"markup.airtel", "250"},
		"a fractional naira amount":     {"electricity_fee.min_naira", "50.5"},
		"a check every 0 minutes":       {"alerts.check_minutes", "0"},
		"a bad switch":                  {"mail.sandbox", `"maybe"`},
		"a bad address":                 {"alerts.email", `"not an email"`},
		"a name with an address":        {"alerts.email", `"Owner <me@example.com>"`},
		"a plan id with a space":        {"hidden_plans", `["A B"]`},
		"a list of numbers":             {"hidden_plans", `[1,2]`},
		"an object":                     {"markup.mtn", `{"a":1}`},
	}
	for name, c := range cases {
		code, _ := put(t, srv, c.key, c.value)
		if code != 400 {
			t.Errorf("%s: got %d, want 400", name, code)
		}
	}
	if s.Float("markup.glo") != 5 || s.Float("electricity_fee.percent") != 1.8 || s.Text("alerts.email") != ownerEmail {
		t.Error("a refused change must change nothing")
	}
	if code, _ := send(t, srv, "PUT", "/admin/settings/markup.glo", `{}`); code != 400 {
		t.Errorf("no value: %d", code)
	}
	if code, _ := put(t, srv, "no.such.setting", "1"); code != 404 {
		t.Errorf("unknown key: %d", code)
	}
	if code, _ := send(t, srv, "DELETE", "/admin/settings/no.such.setting", ""); code != 404 {
		t.Errorf("unknown key on reset: %d", code)
	}
	if hist, _ := s.History(context.Background(), 10); len(hist) != 0 {
		t.Errorf("refused changes leave no history: %v", hist)
	}
}

func TestHiddenPlansCanBeChangedLive(t *testing.T) {
	f := newFakes(t)
	_, srv := withSettings(t, f)
	ctx := context.Background()
	req := CreateRequest{IdempotencyKey: "h1", Kind: KindData, PlanID: "P1", Recipient: "08012345678", Email: "a@b.co"}

	if _, err := f.svc.Create(ctx, req); err != nil {
		t.Fatal(err)
	}
	if code, _ := put(t, srv, "hidden_plans", `[" P1 ","P1","HIDE2"]`); code != 200 { // trimmed and de-duplicated
		t.Fatal(code)
	}
	req.IdempotencyKey = "h2"
	var invalid InvalidError
	if _, err := f.svc.Create(ctx, req); !errors.As(err, &invalid) {
		t.Fatalf("a hidden plan must be refused, got %v", err)
	}
	if code, reply := send(t, srv, "GET", "/admin/settings", ""); code != 200 {
		t.Fatal(reply)
	}
	if code, _ := put(t, srv, "hidden_plans", `[]`); code != 200 { // hide nothing
		t.Fatal(code)
	}
	req.IdempotencyKey = "h3"
	if _, err := f.svc.Create(ctx, req); err != nil {
		t.Errorf("nothing is hidden now: %v", err)
	}
	// a comma separated string works as well
	if code, _ := put(t, srv, "hidden_plans", `"P1, X9"`); code != 200 {
		t.Fatal(code)
	}
	catalog, _ := f.svc.SellCatalog(ctx)
	if _, _, ok := catalog.FindDataPlan("P1"); ok {
		t.Error("P1 should be hidden again")
	}
}

func TestElectricityFeeCanBeChangedLive(t *testing.T) {
	f := newFakes(t)
	_, srv := withSettings(t, f)
	put(t, srv, "electricity_fee.min_naira", "100")
	o, err := f.svc.Create(context.Background(), electricityRequest) // ₦1,000 token: 1.8% is 18, so the minimum wins
	if err != nil {
		t.Fatal(err)
	}
	if o.FeeKobo != 100_00 || o.TotalKobo != 1_100_00 {
		t.Errorf("fee %d total %d", o.FeeKobo, o.TotalKobo)
	}
	_, reply := send(t, srv, "GET", "/v1/catalog", "")
	if fee := reply["data"].(map[string]any)["electricity_fee"].(map[string]any); fee["min_kobo"] != float64(100_00) || fee["percent"] != 1.8 {
		t.Errorf("the catalog should publish the new rule: %v", fee)
	}
}

func TestResetGoesBackToTheEnvValue(t *testing.T) {
	f := newFakes(t)
	s, srv := withSettings(t, f)
	put(t, srv, "markup.glo", "7")
	if s.Float("markup.glo") != 7 {
		t.Fatal("not changed")
	}
	code, reply := send(t, srv, "DELETE", "/admin/settings/markup.glo", "")
	view := reply["data"].(map[string]any)
	if code != 200 || view["value"] != float64(5) || view["source"] != "default" {
		t.Fatalf("%d %v", code, reply)
	}
	if code, _ := send(t, srv, "DELETE", "/admin/settings/markup.glo", ""); code != 200 {
		t.Errorf("resetting what is not changed is harmless: %d", code)
	}
}

func TestChangesSurviveARestartAndABadSavedValueIsIgnored(t *testing.T) {
	f := newFakes(t)
	_, srv := withSettings(t, f)
	put(t, srv, "markup.airtel", "6")
	put(t, srv, "alerts.email", `"new@example.com"`)
	// something stricter than today's rules got saved earlier (or someone edited the database)
	if _, err := f.svc.Store.db.Exec(`INSERT INTO settings (key, value, updated_at) VALUES ('markup.glo', '0.5', 1), ('no.such', 'x', 1)`); err != nil {
		t.Fatal(err)
	}
	again, err := NewSettings(context.Background(), f.svc.Store, defaultSettings(), quiet) // a restart
	if err != nil {
		t.Fatal(err)
	}
	if again.Float("markup.airtel") != 6 || again.Text("alerts.email") != "new@example.com" {
		t.Errorf("changes lost: %v %q", again.Float("markup.airtel"), again.Text("alerts.email"))
	}
	if again.Float("markup.glo") != 5 {
		t.Errorf("a saved markup of 0.5%% must not be used, got %v", again.Float("markup.glo"))
	}
}

func TestSettingsHistory(t *testing.T) {
	f := newFakes(t)
	_, srv := withSettings(t, f)
	put(t, srv, "markup.airtel", "5")
	put(t, srv, "markup.airtel", "5") // same value: nothing happened
	put(t, srv, "markup.airtel", "6")
	send(t, srv, "DELETE", "/admin/settings/markup.airtel", "")
	code, reply := send(t, srv, "GET", "/admin/settings/history", "")
	data := reply["data"].(map[string]any)
	changes := data["changes"].([]any)
	if code != 200 || len(changes) != 3 {
		t.Fatalf("%d %v", code, reply)
	}
	newest, oldest := changes[0].(map[string]any), changes[2].(map[string]any)
	if newest["key"] != "markup.airtel" || newest["from"] != "6" || newest["to"] != "4" {
		t.Errorf("newest: %v", newest)
	}
	if oldest["from"] != "4" || oldest["to"] != "5" {
		t.Errorf("oldest: %v", oldest)
	}
	if code, _ := send(t, srv, "GET", "/admin/settings/history?limit=0", ""); code != 400 {
		t.Errorf("limit 0: %d", code)
	}
}

func TestListenersHearOfEveryChange(t *testing.T) {
	f := newFakes(t)
	s, srv := withSettings(t, f)
	calls := 0
	s.OnChange(func() { calls++ })
	put(t, srv, "markup.airtel", "5")
	send(t, srv, "DELETE", "/admin/settings/markup.airtel", "")
	put(t, srv, "markup.glo", "1") // refused: no call
	if calls != 2 {
		t.Errorf("listeners called %d times, want 2", calls)
	}
}

// The email switch and the alert address take effect at once, as main wires them.
func TestMailSandboxAndAlertAddressCanBeChangedLive(t *testing.T) {
	f := newFakes(t)
	s, srv := withSettings(t, f)
	alerts := NewEmailAlerts(f.svc.Mailer, ownerEmail, quiet)
	f.svc.Alerts = alerts
	mailer := f.svc.Mailer
	s.OnChange(func() {
		mailer.SetSandbox(s.Bool("mail.sandbox"))
		alerts.SetTo(s.Text("alerts.email"))
	})
	sentTo := func() []string {
		alerts.Wait()
		f.mu.Lock()
		defer f.mu.Unlock()
		var to []string
		for _, m := range f.allEmails {
			to = append(to, m.To)
		}
		return to
	}

	// switched on: only logged, nothing reaches the mail service
	if code, _ := put(t, srv, "mail.sandbox", "true"); code != 200 {
		t.Fatal(code)
	}
	alerts.Alert("a", 0, "first", "body")
	if got := sentTo(); len(got) != 0 {
		t.Fatalf("sandbox on, yet %v was sent", got)
	}
	// switched off: sent for real, and the owner is told what that means
	code, reply := put(t, srv, "mail.sandbox", "false")
	if code != 200 || !strings.Contains(reply["message"].(string), "real emails") {
		t.Fatalf("%d %v", code, reply)
	}
	alerts.Alert("b", 0, "second", "body")
	if got := sentTo(); len(got) != 1 || got[0] != ownerEmail {
		t.Fatalf("%v", got)
	}
	// a new address is used from the next alert
	put(t, srv, "alerts.email", `"other@example.com"`)
	alerts.Alert("c", 0, "third", "body")
	if got := sentTo(); len(got) != 2 || got[1] != "other@example.com" {
		t.Errorf("%v", got)
	}
	// an empty address stops the emails
	put(t, srv, "alerts.email", `""`)
	alerts.Alert("d", 0, "fourth", "body")
	if got := sentTo(); len(got) != 2 {
		t.Errorf("no address, yet %v", got)
	}
}

func TestEmailSettingsNeedEmailToBeSetUp(t *testing.T) {
	f := newFakes(t)
	s, srv := withSettings(t, f)
	s.SetCapability("mail", false) // as when ZEPTOMAIL_TOKEN is not set
	for _, key := range []string{"alerts.email", "alerts.low_balance_naira", "alerts.check_minutes", "mail.sandbox"} {
		value := "5"
		if key == "alerts.email" {
			value = `"me@example.com"`
		} else if key == "mail.sandbox" {
			value = "true"
		}
		code, reply := put(t, srv, key, value)
		if code != 409 || !strings.Contains(reply["error"].(string), "ZEPTOMAIL_TOKEN") || strings.Contains(reply["error"].(string), "order:") {
			t.Errorf("%s: %d %v", key, code, reply)
		}
		if code, _ := send(t, srv, "DELETE", "/admin/settings/"+key, ""); code != 409 {
			t.Errorf("%s reset: %d", key, code)
		}
	}
	_, reply := send(t, srv, "GET", "/admin/settings", "")
	for _, item := range reply["data"].(map[string]any)["settings"].([]any) {
		m := item.(map[string]any)
		wantAvailable := !strings.HasPrefix(m["key"].(string), "alerts.") && m["key"] != "mail.sandbox"
		if m["available"] != wantAvailable {
			t.Errorf("%v: available = %v", m["key"], m["available"])
		}
	}
	// pricing settings do not need email
	if code, _ := put(t, srv, "markup.airtel", "5"); code != 200 {
		t.Errorf("pricing is unaffected: %d", code)
	}
}

func TestFeeWatchLimitFollowsTheSetting(t *testing.T) {
	f := newFakes(t)
	s, srv := withSettings(t, f)
	if f.svc.feeWarnPercent() != 1.7 {
		t.Fatalf("%v", f.svc.feeWarnPercent())
	}
	put(t, srv, "payment_fee_warn_percent", "3.5")
	if f.svc.feeWarnPercent() != 3.5 || s.Float("payment_fee_warn_percent") != 3.5 {
		t.Errorf("%v", f.svc.feeWarnPercent())
	}
}

func TestSettingsAreOffWithoutThem(t *testing.T) {
	f := newFakes(t) // no Settings
	srv := httptest.NewServer((&API{Service: f.svc}).AdminRoutes())
	defer srv.Close()
	for _, c := range [][2]string{{"GET", "/admin/settings"}, {"GET", "/admin/settings/history"}, {"PUT", "/admin/settings/markup.mtn"}, {"DELETE", "/admin/settings/markup.mtn"}} {
		if code, _ := send(t, srv, c[0], c[1], `{"value":3}`); code != 404 {
			t.Errorf("%s %s: %d", c[0], c[1], code)
		}
	}
	// and the pricing the service was given directly still works
	f.svc.Pricing = realPricing()
	if f.svc.pricing().Markups["mtn"] != 2.5 {
		t.Error("the fixed pricing should be used when there are no settings")
	}
}

func TestWatchBalanceReadsItsSettingsEachRound(t *testing.T) {
	f := newFakes(t) // the fake ABS wallet holds ₦726
	alerts, sent := alertsOn(t, f)
	_ = alerts
	api := &API{Service: f.svc}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		api.WatchBalance(ctx, func() time.Duration { return time.Hour }, func() int64 { return 5_000_00 })
		close(done)
	}()
	deadline := time.Now().Add(3 * time.Second)
	for len(sent()) == 0 && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	if got := sent(); len(got) != 1 || !strings.Contains(got[0], "ABS wallet is low") {
		t.Fatalf("%q", got)
	}
	cancel()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("the watcher did not stop when asked")
	}

	// A threshold of 0 means no check at all.
	f2 := newFakes(t)
	_, sent2 := alertsOn(t, f2)
	api2 := &API{Service: f2.svc}
	ctx2, cancel2 := context.WithCancel(context.Background())
	done2 := make(chan struct{})
	go func() {
		api2.WatchBalance(ctx2, func() time.Duration { return time.Hour }, func() int64 { return 0 })
		close(done2)
	}()
	time.Sleep(200 * time.Millisecond)
	cancel2()
	<-done2
	if got := sent2(); len(got) != 0 {
		t.Errorf("threshold 0 sends nothing: %q", got)
	}
	_ = filepath.Join
}

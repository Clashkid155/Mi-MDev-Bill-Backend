package platform

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// healthFixture is a tracker on a real database with a clock the test moves.
type healthFixture struct {
	*fixture
	h      *ProviderHealth
	events *EventRecorder
	now    time.Time
}

func newHealth(t *testing.T) *healthFixture {
	t.Helper()
	f := newFixture(t)
	events := NewEventRecorder(f.store, f.log)
	t.Cleanup(events.Close)
	hf := &healthFixture{fixture: f, events: events, now: time.Unix(1_800_000_000, 0)}
	hf.h = NewProviderHealth(f.store, f.log, events, f.alerts)
	hf.h.now = func() time.Time { return hf.now }
	return hf
}

func (hf *healthFixture) report(t *testing.T, name string) ProviderReport {
	t.Helper()
	all, err := hf.h.Report(context.Background(), 24)
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range all {
		if r.Name == name {
			return r
		}
	}
	t.Fatalf("no report for %s in %+v", name, all)
	return ProviderReport{}
}

func (hf *healthFixture) kinds(t *testing.T) []string {
	t.Helper()
	hf.events.Flush()
	list, err := hf.store.ListEvents(context.Background(), EventFilter{Kind: "provider.", Limit: 50})
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for i := len(list) - 1; i >= 0; i-- { // oldest first
		out = append(out, list[i].Kind)
	}
	return out
}

func TestWhatCountsAsAFailure(t *testing.T) {
	hf := newHealth(t)
	obs := hf.h.Observer("kora")

	obs("GetCharge", 200, nil, 100*time.Millisecond)
	obs("CreateCharge", 400, nil, 100*time.Millisecond) // our request was refused: the provider was there
	obs("GetCharge", 404, nil, 100*time.Millisecond)
	obs("GetCharge", 500, nil, 100*time.Millisecond) // fails
	obs("GetCharge", 503, nil, 100*time.Millisecond) // fails
	obs("GetCharge", 429, nil, 100*time.Millisecond) // limiting us: fails
	obs("GetCharge", 0, errors.New("timeout"), 20*time.Second)
	obs("GetCharge", 0, context.Canceled, time.Second) // we gave up: not counted at all
	obs("GetCharge", 0, errors.Join(errors.New("wrapped"), context.Canceled), time.Second)

	r := hf.report(t, "kora")
	if r.Calls != 7 || r.Failures != 4 {
		t.Errorf("calls %d failures %d, want 7 and 4", r.Calls, r.Failures)
	}
	if r.SuccessPercent == nil || int(*r.SuccessPercent) != 42 {
		t.Errorf("success percent %v", r.SuccessPercent)
	}
	if r.SlowCalls != 1 {
		t.Errorf("slow calls %d, want 1 (the 20 second timeout)", r.SlowCalls)
	}
	if !strings.Contains(r.LastFailure, "timeout") || !strings.HasPrefix(r.LastFailure, "GetCharge: ") {
		t.Errorf("last failure %q", r.LastFailure)
	}
}

func TestAProviderGoesDownAfterThreeFailuresOverAMinuteAndRecovers(t *testing.T) {
	hf := newHealth(t)
	obs := hf.h.Observer("kora")
	obs("GetCharge", 200, nil, 200*time.Millisecond)
	if got := hf.report(t, "kora").State; got != "healthy" {
		t.Fatalf("state %s, want healthy", got)
	}

	// Three failures in ten seconds: a burst, not an outage.
	for i := 0; i < 3; i++ {
		hf.now = hf.now.Add(3 * time.Second)
		obs("GetCharge", 500, nil, 200*time.Millisecond)
	}
	if r := hf.report(t, "kora"); r.State != "degraded" || r.ConsecutiveFailures != 3 {
		t.Errorf("a short burst: state %s, %d in a row; want degraded", r.State, r.ConsecutiveFailures)
	}
	if got := hf.kinds(t); len(got) != 0 || len(hf.alerts.subjects()) != 0 {
		t.Errorf("a short burst must not raise an alarm: events %v, emails %v", got, hf.alerts.subjects())
	}

	// Still failing a minute after the first failure: down, announced once.
	hf.now = hf.now.Add(70 * time.Second)
	obs("GetCharge", 500, nil, 200*time.Millisecond)
	hf.now = hf.now.Add(10 * time.Second)
	obs("GetCharge", 500, nil, 200*time.Millisecond)
	if r := hf.report(t, "kora"); r.State != "down" {
		t.Errorf("state %s, want down", r.State)
	}
	if got := hf.kinds(t); len(got) != 1 || got[0] != "provider.down" {
		t.Errorf("events %v, want one provider.down", got)
	}
	if got := hf.alerts.subjects(); len(got) != 1 || got[0] != "kora is failing" {
		t.Errorf("emails %v, want one", got)
	}

	// The first good answer ends it, and says so once.
	hf.now = hf.now.Add(time.Minute)
	obs("GetCharge", 200, nil, 200*time.Millisecond)
	obs("GetCharge", 200, nil, 200*time.Millisecond)
	if r := hf.report(t, "kora"); r.State == "down" || r.ConsecutiveFailures != 0 {
		t.Errorf("after recovery: state %s, %d in a row", r.State, r.ConsecutiveFailures)
	}
	if got := hf.kinds(t); len(got) != 2 || got[1] != "provider.recovered" {
		t.Errorf("events %v, want provider.down then provider.recovered", got)
	}
	if len(hf.alerts.subjects()) != 1 {
		t.Error("recovery is an event, not another email")
	}
}

func TestOneBlipDoesNotMakeAProviderLookBroken(t *testing.T) {
	hf := newHealth(t)
	obs := hf.h.Observer("abs")
	for i := 0; i < 30; i++ {
		obs("Catalog", 200, nil, 150*time.Millisecond)
	}
	obs("Catalog", 500, nil, 150*time.Millisecond)
	obs("Catalog", 200, nil, 150*time.Millisecond)
	if r := hf.report(t, "abs"); r.State != "healthy" {
		t.Errorf("one failure in 32 calls: %s", r.State)
	}
	// Many failures in the recent calls, though the last one worked: degraded.
	for i := 0; i < 6; i++ {
		obs("Catalog", 500, nil, 150*time.Millisecond)
		obs("Catalog", 200, nil, 150*time.Millisecond)
	}
	if r := hf.report(t, "abs"); r.State != "degraded" {
		t.Errorf("6 failures in the last 20 calls: %s", r.State)
	}
}

func TestAProviderNeverCalledOrQuietForADayIsIdle(t *testing.T) {
	hf := newHealth(t)
	obs := hf.h.Observer("zeptomail")
	if r := hf.report(t, "zeptomail"); r.State != "idle" || r.SuccessPercent != nil || r.Calls != 0 || len(r.Hours) != 0 {
		t.Errorf("never called: %+v", r)
	}
	obs("SendEmail", 200, nil, time.Second)
	hf.now = hf.now.Add(25 * time.Hour)
	if r := hf.report(t, "zeptomail"); r.State != "idle" {
		t.Errorf("quiet for 25 hours: %s", r.State)
	}
}

func TestCountsArePerHourAndSurviveARestart(t *testing.T) {
	hf := newHealth(t)
	obs := hf.h.Observer("kora")
	obs("GetCharge", 200, nil, 100*time.Millisecond)
	obs("GetCharge", 200, nil, 300*time.Millisecond)
	hf.now = hf.now.Add(2 * time.Hour)
	obs("GetCharge", 500, nil, 600*time.Millisecond)

	r := hf.report(t, "kora")
	if len(r.Hours) != 2 || r.Hours[0].Calls != 2 || r.Hours[0].AvgMs != 200 || r.Hours[1].Failures != 1 || r.Hours[1].Hour-r.Hours[0].Hour < 3600 {
		t.Errorf("hours: %+v", r.Hours)
	}
	if r.Calls != 3 || r.AvgMs != 333 {
		t.Errorf("totals: %d calls, %d ms", r.Calls, r.AvgMs)
	}
	hf.report(t, "kora") // reading flushes; reading again must not double count
	if again := hf.report(t, "kora"); again.Calls != 3 {
		t.Errorf("a second read changed the counts: %d", again.Calls)
	}

	// A new server process: the history is still there, and the live detail starts empty.
	fresh := NewProviderHealth(hf.store, hf.log, nil, nil)
	fresh.now = hf.h.now
	fresh.Observer("kora")
	all, err := fresh.Report(context.Background(), 24)
	if err != nil || len(all) != 1 {
		t.Fatalf("%v %v", all, err)
	}
	if all[0].Calls != 3 || all[0].LastCallAt != 0 || !strings.Contains(all[0].LastFailure, "HTTP 500") || all[0].State != "healthy" {
		t.Errorf("after a restart: %+v", all[0])
	}

	// Counts outside the window are not in the report.
	if old, _ := fresh.Report(context.Background(), 1); old[0].Calls != 1 {
		t.Errorf("the last hour only: %d calls, want 1", old[0].Calls)
	}
}

func TestWhatIsWaitingIsWrittenOnCloseAndOldCountsArePurged(t *testing.T) {
	hf := newHealth(t)
	hf.h.Observer("kora")("GetCharge", 200, nil, 100*time.Millisecond)
	hf.h.Close()
	var n int
	hf.db.QueryRow(`SELECT COALESCE(SUM(calls), 0) FROM provider_stats`).Scan(&n)
	if n != 1 {
		t.Errorf("Close wrote %d calls, want 1", n)
	}

	hf.db.Exec(`INSERT INTO provider_stats (provider, hour, calls) VALUES ('kora', ?, 5)`, hf.now.Add(-100*24*time.Hour).Unix()/3600*3600)
	ctx, stop := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { hf.h.Run(ctx); close(done) }()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		hf.db.QueryRow(`SELECT COUNT(*) FROM provider_stats WHERE calls = 5`).Scan(&n)
		if n == 0 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	stop()
	<-done
	if n != 0 {
		t.Error("counts older than 90 days must be purged when the tracker starts")
	}
}

func TestFailedCallDetailsHideSecretsAndAreShort(t *testing.T) {
	hf := newHealth(t)
	obs := hf.h.Observer("zeptomail")
	obs("SendEmail", 0, errors.New(`Post "https://api.zeptomail.com/v1.1/email": dial tcp: lookup failed for ada@example.com `+strings.Repeat("x", 400)), time.Second)
	r := hf.report(t, "zeptomail")
	if strings.Contains(r.LastFailure, "ada@example.com") || len(r.LastFailure) > errorMax+3 {
		t.Errorf("last failure %q", r.LastFailure)
	}
}

func TestTheProvidersEndpoint(t *testing.T) {
	srv, f, _ := adminServer(t, ownerToken)
	// Left out until a tracker is attached.
	if code := getWith(t, srv.URL+"/admin/providers", ownerToken, nil); code != http.StatusNotFound && code != http.StatusMethodNotAllowed {
		t.Errorf("without a tracker the route should not exist, got %d", code)
	}

	mux := http.NewServeMux()
	f.admin.Providers = NewProviderHealth(f.store, f.log, nil, nil)
	f.admin.Providers.Observer("kora")("GetCharge", 500, nil, time.Second)
	f.admin.Providers.Observer("abs")("Catalog", 200, nil, time.Second)
	f.admin.Register(mux)
	guard := &AdminGuard{Store: f.store, Owner: ownerToken, Log: f.log}
	live := httptest.NewServer(guard.Wrap(mux))
	defer live.Close()

	// Any admin token may read it, a viewer too; no token may not.
	viewer := madeToken(t, live.URL, "viewer")
	for name, tok := range map[string]string{"owner": ownerToken, "viewer": viewer} {
		var out struct {
			Data struct {
				Hours     int `json:"hours"`
				Providers []struct {
					Name  string `json:"name"`
					State string `json:"state"`
					Calls int    `json:"calls"`
				} `json:"providers"`
			} `json:"data"`
		}
		if code := getWith(t, live.URL+"/admin/providers", tok, &out); code != 200 || out.Data.Hours != 24 || len(out.Data.Providers) != 2 ||
			out.Data.Providers[0].Name != "kora" || out.Data.Providers[0].State != "degraded" || out.Data.Providers[1].State != "healthy" {
			t.Errorf("%s: %d %+v", name, code, out.Data)
		}
	}
	if code := getWith(t, live.URL+"/admin/providers", "", nil); code != 401 {
		t.Errorf("no token: %d, want 401", code)
	}
	for _, bad := range []string{"?hours=0", "?hours=169", "?hours=x"} {
		if code := getWith(t, live.URL+"/admin/providers"+bad, ownerToken, nil); code != 400 {
			t.Errorf("%s answered %d, want 400", bad, code)
		}
	}
	if code := getWith(t, live.URL+"/admin/providers?hours=168", ownerToken, nil); code != 200 {
		t.Errorf("a week: %d", code)
	}
}

func getWith(t *testing.T, url, token string, into any) int {
	t.Helper()
	req, _ := http.NewRequest("GET", url, nil)
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if into != nil {
		_ = json.Unmarshal(raw, into)
	}
	return resp.StatusCode
}

// madeToken has the owner make a named token with the role and returns it.
func madeToken(t *testing.T, base, role string) string {
	t.Helper()
	req, _ := http.NewRequest("POST", base+"/admin/tokens", strings.NewReader(`{"name":"test `+role+`","role":"`+role+`"}`))
	req.Header.Set("Authorization", "Bearer "+ownerToken)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var out struct {
		Data struct {
			Token string `json:"token"`
		} `json:"data"`
	}
	json.NewDecoder(resp.Body).Decode(&out)
	if out.Data.Token == "" {
		t.Fatalf("making a %s token answered %d", role, resp.StatusCode)
	}
	return out.Data.Token
}

// Many requests report at once while the counts are being written and read: nothing may race, lose
// a call, or count one twice. (Run with -race.)
func TestManyCallsAtOnceAreAllCountedOnce(t *testing.T) {
	hf := newHealth(t)
	hf.h.now = time.Now // real time: the clock fake is not safe to move from many goroutines
	kora, abs := hf.h.Observer("kora"), hf.h.Observer("abs")

	const workers, each = 20, 250
	stop := make(chan struct{})
	var readers sync.WaitGroup
	readers.Add(1)
	go func() { // reads and flushes all the while
		defer readers.Done()
		for {
			select {
			case <-stop:
				return
			default:
				if _, err := hf.h.Report(context.Background(), 1); err != nil {
					t.Error(err)
					return
				}
			}
		}
	}()
	var writers sync.WaitGroup
	for w := 0; w < workers; w++ {
		writers.Add(1)
		go func(w int) {
			defer writers.Done()
			for i := 0; i < each; i++ {
				status := 200
				if i%10 == 0 {
					status = 500
				}
				kora("GetCharge", status, nil, time.Millisecond)
				abs("Catalog", 200, nil, time.Millisecond)
			}
		}(w)
	}
	writers.Wait()
	close(stop)
	readers.Wait()
	hf.h.Close()

	for name, want := range map[string][2]int64{"kora": {workers * each, workers * each / 10}, "abs": {workers * each, 0}} {
		var calls, failures int64
		hf.db.QueryRow(`SELECT COALESCE(SUM(calls),0), COALESCE(SUM(failures),0) FROM provider_stats WHERE provider = ?`, name).Scan(&calls, &failures)
		if calls != want[0] || failures != want[1] {
			t.Errorf("%s: stored %d calls and %d failures, want %d and %d", name, calls, failures, want[0], want[1])
		}
	}
}

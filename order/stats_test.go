package order

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// seed stores an order in any status, created at the given time.
func seed(t *testing.T, s *Store, o Order, status Status, at time.Time) {
	t.Helper()
	o.IdempotencyKey = "k-" + at.Format("0102150405.000") + string(status) + o.Kind + o.PlanName + o.NetworkName
	if o.Recipient == "" {
		o.Recipient = "08012345678"
	}
	o.CustomerEmail = "a@b.co"
	o.ExpiresAt = at.Add(time.Hour)
	if err := s.Insert(context.Background(), &o); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`UPDATE orders SET status = ?, created_at = ?, payment_fee_kobo = ? WHERE id = ?`, string(status), at.Unix(), o.PaymentFeeKobo, o.ID); err != nil {
		t.Fatal(err)
	}
}

func day(y int, m time.Month, d, h, min int) time.Time {
	return time.Date(y, m, d, h, min, 0, 0, lagos)
}

func statsStore(t *testing.T) *Store {
	t.Helper()
	store, err := OpenStore(filepath.Join(t.TempDir(), "stats.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })

	// 10 Oct
	seed(t, store, Order{Kind: KindData, Network: "n", PlanID: "P1", PlanName: "1GB", NetworkName: "MTN Data",
		AmountKobo: 300_00, CostKobo: 290_00, TotalKobo: 300_00, PaymentFeeKobo: 4_84}, StatusSuccess, day(2026, 10, 10, 23, 30))
	// 11 Oct, just after midnight in Nigeria (still the 10th in UTC)
	seed(t, store, Order{Kind: KindElectricity, Network: "e", NetworkName: "IBEDC",
		AmountKobo: 1_000_00, FeeKobo: 50_00, TotalKobo: 1_050_00, CostKobo: 1_000_00, PaymentFeeKobo: 16_93}, StatusSuccess, day(2026, 10, 11, 0, 30))
	seed(t, store, Order{Kind: KindAirtime, Network: "a", NetworkName: "MTN Airtime",
		AmountKobo: 500_00, CostKobo: 495_00, TotalKobo: 500_00, PaymentFeeKobo: 8_06}, StatusSuccess, day(2026, 10, 11, 9, 0))
	// an order from before costs were kept: counted as revenue, never as profit
	seed(t, store, Order{Kind: KindAirtime, Network: "a", NetworkName: "MTN Airtime", AmountKobo: 100_00}, StatusSuccess, day(2026, 10, 11, 10, 0))
	// 12 Oct
	seed(t, store, Order{Kind: KindAirtime, Network: "a", NetworkName: "Glo Airtime", AmountKobo: 10_000_00, CostKobo: 9_900_00, TotalKobo: 10_000_00,
		PaymentFeeKobo: 161_25}, StatusRefundDue, day(2026, 10, 12, 8, 0))
	seed(t, store, Order{Kind: KindAirtime, Network: "a", NetworkName: "Glo Airtime", AmountKobo: 50_00, CostKobo: 49_00, TotalKobo: 50_00}, StatusFailed, day(2026, 10, 12, 9, 0))
	seed(t, store, Order{Kind: KindData, Network: "n", PlanID: "P2", PlanName: "2GB", NetworkName: "MTN Data", AmountKobo: 200_00, CostKobo: 190_00, TotalKobo: 200_00}, StatusPending, day(2026, 10, 12, 10, 0))
	seed(t, store, Order{Kind: KindAirtime, Network: "a", NetworkName: "Glo Airtime", AmountKobo: 70_00, CostKobo: 69_00}, StatusExpired, day(2026, 10, 12, 11, 0))
	// outside the window
	seed(t, store, Order{Kind: KindAirtime, Network: "a", NetworkName: "MTN Airtime", AmountKobo: 999_00, CostKobo: 900_00}, StatusSuccess, day(2026, 10, 9, 12, 0))
	seed(t, store, Order{Kind: KindAirtime, Network: "a", NetworkName: "MTN Airtime", AmountKobo: 999_00, CostKobo: 900_00}, StatusSuccess, day(2026, 10, 13, 0, 0))
	return store
}

func TestStats(t *testing.T) {
	st, err := statsStore(t).Stats(context.Background(), day(2026, 10, 10, 15, 0), day(2026, 10, 12, 3, 0))
	if err != nil {
		t.Fatal(err)
	}
	if st.From != "2026-10-10" || st.To != "2026-10-12" {
		t.Errorf("window %s to %s", st.From, st.To)
	}
	o := st.Orders
	if o.Total != 8 || o.Succeeded != 4 || o.Undelivered != 2 || o.ByStatus[StatusPending] != 1 || o.ByStatus[StatusExpired] != 1 {
		t.Errorf("orders: %+v", o)
	}
	if want := 2.0 / 6.0; o.FailureRate < want-1e-9 || o.FailureRate > want+1e-9 {
		t.Errorf("failure rate %v, want %v", o.FailureRate, want)
	}

	m := st.Money
	// revenue 300 + 1,050 + 500 + 100 naira; the old order counts here
	if m.RevenueKobo != 1_950_00 || m.ServiceFeesKobo != 50_00 {
		t.Errorf("revenue %d, service fees %d", m.RevenueKobo, m.ServiceFeesKobo)
	}
	// cost and profit only over the three orders whose cost is known
	if m.VendorCostKobo != 1_785_00 || m.PaymentFeesKobo != 29_83 {
		t.Errorf("vendor cost %d, payment fees %d", m.VendorCostKobo, m.PaymentFeesKobo)
	}
	// 5.16 + 33.07 - 3.06 naira
	if m.ProfitKobo != 5_16+33_07-3_06 {
		t.Errorf("profit %d", m.ProfitKobo)
	}
	if want := float64(m.ProfitKobo) / 1_850_00 * 100; m.MarginPercent < want-1e-9 || m.MarginPercent > want+1e-9 {
		t.Errorf("margin %v, want %v", m.MarginPercent, want)
	}
	if m.OrdersWithoutCost != 1 || m.AverageOrderKobo != 1_950_00/4 {
		t.Errorf("without cost %d, average %d", m.OrdersWithoutCost, m.AverageOrderKobo)
	}
	// the 161.25 the provider kept on the refunded payment is a loss
	if m.FeesOnUndeliveredKobo != 161_25 || m.NetProfitKobo != m.ProfitKobo-161_25 {
		t.Errorf("fees on undelivered %d, net %d", m.FeesOnUndeliveredKobo, m.NetProfitKobo)
	}

	// money at risk: the refund waiting, and the order whose outcome is unknown
	if st.AtRisk.Count != 2 || st.AtRisk.TotalKobo != 10_000_00+200_00 ||
		st.AtRisk.ByStatus[StatusRefundDue].TotalKobo != 10_000_00 || st.AtRisk.ByStatus[StatusPending].Count != 1 {
		t.Errorf("at risk: %+v", st.AtRisk)
	}

	kinds := map[string]Breakdown{}
	for _, b := range st.ByKind {
		kinds[b.Name] = b
	}
	if kinds[KindAirtime].Orders != 2 || kinds[KindData].Orders != 1 || kinds[KindElectricity].RevenueKobo != 1_050_00 {
		t.Errorf("by kind: %+v", st.ByKind)
	}
	if len(st.ByNetwork) == 0 || st.ByNetwork[0].Name != "IBEDC" {
		t.Errorf("networks should lead with the biggest earner: %+v", st.ByNetwork)
	}
	if len(st.TopPlans) != 1 || st.TopPlans[0].Name != "1GB" || st.TopPlans[0].Orders != 1 {
		t.Errorf("top plans: %+v", st.TopPlans)
	}

	// Three Nigerian days, with the 00:30 order on the 11th and an empty 12th.
	if len(st.Daily) != 3 {
		t.Fatalf("daily: %+v", st.Daily)
	}
	d10, d11, d12 := st.Daily[0], st.Daily[1], st.Daily[2]
	if d10.Date != "2026-10-10" || d10.Orders != 1 || d10.RevenueKobo != 300_00 {
		t.Errorf("10th: %+v", d10)
	}
	if d11.Date != "2026-10-11" || d11.Orders != 3 || d11.RevenueKobo != 1_650_00 {
		t.Errorf("11th: %+v (the 00:30 order belongs to the 11th in Nigeria)", d11)
	}
	if d12.Date != "2026-10-12" || d12.Orders != 0 || d12.RevenueKobo != 0 {
		t.Errorf("12th: %+v", d12)
	}
}

func TestStatsOnAnEmptyDatabase(t *testing.T) {
	store, _ := OpenStore(filepath.Join(t.TempDir(), "empty.db"))
	defer store.Close()
	st, err := store.Stats(context.Background(), day(2026, 10, 1, 0, 0), day(2026, 10, 2, 0, 0))
	if err != nil {
		t.Fatal(err)
	}
	if st.Orders.Total != 0 || st.Money.RevenueKobo != 0 || st.Money.MarginPercent != 0 || st.Orders.FailureRate != 0 || len(st.Daily) != 2 {
		t.Errorf("%+v", st)
	}
	body, _ := json.Marshal(st)
	if strings.Contains(string(body), "null") {
		t.Errorf("lists must be [] and maps {}, not null: %s", body)
	}
}

func TestStatsEndpoint(t *testing.T) {
	f := newFakes(t)
	f.svc.Store = statsStore(t)
	srv := httptest.NewServer((&API{Service: f.svc}).AdminRoutes())
	defer srv.Close()

	get := func(query string) (int, map[string]any) {
		resp, err := http.Get(srv.URL + "/admin/stats" + query)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		var reply map[string]any
		body, _ := io.ReadAll(resp.Body)
		json.Unmarshal(body, &reply)
		return resp.StatusCode, reply
	}
	code, reply := get("?from=2026-10-10&to=2026-10-12")
	data, _ := reply["data"].(map[string]any)
	if code != 200 || data == nil || data["money"].(map[string]any)["revenue_kobo"].(float64) != 195000 {
		t.Fatalf("%d %v", code, reply)
	}
	if code, _ := get(""); code != 200 {
		t.Errorf("no dates means the last 30 days, got %d", code)
	}
	for name, query := range map[string]string{
		"a bad date": "?from=yesterday",
		"backwards":  "?from=2026-10-12&to=2026-10-10",
		"too long":   "?from=2024-01-01&to=2026-01-01",
		"a bad to":   "?to=2026-13-40",
	} {
		if code, _ := get(query); code != 400 {
			t.Errorf("%s: got %d, want 400", name, code)
		}
	}
}

func TestBalances(t *testing.T) {
	f := newFakes(t)
	newKoraFake(t, f)
	var logs strings.Builder
	f.svc.Vendor.Log = slog.New(slog.NewTextHandler(&logs, nil))
	srv := httptest.NewServer((&API{Service: f.svc}).AdminRoutes())
	defer srv.Close()

	read := func() map[string]map[string]any {
		resp, err := http.Get(srv.URL + "/admin/balances")
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		var reply struct {
			Data map[string]map[string]any `json:"data"`
		}
		json.NewDecoder(resp.Body).Decode(&reply)
		return reply.Data
	}
	got := read()
	if got["abs"]["available_kobo"] != float64(726_00) {
		t.Errorf("abs: %+v", got["abs"])
	}
	if got["payments"]["available_kobo"] != float64(2500_50) || got["payments"]["pending_kobo"] != float64(100_00) {
		t.Errorf("payments: %+v", got["payments"])
	}
	// The profile reply holds personal details and the account's key; none of it is logged.
	if strings.Contains(logs.String(), "owner@example.com") || strings.Contains(logs.String(), "apikey") {
		t.Errorf("the profile reply was logged: %s", logs.String())
	}

	// One account failing does not hide the other.
	f.profileDown.Store(true)
	got = read()
	if got["abs"]["error"] == nil || got["abs"]["available_kobo"] != nil {
		t.Errorf("abs should show an error: %+v", got["abs"])
	}
	if got["payments"]["available_kobo"] != float64(2500_50) {
		t.Errorf("payments should still answer: %+v", got["payments"])
	}
}

func TestBalancesWhenThePaymentProviderCannotSay(t *testing.T) {
	f := newFakes(t) // Monnify, which has no balance call here
	srv := httptest.NewServer((&API{Service: f.svc}).AdminRoutes())
	defer srv.Close()
	resp, err := http.Get(srv.URL + "/admin/balances")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var reply struct {
		Data map[string]map[string]any `json:"data"`
	}
	json.NewDecoder(resp.Body).Decode(&reply)
	if reply.Data["abs"]["available_kobo"] != float64(726_00) || reply.Data["payments"]["error"] == nil {
		t.Errorf("%+v", reply.Data)
	}
}

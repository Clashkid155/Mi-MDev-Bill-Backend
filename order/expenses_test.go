package order

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestExpensesEndToEnd(t *testing.T) {
	f := newFakes(t)
	srv := httptest.NewServer((&API{Service: f.svc}).AdminRoutes())
	defer srv.Close()

	call := func(method, path, body string) (int, map[string]any) {
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

	code, reply := call("POST", "/admin/expenses", `{"day":"2026-10-05","category":" Hosting ","label":"Server for October","amount_kobo":1500000}`)
	first, _ := reply["data"].(map[string]any)
	if code != 200 || first["category"] != "hosting" || first["day"] != "2026-10-05" || first["amount_kobo"] != float64(1500000) {
		t.Fatalf("%d %v", code, reply)
	}
	call("POST", "/admin/expenses", `{"day":"2026-10-20","category":"agent_fee","label":"ABS agent plan, a year","amount_kobo":999900,"note":"9,999 naira"}`)
	call("POST", "/admin/expenses", `{"day":"2026-10-21","category":"hosting","label":"Extra storage","amount_kobo":50000}`)
	call("POST", "/admin/expenses", `{"day":"2026-09-30","category":"domain","label":"Last month","amount_kobo":777}`)
	code, reply = call("GET", "/admin/expenses?from=2026-10-01&to=2026-10-31", "")
	data, _ := reply["data"].(map[string]any)
	if code != 200 || data["total_kobo"] != float64(1500000+999900+50000) {
		t.Fatalf("%d %v", code, reply)
	}
	cats := data["by_category"].([]any)
	if len(cats) != 2 || cats[0].(map[string]any)["category"] != "hosting" || cats[0].(map[string]any)["total_kobo"] != float64(1550000) || cats[0].(map[string]any)["entries"] != float64(2) {
		t.Errorf("by category: %v", cats)
	}
	if list := data["expenses"].([]any); len(list) != 3 || list[0].(map[string]any)["day"] != "2026-10-21" {
		t.Errorf("newest day first, the other month left out: %v", list)
	}

	// no day means today in Nigeria (entered last, so it cannot disturb the October totals above)
	_, reply = call("POST", "/admin/expenses", `{"category":"email","label":"ZeptoMail","amount_kobo":10000}`)
	if today := time.Now().In(lagos).Format(dateLayout); reply["data"].(map[string]any)["day"] != today {
		t.Errorf("default day: %v, want %s", reply["data"], today)
	}

	// refused
	for name, body := range map[string]string{
		"no category": `{"label":"x","amount_kobo":1}`,
		"no label":    `{"category":"x","amount_kobo":1}`,
		"zero amount": `{"category":"x","label":"x","amount_kobo":0}`,
		"negative":    `{"category":"x","label":"x","amount_kobo":-5}`,
		"a bad day":   `{"category":"x","label":"x","amount_kobo":1,"day":"yesterday"}`,
		"broken JSON": `{"category":`,
		"a long note": `{"category":"x","label":"x","amount_kobo":1,"note":"` + strings.Repeat("n", 501) + `"}`,
	} {
		if code, _ := call("POST", "/admin/expenses", body); code != 400 {
			t.Errorf("%s: got %d, want 400", name, code)
		}
	}

	// delete
	id := int64(first["id"].(float64))
	if code, _ := call("DELETE", "/admin/expenses/"+itoa(id), ""); code != 200 {
		t.Errorf("delete: %d", code)
	}
	if code, _ := call("DELETE", "/admin/expenses/"+itoa(id), ""); code != 404 {
		t.Errorf("deleting twice: %d", code)
	}
	if code, _ := call("DELETE", "/admin/expenses/abc", ""); code != 400 {
		t.Errorf("a bad id: %d", code)
	}
	if code, _ := call("GET", "/admin/expenses?from=2026-10-31&to=2026-10-01", ""); code != 400 {
		t.Errorf("backwards window: %d", code)
	}
}

func itoa(n int64) string { b, _ := json.Marshal(n); return string(b) }

func TestExpensesComeOffTheProfit(t *testing.T) {
	store, err := OpenStore(filepath.Join(t.TempDir(), "x.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	ctx := context.Background()
	seed(t, store, Order{Kind: KindData, Network: "n", PlanID: "P", PlanName: "p", NetworkName: "MTN", AmountKobo: 44_970_00, CostKobo: 44_000_00,
		TotalKobo: 44_970_00, PaymentFeeKobo: 725_14}, StatusSuccess, day(2026, 10, 23, 22, 16)) // profit 244.86
	store.AddExpense(ctx, Expense{Day: "2026-10-20", Category: "hosting", Label: "server", AmountKobo: 100_00})
	store.AddExpense(ctx, Expense{Day: "2026-10-23", Category: "email", Label: "zepto", AmountKobo: 20_00})
	store.AddExpense(ctx, Expense{Day: "2026-10-19", Category: "hosting", Label: "outside the window", AmountKobo: 999_00})

	st, err := store.Stats(ctx, day(2026, 10, 20, 0, 0), day(2026, 10, 23, 0, 0))
	if err != nil {
		t.Fatal(err)
	}
	m := st.Money
	if m.ProfitKobo != 244_86 || m.ExpensesKobo != 120_00 || m.ProfitAfterExpensesKobo != 244_86-120_00 {
		t.Fatalf("profit %d, expenses %d, after %d", m.ProfitKobo, m.ExpensesKobo, m.ProfitAfterExpensesKobo)
	}
	if len(m.ExpensesByCategory) != 2 || m.ExpensesByCategory[0].Category != "hosting" {
		t.Errorf("by category: %+v", m.ExpensesByCategory)
	}
	// a quiet window still has the lists, not nulls
	quiet, _ := store.Stats(ctx, day(2026, 1, 1, 0, 0), day(2026, 1, 2, 0, 0))
	if body, _ := json.Marshal(quiet); strings.Contains(string(body), "null") {
		t.Errorf("nulls in %s", body)
	}
}

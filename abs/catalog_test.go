package abs

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

// A trimmed copy of the shapes seen in ABS's real catalog, plus everything the
// app must never be sent.
const sampleSystemData = `{
  "countries": [{"name":"Nigeria"}],
  "cities": [{"name":"Ikeja"}],
  "states": [{"name":"Lagos"}],
  "bank_acc_allowed": [{"name":"GTBank"}],
  "swapairtimeproviders": [{"name":"MTN"}],
  "systemsettings": {"advertheadline":"x"},
  "education_providers": [{"name":"WAEC"}],
  "points_spending": {"locked":true},
  "airtimeproviders": [
    {"name":"MTN Airtime Nigeria","product_trackid":"MTNAIR6TFD","allowed_numbers":"0803, 0806","sellatwhatpercentoff":"99.00","points_price":1000}
  ],
  "data_providers": [
    {"name":"MTN Data Nigeria","product_trackid":"MTN56TFD","allowed_numbers":"0803,0806","bouquetdata":[
      {"trackid":"3JZJO","name":"500MB HOT DEAL","data_size":"500MB","sellatwhatpercentoff":"300.00","recipient_type":"phone","validity_type":"Monthly","validity":"30 days","validity_days":30,"points_awarded":15,"provider_type":11},
      {"trackid":"1Q8VZ","name":"FibreX 50Mbps","data_size":"50mbps FibreX","sellatwhatpercentoff":"29000.00","recipient_type":"fn","validity_type":"FibreX / ODU"},
      {"trackid":"KGEHF","name":"50Mbps unlimited ODU","data_size":"50MBPS ODU","sellatwhatpercentoff":"29000.00","recipient_type":"phone","validity_type":"Monthly","validity":"30 days","validity_days":30},
      {"trackid":"NOSIZE","name":"No size plan","data_size":"","sellatwhatpercentoff":"430.00","recipient_type":"phone","validity_type":"Weekly","validity":"7 days","validity_days":null}
    ]},
    {"name":"Only Fibre","product_trackid":"FIBRE","bouquetdata":[
      {"trackid":"F1","name":"Fibre","sellatwhatpercentoff":"100.00","recipient_type":"fn"}
    ]}
  ],
  "electricity_providers": [
    {"name":"Oyo","trackid":"OUDIU","servicedata":[
      {"trackid":"AST36VAN","name":"IBEDC – Ibadan Electricity Distribution Company(postpaid)","sellatwhatpercentoff":"100"},
      {"trackid":"SVSUEMYU","name":"IBEDC – Ibadan Electricity Distribution Company(prepaid)"}
    ]},
    {"name":"Kogi","trackid":"VBTR23","servicedata":[]},
    {"name":"DSTV","trackid":"B35PZB2I","servicedata":[{"trackid":"W9427YXU","name":"DSTV(prepaid)"}]},
    {"name":"MTN","trackid":"LEAWU7VZ","servicedata":[{"trackid":"BXXITREM","name":"MTN(prepaid)"}]},
    {"name":"NESCO","trackid":"0SM3EWA9","servicedata":[{"trackid":"CLL1HAVO","name":"NESCO"}]}
  ],
  "voucherproviders": [
    {"name":"MTN Airtime Nigeria","product_trackid":"MTNAIR6TFD","bouquetdata":[{"trackid":"HBSI2","name":"MTN 500 NGN Voucher","sellatwhatpercentoff":"500.00"}]}
  ]
}`

func TestParseCatalog(t *testing.T) {
	c, err := parseCatalog(json.RawMessage(sampleSystemData))
	if err != nil {
		t.Fatal(err)
	}

	if len(c.Airtime) != 1 || c.Airtime[0].ID != "MTNAIR6TFD" || strings.Join(c.Airtime[0].NumberPrefixes, ",") != "0803,0806" {
		t.Errorf("airtime: %+v", c.Airtime)
	}

	if len(c.Data) != 2 {
		t.Fatalf("data should keep both networks (FibreX included): %+v", c.Data)
	}
	mtn := c.Data[0]
	var names []string
	for _, cat := range mtn.Categories {
		names = append(names, cat.Name)
	}
	if strings.Join(names, ",") != "Weekly,Monthly,FibreX,ODU" {
		t.Fatalf("categories not grouped in display order, FibreX and ODU separated: %v", names)
	}
	monthly := mtn.Categories[1].Plans[0]
	if monthly.ID != "3JZJO" || monthly.PriceKobo != 30000 || *monthly.ValidityDays != 30 || monthly.RecipientType != "phone" {
		t.Errorf("plan: %+v", monthly)
	}
	if fibre := mtn.Categories[2].Plans[0]; fibre.ID != "1Q8VZ" || fibre.RecipientType != "fn" {
		t.Errorf("FibreX plan should need an fn recipient: %+v", fibre)
	}
	if odu := mtn.Categories[3].Plans[0]; odu.ID != "KGEHF" || odu.RecipientType != "phone" {
		t.Errorf("ODU plan should need a phone recipient: %+v", odu)
	}
	if c.Recipients["fn"].Example != "FN111700" || c.Recipients["phone"].Label == "" {
		t.Errorf("recipient formats: %+v", c.Recipients)
	}

	if len(c.Electricity) != 2 || c.Electricity[0].Name != "Oyo" || c.Electricity[1].Name != "NESCO" {
		t.Fatalf("electricity should keep only Oyo and NESCO: %+v", c.Electricity)
	}
	// The postpaid service carries a fee of 100, so only the prepaid one is offered.
	if len(c.Electricity[0].Services) != 1 || c.Electricity[0].Services[0].ID != "SVSUEMYU" ||
		c.Electricity[0].Services[0].Kind != "prepaid" || c.Electricity[1].Services[0].Kind != "" {
		t.Errorf("services: %+v", c.Electricity)
	}
	if len(c.ChargingFees) != 1 || !strings.Contains(c.ChargingFees[0], "postpaid") {
		t.Errorf("services left out for their fee: %v", c.ChargingFees)
	}
	if _, ok := c.FindElectricityService("AST36VAN"); ok {
		t.Error("a service that charges a fee must not be orderable")
	}

	// Nothing from the dropped sections, or ABS's internal fields, may leak out.
	// The app has no use for vouchers, so voucherproviders is not parsed at all.
	body, _ := json.Marshal(c)
	for _, leaked := range []string{"countries", "cities", "states", "bank", "swap", "systemsettings", "education", "points", "provider_type", "sellatwhat", "advert", "WAEC", "Ikeja", "voucher", "HBSI2"} {
		if strings.Contains(strings.ToLower(string(body)), strings.ToLower(leaked)) {
			t.Errorf("%q leaked into the client catalog: %s", leaked, body)
		}
	}
	for _, empty := range []string{`"plans":null`, `"services":null`, `"units":null`, `"airtime":null`} {
		if strings.Contains(string(body), empty) {
			t.Errorf("found %s; lists must be [] not null", empty)
		}
	}
}

func TestParseCatalogRejectsBadPrice(t *testing.T) {
	bad := `{"data_providers":[{"product_trackid":"N","bouquetdata":[{"trackid":"X","sellatwhatpercentoff":"free","recipient_type":"phone"}]}]}`
	if _, err := parseCatalog(json.RawMessage(bad)); err == nil {
		t.Fatal("unreadable price accepted")
	}
}

func TestCatalogIsCachedAndServedStale(t *testing.T) {
	calls := 0
	c := fakeABS(t, 200, `{"status":true,"data":{"airtimeproviders":[]}}`, func(r *http.Request) { calls++ })
	for i := 0; i < 3; i++ {
		if _, err := c.Catalog(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	if calls != 1 {
		t.Fatalf("ABS called %d times, want 1 (cached)", calls)
	}

	// Cache expired and ABS now failing: the last good copy is still served.
	c.catalogAt = c.catalogAt.Add(-2 * catalogTTL)
	c.baseURL = "http://127.0.0.1:1"
	if _, err := c.Catalog(context.Background()); err != nil {
		t.Fatalf("stale catalog not served: %v", err)
	}
}

func TestGroupAndSortPlans(t *testing.T) {
	days := func(n int) *int { return &n }
	in := []categorizedPlan{
		{"Router", DataPlan{ID: "router", ValidityDays: days(30), PriceKobo: 100}},
		{"Brand New", DataPlan{ID: "new-category", PriceKobo: 1}},
		{"Monthly", DataPlan{ID: "monthly-dear", ValidityDays: days(30), PriceKobo: 900}},
		{"Monthly", DataPlan{ID: "monthly-cheap", ValidityDays: days(30), PriceKobo: 500}},
		{"Monthly", DataPlan{ID: "monthly-open", PriceKobo: 1}},
		{"Daily", DataPlan{ID: "daily", ValidityDays: days(1), PriceKobo: 700}},
		{"Monthly", DataPlan{ID: "monthly-short", ValidityDays: days(14), PriceKobo: 800}},
		{"", DataPlan{ID: "uncategorised", PriceKobo: 5}},
	}
	var got []string
	for _, cat := range groupPlans(in) {
		ids := []string{}
		for _, p := range cat.Plans {
			ids = append(ids, p.ID)
		}
		got = append(got, cat.Name+":"+strings.Join(ids, "+"))
	}
	want := "Daily:daily Monthly:monthly-short+monthly-cheap+monthly-dear+monthly-open Router:router Brand New:new-category Other:uncategorised"
	if strings.Join(got, " ") != want {
		t.Fatalf("got  %s\nwant %s", strings.Join(got, " "), want)
	}
}

func TestValidRecipient(t *testing.T) {
	cases := []struct {
		kind, value string
		want        bool
	}{
		{"fn", "FN111700", true},
		{"fn", "FN11170", false},
		{"fn", "fn111700", false},
		{"fn", "AB111700", false},
		{"fn", "FN1117000", false},
		{"phone", "08012345678", true},
		{"phone", "8012345678", false},
		{"phone", "FN111700", false},
		{"meter", "123", false},
	}
	for _, c := range cases {
		if got := ValidRecipient(c.kind, c.value); got != c.want {
			t.Errorf("%s %q: got %v want %v", c.kind, c.value, got, c.want)
		}
	}
}

func TestDataCategorySplitsFibreXFromODU(t *testing.T) {
	cases := []struct {
		name, validityType, dataSize, recipientType, want string
	}{
		{"FibreX 50Mbps", "FibreX / ODU", "50mbps FibreX", "fn", "FibreX"},
		{"50Mbps unlimited ODU", "FibreX / ODU", "50MBPS ODU", "phone", "ODU"},
		{"Unlabelled", "FibreX / ODU", "unlabelled", "fn", "FibreX"}, // recipient_type wins over an unclear name/size
		{"Unlabelled", "FibreX / ODU", "unlabelled", "phone", "ODU"},
		{"1GB Monthly", "Monthly", "1GB", "phone", "Monthly"}, // untouched outside FibreX / ODU
		// ODU marked only in data_size, ordinary validity_type (the first bug found).
		{"50Mbps unlimited ODU", "Monthly", "50MBPS ODU", "phone", "ODU"},
		// ODU marked only in the plan name, neither data_size nor validity_type
		// says so (the second bug found: real Airtel plans HPLQP/QRZG5).
		{"50MBPS ROUTER UNLIMITED   [ODU ONLY]- 30days", "Monthly", "50MBPS", "phone", "ODU"},
		// A name that merely contains "ODU" as part of another word must not
		// false-positive into ODU.
		{"Produce More Bundle", "Monthly", "1GB", "phone", "Monthly"},
	}
	for _, c := range cases {
		if got := dataCategory(c.name, c.validityType, c.dataSize, c.recipientType); got != c.want {
			t.Errorf("(%q,%q,%q,%q): got %q want %q", c.name, c.validityType, c.dataSize, c.recipientType, got, c.want)
		}
	}
}

// TestDataCategoryAgainstRealCatalog uses verbatim excerpts from the user's
// own ABS catalog (pasted into the assistant's session on 2026-09-21) to
// check the FibreX/ODU split against real data, not just invented samples.
// It caught a real bug: MTN's own "unlimited ODU" plans are NOT labelled
// validity_type "FibreX / ODU" like the docs' example suggested — they carry
// an ordinary "Monthly" validity_type, and data_size ("50MBPS ODU") is the
// only thing that marks them as ODU plans.
func TestDataCategoryAgainstRealCatalog(t *testing.T) {
	const real = `{"status":true,"data":{
	  "data_providers": [
	    {"name":"MTN Data Nigeria","product_trackid":"MTN56TFD","allowed_numbers":"0803","bouquetdata":[
	      {"trackid":"1Q8VZ","name":"FibreX 50Mbps Monthly","data_size":"50mbps FibreX","sellatwhatpercentoff":"29000.00","provider_type":6,"bouquet_type":8,"recipient_type":"fn","validity_type":"FibreX / ODU","validity":"30 days","validity_days":30},
	      {"trackid":"7Z5QU","name":"FibreX 100Mbps Monthly","data_size":"100mbps FirbeX","sellatwhatpercentoff":"44000.00","provider_type":6,"bouquet_type":8,"recipient_type":"fn","validity_type":"FibreX / ODU","validity":"30 days","validity_days":30},
	      {"trackid":"KGEHF","name":"50Mbps unlimited ODU","data_size":"50MBPS ODU","sellatwhatpercentoff":"29000.00","provider_type":6,"bouquet_type":3,"recipient_type":"phone","validity_type":"Monthly","validity":"30 days","validity_days":30},
	      {"trackid":"BBNJ7","name":"100Mbps unlimited ODU","data_size":"100MBPS ODU","sellatwhatpercentoff":"44000.00","provider_type":6,"bouquet_type":3,"recipient_type":"phone","validity_type":"Monthly","validity":"30 days","validity_days":30},
	      {"trackid":"20GBHOT","name":"20GB HOT DEAL","data_size":"20GB","sellatwhatpercentoff":"6100.00","provider_type":11,"bouquet_type":3,"recipient_type":"phone","validity_type":"Monthly","validity":"30 days","validity_days":30}
	    ]},
	    {"name":"Airtel Data Nigeria","product_trackid":"AIRYU78TFD","allowed_numbers":"0701","bouquetdata":[
	      {"trackid":"9G67I","name":"Unlimited 50 90 Days - Data - 50.0 Mbps (90 Days)","data_size":"50mbps","sellatwhatpercentoff":"65000.00","provider_type":6,"bouquet_type":8,"recipient_type":"phone","validity_type":"FibreX / ODU","validity":"90 days","validity_days":90},
	      {"trackid":"TJ4BQ","name":"60mbps  Airtel Unlimited  - 30 days validity","data_size":"60MBPS","sellatwhatpercentoff":"48015.00","provider_type":7,"bouquet_type":3,"recipient_type":"phone","validity_type":"Monthly","validity":"30 days","validity_days":30},
	      {"trackid":"HPLQP","name":"50MBPS ROUTER UNLIMITED   [ODU ONLY]- 30days","data_size":"50MBPS","sellatwhatpercentoff":"23998.00","provider_type":6,"bouquet_type":3,"recipient_type":"phone","display_category":8,"validity_type":"Monthly","validity":"30 days","validity_days":30},
	      {"trackid":"QRZG5","name":"100MBPS ROUTER UNLIMITED [ODU ONLY]- 30days","data_size":"100MBPS","sellatwhatpercentoff":"43500.00","provider_type":6,"bouquet_type":3,"recipient_type":"phone","display_category":8,"validity_type":"Monthly","validity":"30 days","validity_days":30}
	    ]}
	  ]
	}}`

	c, err := parseCatalog(json.RawMessage(mustExtractData(t, real)))
	if err != nil {
		t.Fatal(err)
	}

	categoryOf := func(networkID, planID string) string {
		t.Helper()
		for _, n := range c.Data {
			if n.ID != networkID {
				continue
			}
			for _, cat := range n.Categories {
				for _, p := range cat.Plans {
					if p.ID == planID {
						return cat.Name
					}
				}
			}
		}
		t.Fatalf("plan %s not found under network %s", planID, networkID)
		return ""
	}

	// MTN: real FibreX plans (recipient_type "fn", validity_type "FibreX / ODU").
	if got := categoryOf("MTN56TFD", "1Q8VZ"); got != "FibreX" {
		t.Errorf("1Q8VZ: got %q, want FibreX", got)
	}
	// The 100Mbps FibreX plan has a typo in data_size ("FirbeX"); recipient_type
	// alone must still be enough to classify it correctly.
	if got := categoryOf("MTN56TFD", "7Z5QU"); got != "FibreX" {
		t.Errorf("7Z5QU: got %q, want FibreX", got)
	}
	// MTN: real ODU plans, mislabelled validity_type "Monthly" — the bug this
	// test exists to catch.
	if got := categoryOf("MTN56TFD", "KGEHF"); got != "ODU" {
		t.Errorf("KGEHF: got %q, want ODU (validity_type says Monthly, data_size says ODU)", got)
	}
	if got := categoryOf("MTN56TFD", "BBNJ7"); got != "ODU" {
		t.Errorf("BBNJ7: got %q, want ODU", got)
	}
	// An ordinary monthly bundle must not be swept into ODU by mistake.
	if got := categoryOf("MTN56TFD", "20GBHOT"); got != "Monthly" {
		t.Errorf("20GBHOT: got %q, want Monthly", got)
	}
	// Airtel: its "Unlimited N Mbps" plans use validity_type "FibreX / ODU" but
	// recipient_type "phone" (bought like ordinary data, not with an FN) — ODU.
	if got := categoryOf("AIRYU78TFD", "9G67I"); got != "ODU" {
		t.Errorf("9G67I: got %q, want ODU", got)
	}
	if got := categoryOf("AIRYU78TFD", "TJ4BQ"); got != "Monthly" {
		t.Errorf("TJ4BQ: got %q, want Monthly", got)
	}
	// Airtel: ODU marked only in the plan name ("[ODU ONLY]") — neither
	// validity_type ("Monthly") nor data_size ("50MBPS"/"100MBPS") says so.
	// The bug this addition to the test exists to catch.
	if got := categoryOf("AIRYU78TFD", "HPLQP"); got != "ODU" {
		t.Errorf("HPLQP: got %q, want ODU (name says ODU, data_size and validity_type don't)", got)
	}
	if got := categoryOf("AIRYU78TFD", "QRZG5"); got != "ODU" {
		t.Errorf("QRZG5: got %q, want ODU", got)
	}
}

func mustExtractData(t *testing.T, envelope string) string {
	t.Helper()
	var wrapper struct {
		Data json.RawMessage `json:"data"`
	}
	if err := json.Unmarshal([]byte(envelope), &wrapper); err != nil {
		t.Fatal(err)
	}
	return string(wrapper.Data)
}

func TestElectricityServicesThatChargeAFeeAreLeftOut(t *testing.T) {
	cases := []struct {
		name string
		json string // the fee fields of one service
		kept bool
	}{
		{"no fee fields", ``, true},
		{"zero string", `,"sellatwhatpercentoff":"0"`, true},
		{"zero decimals", `,"sellatwhatpercentoff":"0.00","percentoffforagent":"0.00"`, true},
		{"zero number", `,"sellatwhatpercentoff":0,"percentoffforagent":0`, true},
		{"empty and null", `,"sellatwhatpercentoff":"","percentoffforagent":null`, true},
		{"fee as string", `,"sellatwhatpercentoff":"100"`, false},
		{"fee as number", `,"sellatwhatpercentoff":150`, false},
		{"fee with decimals", `,"sellatwhatpercentoff":"50.00"`, false},
		{"agent fee only", `,"sellatwhatpercentoff":"0","percentoffforagent":"50"`, false},
		{"unreadable is treated as a fee", `,"sellatwhatpercentoff":"n/a"`, false},
	}
	for _, c := range cases {
		raw := `{"electricity_providers":[{"name":"Oyo","servicedata":[{"trackid":"S1","name":"IBEDC(prepaid)"` + c.json + `}]}]}`
		cat, err := parseCatalog(json.RawMessage(raw))
		if err != nil {
			t.Fatal(c.name, err)
		}
		_, found := cat.FindElectricityService("S1")
		if found != c.kept {
			t.Errorf("%s: offered = %t, want %t", c.name, found, c.kept)
		}
		if !c.kept && len(cat.Electricity) != 0 {
			t.Errorf("%s: a region with nothing left to offer must be dropped: %+v", c.name, cat.Electricity)
		}
	}
}

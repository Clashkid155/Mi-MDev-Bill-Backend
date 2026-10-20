package order

import (
	"strings"
	"testing"

	"Bills-Backend/abs"
)

// The numbers decided with the user: MTN 2.5% (2.2% from 20,000), Airtel 4%, Glo 5%.
func realPricing() Pricing {
	return Pricing{
		Markups:               map[string]float64{"mtn": 2.5, "airtel": 4, "glo": 5, "9mobile": 5},
		DefaultMarkup:         4,
		LargeFromKobo:         20_000_00,
		LargeMarkups:          map[string]float64{"mtn": 2.2},
		Hidden:                map[string]bool{"HIDE": true},
		ElectricityFeePercent: 1.8,
		ElectricityFeeMinKobo: 50_00,
	}
}

func TestPriceIsMarkedUpAndRoundedUpToFiveNaira(t *testing.T) {
	p := realPricing()
	cases := []struct {
		network string
		cost    int64 // kobo
		want    int64
	}{
		// 1,000 x 1.025 is exactly 1,025: floating point must not tip it to 1,030.
		{"MTN Data Nigeria", 1_000_00, 1_025_00},
		{"MTN Data Nigeria", 343_00, 355_00},       // 351.575 rounds up to 355
		{"MTN Data Nigeria", 20_000_00, 20_440_00}, // exactly 20,000: the large rate, 2.2%
		{"MTN Data Nigeria", 19_999_00, 20_500_00}, // just under: 2.5% gives 20,498.975 -> 20,500
		{"MTN Data Nigeria", 44_000_00, 44_970_00}, // 44,968 -> 44,970, under the 45,000 official price
		{"MTN Data Nigeria", 29_000_00, 29_640_00},
		{"Airtel Data", 1_440_00, 1_500_00}, // 1,497.60 -> 1,500
		{"Glo Data", 14_250_00 * 10, 149_625_00},
		{"Some New Network", 1_000_00, 1_040_00}, // not known: the default 4%
		{"9mobile Data", 1_000_00, 1_050_00},
	}
	for _, c := range cases {
		if got := p.Price(c.network, c.cost); got != c.want {
			t.Errorf("%s at %d kobo: got %d, want %d", c.network, c.cost, got, c.want)
		}
	}
}

func TestEveryPriceCoversThePaymentFee(t *testing.T) {
	// The payment provider takes 1.6125%. With every markup at least 2%, no plan
	// may ever lose money, at any cost.
	p := realPricing()
	for _, network := range []string{"MTN Data", "Airtel Data", "Glo Data", "Unknown"} {
		for cost := int64(100); cost < 30_000_000; cost += 7_919 {
			price := p.Price(network, cost)
			if price*983_875/1_000_000 < cost { // price after a 1.6125% fee
				t.Fatalf("%s: cost %d sold at %d loses money after the fee", network, cost, price)
			}
		}
	}
}

func TestZeroPricingChangesNothing(t *testing.T) {
	var p Pricing
	if got := p.Price("MTN Data", 29_900); got != 29_900 {
		t.Errorf("no markup should leave the price alone, got %d", got)
	}
	if got := p.ElectricityFee(1_000_00); got != 0 {
		t.Errorf("no electricity fee configured, got %d", got)
	}
	c := abs.Catalog{Data: []abs.DataNetwork{{Name: "MTN", Categories: []abs.DataCategory{{Name: "Daily", Plans: []abs.DataPlan{{ID: "a", PriceKobo: 123}}}}}}}
	if got := p.Apply(c); got.Data[0].Categories[0].Plans[0].PriceKobo != 123 {
		t.Errorf("the catalog should come back unchanged: %+v", got)
	}
}

func TestElectricityFee(t *testing.T) {
	p := realPricing()
	cases := map[int64]int64{ // amount -> fee, both in naira
		1_000:   50, // 1.8% is 18: the minimum wins
		2_000:   50, // 36
		3_333:   60, // 59.994 rounds up
		5_000:   90,
		10_000:  180,
		50_000:  900,
		100_000: 1_800, // no cap
	}
	for amount, want := range cases {
		if got := p.ElectricityFee(amount * 100); got != want*100 {
			t.Errorf("fee on ₦%d: got ₦%d, want ₦%d", amount, got/100, want)
		}
	}
	// Whatever the token, the fee covers the payment provider's 1.6125% and then some.
	for amount := int64(1_000); amount <= 300_000; amount += 137 {
		total := amount*100 + p.ElectricityFee(amount*100)
		if total*983_875/1_000_000 < amount*100 {
			t.Fatalf("a ₦%d token loses money after the payment fee", amount)
		}
	}
}

func TestApplyHidesPlansAndMarksUpWithoutTouchingTheOriginal(t *testing.T) {
	original := abs.Catalog{Data: []abs.DataNetwork{
		{Name: "MTN Data", Categories: []abs.DataCategory{
			{Name: "Daily", Plans: []abs.DataPlan{{ID: "a", PriceKobo: 1_000_00, CostKobo: 1_000_00}, {ID: "HIDE", PriceKobo: 500_00}}},
			{Name: "Weekly", Plans: []abs.DataPlan{{ID: "HIDE", PriceKobo: 9_00}}}, // nothing else: the category goes
		}},
		{Name: "Glo Data", Categories: []abs.DataCategory{{Name: "Daily", Plans: []abs.DataPlan{{ID: "HIDE"}}}}}, // the network goes
	}}
	got := realPricing().Apply(original)
	if len(got.Data) != 1 || len(got.Data[0].Categories) != 1 || len(got.Data[0].Categories[0].Plans) != 1 {
		t.Fatalf("hidden plans and what they leave empty should be dropped: %+v", got.Data)
	}
	plan := got.Data[0].Categories[0].Plans[0]
	if plan.ID != "a" || plan.PriceKobo != 1_025_00 || plan.CostKobo != 1_000_00 {
		t.Errorf("plan: %+v", plan)
	}
	if original.Data[0].Categories[0].Plans[0].PriceKobo != 1_000_00 || len(original.Data) != 2 || len(original.Data[0].Categories[0].Plans) != 2 {
		t.Errorf("the catalog that was passed in must not change: %+v", original.Data)
	}
}

func TestPricingValidate(t *testing.T) {
	if err := realPricing().Validate(); err != nil {
		t.Fatalf("the agreed numbers must pass: %v", err)
	}
	if err := (Pricing{}).Validate(); err != nil {
		t.Fatalf("no pricing at all is allowed: %v", err)
	}
	bad := map[string]func(*Pricing){
		"a network markup under 2%":  func(p *Pricing) { p.Markups["glo"] = 1.9 },
		"a large markup under 2%":    func(p *Pricing) { p.LargeMarkups["mtn"] = 1.5 },
		"a default markup under 2%":  func(p *Pricing) { p.DefaultMarkup = 0 },
		"an electricity fee of 1.5%": func(p *Pricing) { p.ElectricityFeePercent = 1.5 },
	}
	for name, change := range bad {
		p := realPricing()
		change(&p)
		err := p.Validate()
		if err == nil || !strings.Contains(err.Error(), "%") {
			t.Errorf("%s should be refused, got %v", name, err)
		}
	}
}

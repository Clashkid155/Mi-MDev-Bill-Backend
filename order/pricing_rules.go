package order

import (
	"fmt"
	"math"
	"strings"

	"Bills-Backend/abs"
)

// The payment provider takes about 1.6125% of every payment (Kora: 1.5% plus
// VAT). A markup under minMarkupPercent would sell at a loss, so the server
// refuses to start with one; an electricity fee needs at least
// minElectricityFeePercent for the same reason.
const (
	minMarkupPercent         = 2.0
	minElectricityFeePercent = 1.7
)

// Pricing turns what ABS charges us into what the customer pays. The zero value
// changes nothing: no markup, no hidden plans, no electricity fee.
//
// Percentages are percent over the ABS price. They are looked up by network
// ("mtn", "airtel", "glo", "9mobile"); DefaultMarkup covers any other network.
// From LargeFromKobo up (a plan's ABS price), LargeMarkups replace Markups for
// the networks they list: the official price of a big plan is only 2% or 3%
// above ABS's, and a small markup keeps it at or under the official price.
type Pricing struct {
	Markups       map[string]float64
	DefaultMarkup float64
	LargeFromKobo int64
	LargeMarkups  map[string]float64

	// Hidden lists data plan ids that are not offered at all.
	Hidden map[string]bool

	// ElectricityFeePercent and ElectricityFeeMinKobo make the service fee on
	// electricity: the greater of the two, in whole naira, with no cap. ABS gives
	// no discount on electricity, so the fee is what pays for the payment fee.
	ElectricityFeePercent float64
	ElectricityFeeMinKobo int64
}

// networkKey says which network a display name belongs to: "MTN Data Nigeria"
// is "mtn". It returns "" for a network it does not know.
func networkKey(name string) string {
	lower := strings.ToLower(name)
	switch {
	case strings.Contains(lower, "mtn"):
		return "mtn"
	case strings.Contains(lower, "airtel"):
		return "airtel"
	case strings.Contains(lower, "glo"):
		return "glo"
	case strings.Contains(lower, "9mobile"), strings.Contains(lower, "t2"), strings.Contains(lower, "etisalat"):
		return "9mobile"
	}
	return ""
}

func (p Pricing) isZero() bool {
	return len(p.Markups) == 0 && p.DefaultMarkup == 0 && len(p.LargeMarkups) == 0 && len(p.Hidden) == 0
}

// markup is the percentage that applies to a plan with this network name and ABS price.
func (p Pricing) markup(networkName string, costKobo int64) float64 {
	key := networkKey(networkName)
	if p.LargeFromKobo > 0 && costKobo >= p.LargeFromKobo {
		if v, ok := p.LargeMarkups[key]; ok {
			return v
		}
	}
	if v, ok := p.Markups[key]; ok {
		return v
	}
	return p.DefaultMarkup
}

// percentMilli turns 2.2 (percent) into 2200 so that money is worked out in
// whole numbers; floating point would turn 205000 kobo into 205000.0000000001
// and round it up to the next 5 naira.
func percentMilli(percent float64) int64 { return int64(math.Round(percent * 1000)) }

// ceilDiv is a/b rounded up, for positive numbers.
func ceilDiv(a, b int64) int64 { return (a + b - 1) / b }

// Price is what a customer pays for a plan that costs costKobo at ABS: the
// markup added, then rounded up to the next 5 naira. With no markup the price is the cost.
func (p Pricing) Price(networkName string, costKobo int64) int64 {
	pm := percentMilli(p.markup(networkName, costKobo))
	if pm <= 0 {
		return costKobo
	}
	const fiveNaira = 5 * kobo
	return ceilDiv(costKobo*(100_000+pm), 100_000*fiveNaira) * fiveNaira
}

// ElectricityFee is the service fee for a token of amountKobo: at least the
// minimum, or the percentage rounded up to whole naira, whichever is more.
func (p Pricing) ElectricityFee(amountKobo int64) int64 {
	pm := percentMilli(p.ElectricityFeePercent)
	fee := int64(0)
	if pm > 0 {
		fee = ceilDiv(amountKobo*pm, 100_000*kobo) * kobo
	}
	return max(fee, p.ElectricityFeeMinKobo)
}

// Apply returns the catalog the customer is shown: hidden plans removed, every
// data plan's price marked up (its cost stays in CostKobo, which is never sent
// to the app), and categories or networks left empty dropped. The catalog it is
// given is not changed.
func (p Pricing) Apply(c abs.Catalog) abs.Catalog {
	if p.isZero() {
		return c
	}
	networks := make([]abs.DataNetwork, 0, len(c.Data))
	for _, network := range c.Data {
		kept := network
		kept.Categories = nil
		for _, category := range network.Categories {
			plans := make([]abs.DataPlan, 0, len(category.Plans))
			for _, plan := range category.Plans {
				if p.Hidden[plan.ID] {
					continue
				}
				if plan.CostKobo == 0 {
					plan.CostKobo = plan.PriceKobo
				}
				plan.PriceKobo = p.Price(network.Name, plan.CostKobo)
				plans = append(plans, plan)
			}
			if len(plans) > 0 {
				kept.Categories = append(kept.Categories, abs.DataCategory{Name: category.Name, Plans: plans})
			}
		}
		if len(kept.Categories) > 0 {
			networks = append(networks, kept)
		}
	}
	c.Data = networks
	return c
}

// Validate refuses settings that would sell at a loss.
func (p Pricing) Validate() error {
	check := func(label string, percent float64) error {
		if percent < minMarkupPercent {
			return fmt.Errorf("%s is %v%%; every markup must be at least %v%% or plans sell at a loss after the payment fee", label, percent, minMarkupPercent)
		}
		return nil
	}
	if p.ElectricityFeePercent != 0 && p.ElectricityFeePercent < minElectricityFeePercent {
		return fmt.Errorf("the electricity fee is %v%%; it must be at least %v%% or electricity sells at a loss", p.ElectricityFeePercent, minElectricityFeePercent)
	}
	if len(p.Markups) == 0 && p.DefaultMarkup == 0 && len(p.LargeMarkups) == 0 {
		return nil // no markup at all is allowed (hiding plans alone, or tests)
	}
	if err := check("the default markup", p.DefaultMarkup); err != nil {
		return err
	}
	for network, percent := range p.Markups {
		if err := check("the "+network+" markup", percent); err != nil {
			return err
		}
	}
	for network, percent := range p.LargeMarkups {
		if err := check("the large plan "+network+" markup", percent); err != nil {
			return err
		}
	}
	return nil
}

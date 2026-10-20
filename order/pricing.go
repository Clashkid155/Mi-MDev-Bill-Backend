package order

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/mail"
	"regexp"
	"strings"

	"Bills-Backend/abs"
)

const (
	KindAirtime     = "airtime"
	KindData        = "data"
	KindElectricity = "electricity"

	kobo               = 100
	minAirtimeKobo     = 50 * kobo
	minElectricityKobo = 1000 * kobo
)

var meterNumber = regexp.MustCompile(`^\d{6,}$`)

// InvalidError is a problem with the request itself (HTTP 400), not a server fault.
type InvalidError struct{ Message string }

func (e InvalidError) Error() string { return e.Message }

// CreateRequest is what the app sends to start a purchase. Which fields apply
// depends on Kind:
//
//	airtime      network, recipient (phone), amount_kobo
//	data         plan_id, recipient (phone, or FibreX FN for fibre plans)
//	electricity  service_id, recipient (meter number), amount_kobo
//
// The price of data always comes from the catalog, never from the app.
// Electricity meters are not checked here: the app is expected to have called
// POST /v1/meters/validate and shown the owner to the customer first.
type CreateRequest struct {
	IdempotencyKey string `json:"idempotency_key"`
	Kind           string `json:"kind"`
	Email          string `json:"email"`
	// CustomerName is optional; without it Monnify is given the part of the
	// email before the @.
	CustomerName string `json:"customer_name"`

	Network    string `json:"network"`
	PlanID     string `json:"plan_id"`
	ServiceID  string `json:"service_id"`
	Recipient  string `json:"recipient"`
	AmountKobo int64  `json:"amount_kobo"`
}

const maxCustomerName = 100

func (r CreateRequest) validate() error {
	if strings.TrimSpace(r.IdempotencyKey) == "" {
		return InvalidError{"idempotency_key is required"}
	}
	if _, err := mail.ParseAddress(r.Email); err != nil {
		return InvalidError{"a valid email is required"}
	}
	if len([]rune(strings.TrimSpace(r.CustomerName))) > maxCustomerName {
		return InvalidError{fmt.Sprintf("customer_name must be at most %d characters", maxCustomerName)}
	}
	switch r.Kind {
	case KindAirtime, KindData, KindElectricity:
		return nil
	}
	return InvalidError{"kind must be airtime, data or electricity"}
}

// withinMinimumPayment is the catalog the app is shown when the payment
// provider has a minimum payment (Kora: ₦100). Data plans that cost less
// cannot be paid for, so they are left out, along with any category that
// ends up empty. Prices are never changed. A minimum of 0 returns the catalog as is.
func withinMinimumPayment(c abs.Catalog, minPayment int64) abs.Catalog {
	if minPayment <= 0 {
		return c
	}
	networks := make([]abs.DataNetwork, 0, len(c.Data))
	for _, network := range c.Data {
		kept := network
		kept.Categories = nil
		for _, category := range network.Categories {
			plans := make([]abs.DataPlan, 0, len(category.Plans))
			for _, plan := range category.Plans {
				if plan.PriceKobo >= minPayment {
					plans = append(plans, plan)
				}
			}
			if len(plans) > 0 {
				kept.Categories = append(kept.Categories, abs.DataCategory{Name: category.Name, Plans: plans})
			}
		}
		networks = append(networks, kept)
	}
	c.Data = networks
	return c
}

// product is what an order buys, worked out from the request and the catalog.
type product struct {
	Network    string
	PlanID     string
	Recipient  string
	AmountKobo int64
	// CostKobo is what ABS charges us; FeeKobo the service fee added for the customer.
	CostKobo int64
	FeeKobo  int64
	// Display names as the catalog has them right now. They are stored on the
	// order as a snapshot, so a receipt still says what was bought after ABS
	// renames or removes it.
	NetworkName string
	PlanName    string
}

// resolve checks the request against ABS's live catalog and works out what is
// being bought and what it costs.
func (s *Service) resolve(ctx context.Context, r CreateRequest) (product, error) {
	catalog, err := s.SellCatalog(ctx)
	if err != nil {
		return product{}, fmt.Errorf("catalog: %w", err)
	}
	switch r.Kind {
	case KindAirtime:
		return resolveAirtime(catalog, r, s.MinPaymentKobo)
	case KindData:
		return resolveData(catalog, r, s.MinPaymentKobo)
	default:
		p, err := resolveElectricity(catalog, r, s.MinPaymentKobo)
		p.FeeKobo = s.Pricing.ElectricityFee(p.AmountKobo)
		return p, err
	}
}

func resolveAirtime(catalog abs.Catalog, r CreateRequest, minPayment int64) (product, error) {
	network, ok := catalog.FindAirtimeNetwork(r.Network)
	if !ok {
		return product{}, InvalidError{"unknown airtime network"}
	}
	phone := strings.TrimSpace(r.Recipient)
	if !abs.ValidRecipient("phone", phone) {
		return product{}, InvalidError{"recipient must be an 11-digit phone number like 08012345678"}
	}
	if minimum := max(minAirtimeKobo, minPayment); r.AmountKobo < minimum || r.AmountKobo%kobo != 0 {
		return product{}, InvalidError{fmt.Sprintf("amount must be at least ₦%d, in whole naira", minimum/kobo)}
	}
	// Airtime is sold at face value; ABS charges a little less (CostPercent per 100).
	cost := int64(math.Round(float64(r.AmountKobo) * network.CostPercent / 100))
	if network.CostPercent <= 0 {
		cost = r.AmountKobo
	}
	return product{Network: r.Network, Recipient: phone, AmountKobo: r.AmountKobo, CostKobo: cost, NetworkName: network.Name}, nil
}

func resolveData(catalog abs.Catalog, r CreateRequest, minPayment int64) (product, error) {
	network, plan, ok := catalog.FindDataPlan(r.PlanID)
	if !ok {
		return product{}, InvalidError{"unknown data plan"}
	}
	recipient := strings.TrimSpace(r.Recipient)
	if plan.RecipientType == "fn" {
		recipient = strings.ToUpper(recipient)
	}
	if !abs.ValidRecipient(plan.RecipientType, recipient) {
		format := catalog.Recipients[plan.RecipientType]
		return product{}, InvalidError{fmt.Sprintf("recipient must be a %s like %s", strings.ToLower(format.Label), format.Example)}
	}
	if plan.PriceKobo <= 0 {
		return product{}, InvalidError{"this plan has no price right now"}
	}
	if plan.PriceKobo < minPayment {
		return product{}, InvalidError{"this plan costs less than the minimum payment and cannot be bought"}
	}
	cost := plan.CostKobo
	if cost == 0 {
		cost = plan.PriceKobo
	}
	return product{Network: network.ID, PlanID: plan.ID, Recipient: recipient, AmountKobo: plan.PriceKobo, CostKobo: cost,
		NetworkName: network.Name, PlanName: plan.Name}, nil
}

// resolveElectricity checks the request's shape only. It does not call ABS's
// validate-meter endpoint: the app already validated the meter with
// POST /v1/meters/validate and showed the owner to the customer, so asking
// ABS again here would be a second, redundant round trip.
func resolveElectricity(catalog abs.Catalog, r CreateRequest, minPayment int64) (product, error) {
	service, ok := catalog.FindElectricityService(r.ServiceID)
	if !ok {
		return product{}, InvalidError{"unknown electricity service"}
	}
	meter := strings.TrimSpace(r.Recipient)
	if !meterNumber.MatchString(meter) {
		return product{}, InvalidError{"recipient must be a meter number (digits only, at least 6)"}
	}
	if minimum := max(minElectricityKobo, minPayment); r.AmountKobo < minimum || r.AmountKobo%kobo != 0 {
		return product{}, InvalidError{fmt.Sprintf("amount must be at least ₦%d, in whole naira", minimum/kobo)}
	}
	// ABS gives no discount on electricity: the cost is the amount.
	return product{Network: r.ServiceID, Recipient: meter, AmountKobo: r.AmountKobo, CostKobo: r.AmountKobo, NetworkName: service.Name}, nil
}

// ValidateMeter asks ABS who a meter belongs to (free, nothing is charged), for
// POST /v1/meters/validate: the app calls this on its own, before the customer
// pays, and shows the owner to them. A meter ABS does not accept is an
// InvalidError; any other failure is passed on.
func (s *Service) ValidateMeter(ctx context.Context, serviceID, meter string) (json.RawMessage, error) {
	/*catalog, err := s.Vendor.Catalog(ctx)
	if err != nil {
		return nil, fmt.Errorf("catalog: %w", err)
	}
	if _, ok := catalog.FindElectricityService(serviceID); !ok {
		return nil, InvalidError{"unknown electricity DISCO"}
	}*/
	details, err := s.Vendor.ValidateMeter(ctx, serviceID, meter)
	if rejected, ok := errors.AsType[abs.Rejected](err); ok {
		//return nil, InvalidError{"this meter was not accepted: " + rejected.Reason}
		return nil, InvalidError{rejected.Reason}
	}
	return details, err
}

// buy sends the order to ABS.
func (s *Service) buy(ctx context.Context, o Order) abs.Result {
	switch o.Kind {
	case KindAirtime:
		return s.Vendor.Airtime(ctx, o.Network, o.Recipient, o.AmountKobo)
	case KindData:
		return s.Vendor.Data(ctx, o.Network, o.PlanID, o.Recipient)
	case KindElectricity:
		return s.Vendor.Electricity(ctx, o.Network, o.Recipient, o.AmountKobo)
	}
	return abs.Result{State: abs.Failed, Message: "unknown order kind " + o.Kind}
}

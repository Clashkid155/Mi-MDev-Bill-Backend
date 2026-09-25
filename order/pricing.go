package order

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
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

	Network    string `json:"network"`
	PlanID     string `json:"plan_id"`
	ServiceID  string `json:"service_id"`
	Recipient  string `json:"recipient"`
	AmountKobo int64  `json:"amount_kobo"`
}

func (r CreateRequest) validate() error {
	if strings.TrimSpace(r.IdempotencyKey) == "" {
		return InvalidError{"idempotency_key is required"}
	}
	if _, err := mail.ParseAddress(r.Email); err != nil {
		return InvalidError{"a valid email is required"}
	}
	switch r.Kind {
	case KindAirtime, KindData, KindElectricity:
		return nil
	}
	return InvalidError{"kind must be airtime, data or electricity"}
}

// product is what an order buys, worked out from the request and the catalog.
type product struct {
	Network    string
	PlanID     string
	Recipient  string
	AmountKobo int64
}

// resolve checks the request against ABS's live catalog and works out what is
// being bought and what it costs.
func (s *Service) resolve(ctx context.Context, r CreateRequest) (product, error) {
	catalog, err := s.Vendor.Catalog(ctx)
	if err != nil {
		return product{}, fmt.Errorf("catalog: %w", err)
	}
	switch r.Kind {
	case KindAirtime:
		return resolveAirtime(catalog, r)
	case KindData:
		return resolveData(catalog, r)
	default:
		return resolveElectricity(catalog, r)
	}
}

func resolveAirtime(catalog abs.Catalog, r CreateRequest) (product, error) {
	if _, ok := catalog.FindAirtimeNetwork(r.Network); !ok {
		return product{}, InvalidError{"unknown airtime network"}
	}
	phone := strings.TrimSpace(r.Recipient)
	if !abs.ValidRecipient("phone", phone) {
		return product{}, InvalidError{"recipient must be an 11-digit phone number like 08012345678"}
	}
	if r.AmountKobo < minAirtimeKobo || r.AmountKobo%kobo != 0 {
		return product{}, InvalidError{"amount must be at least ₦50, in whole naira"}
	}
	return product{Network: r.Network, Recipient: phone, AmountKobo: r.AmountKobo}, nil
}

func resolveData(catalog abs.Catalog, r CreateRequest) (product, error) {
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
	return product{Network: network.ID, PlanID: plan.ID, Recipient: recipient, AmountKobo: plan.PriceKobo}, nil
}

// resolveElectricity checks the request's shape only. It does not call ABS's
// validate-meter endpoint: the app already validated the meter with
// POST /v1/meters/validate and showed the owner to the customer, so asking
// ABS again here would be a second, redundant round trip.
func resolveElectricity(catalog abs.Catalog, r CreateRequest) (product, error) {
	if _, ok := catalog.FindElectricityService(r.ServiceID); !ok {
		return product{}, InvalidError{"unknown electricity service"}
	}
	meter := strings.TrimSpace(r.Recipient)
	if !meterNumber.MatchString(meter) {
		return product{}, InvalidError{"recipient must be a meter number (digits only, at least 6)"}
	}
	if r.AmountKobo < minElectricityKobo || r.AmountKobo%kobo != 0 {
		return product{}, InvalidError{"amount must be at least ₦1000, in whole naira"}
	}
	return product{Network: r.ServiceID, Recipient: meter, AmountKobo: r.AmountKobo}, nil
}

// ValidateMeter asks ABS who a meter belongs to (free, nothing is charged), for
// POST /v1/meters/validate: the app calls this on its own, before the customer
// pays, and shows the owner to them. A meter ABS does not accept is an
// InvalidError; any other failure is passed on.
func (s *Service) ValidateMeter(ctx context.Context, serviceID, meter string) (json.RawMessage, error) {
	catalog, err := s.Vendor.Catalog(ctx)
	if err != nil {
		return nil, fmt.Errorf("catalog: %w", err)
	}
	if _, ok := catalog.FindElectricityService(serviceID); !ok {
		return nil, InvalidError{"unknown electricity service"}
	}
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

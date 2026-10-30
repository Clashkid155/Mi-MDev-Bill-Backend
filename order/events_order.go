package order

import (
	"fmt"

	"Bills-Backend/platform"
)

// The event levels, as the bills code writes them.
const (
	LevelInfo  = platform.LevelInfo
	LevelWarn  = platform.LevelWarn
	LevelError = platform.LevelError
)

// event records something about an order's life, if events are switched on.
func (s *Service) event(level, kind, orderID, message string, detail map[string]any) {
	s.Events.Record(level, kind, orderID, message, detail)
}

// describe says what an order is, with no customer details: "airtime ₦500.00" or "data MTN 1GB Daily".
func describe(o Order) string {
	what := o.Kind
	if o.PlanName != "" {
		what += " " + o.PlanName
	} else if o.NetworkName != "" {
		what += " " + o.NetworkName
	}
	return fmt.Sprintf("%s, %s", what, formatNaira(o.Due()))
}

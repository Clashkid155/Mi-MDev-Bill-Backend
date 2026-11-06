package order

import (
	"context"
	"errors"
	"fmt"
	"strings"
)

// What the owner found out about a PENDING order.
const (
	OutcomeDelivered = "delivered" // ABS did deliver it
	OutcomeFailed    = "failed"    // ABS did not: the customer is owed their money back
)

// ErrNotPending means the order is not waiting for a decision.
var ErrNotPending = errors.New("order: only a PENDING order can be resolved")

// Resolve settles an order whose outcome ABS never made clear (a timeout, a 5xx, an unreadable reply):
// the server cannot know whether the customer got what they paid for and never buys again by itself,
// so the owner checks with ABS and says which it was.
//
//	delivered: the order becomes SUCCESS, and the customer gets their receipt (not for electricity, see below).
//	failed:    the order becomes FAILED and then REFUND_DUE, ready for the usual refund steps.
//
// It is safe against a second click or a late answer: the move only happens while the order is still
// PENDING, and otherwise ErrNotPending is returned with the order as it now is. by is who decided,
// for the events; the audit log records the request itself.
//
// An electricity order marked delivered gets no receipt, because the token is only in ABS's reply
// and that reply never arrived: the owner has to pass the token to the customer by hand.
func (s *Service) Resolve(ctx context.Context, id, outcome, note, by string) (Order, error) {
	o, err := s.Store.ByID(ctx, id)
	if err != nil {
		return Order{}, err
	}
	if o.Status != StatusPending {
		return o, ErrNotPending
	}

	switch outcome {
	case OutcomeDelivered:
		moved, err := s.Store.Move(ctx, id, StatusPending, StatusSuccess)
		if err != nil {
			return o, err
		}
		if !moved {
			return s.current(ctx, id)
		}
		o.Status = StatusSuccess
		s.event(LevelInfo, "order.resolved", id, resolvedMessage(o, "delivered", by, note), map[string]any{"outcome": outcome})
		if o.Kind != KindElectricity {
			s.sendReceipt(ctx, o, nil)
		}
	case OutcomeFailed:
		moved, err := s.Store.Move(ctx, id, StatusPending, StatusFailed)
		if err != nil {
			return o, err
		}
		if !moved {
			return s.current(ctx, id)
		}
		if _, err := s.Store.Move(ctx, id, StatusFailed, StatusRefundDue); err != nil {
			return o, err
		}
		s.event(LevelWarn, "order.resolved", id, resolvedMessage(o, "not delivered, refund due "+formatNaira(o.Due()), by, note),
			map[string]any{"outcome": outcome, "refund_due_kobo": o.Due()})
	default:
		return o, fmt.Errorf("order: unknown outcome %q", outcome)
	}
	return s.Store.ByID(ctx, id)
}

// current returns the order as it is now together with ErrNotPending: someone else settled it first.
func (s *Service) current(ctx context.Context, id string) (Order, error) {
	o, err := s.Store.ByID(ctx, id)
	if err != nil {
		return Order{}, err
	}
	return o, ErrNotPending
}

func resolvedMessage(o Order, what, by, note string) string {
	msg := fmt.Sprintf("%s: marked %s by %s after an unknown outcome", describe(o), what, by)
	if note = strings.TrimSpace(note); note != "" {
		msg += ": " + note
	}
	return msg
}

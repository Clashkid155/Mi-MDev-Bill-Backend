package order

import (
	"context"
	"errors"
	"fmt"
	"strings"
)

// ErrNotRetryable means the order is in a state the owner cannot buy again from.
var ErrNotRetryable = errors.New("order: only a PENDING or REFUND_DUE order can be bought again")

// ErrConfirmRequired means a PENDING order was not confirmed: nobody knows yet whether the first purchase went
// through, and buying again may deliver it twice.
var ErrConfirmRequired = errors.New("order: buying a PENDING order again needs confirmation")

// Retry is the owner's decision to buy an order from ABS again. The server never does this by itself: a purchase
// whose outcome is known to have failed (REFUND_DUE) or is not known at all (PENDING) stays as it is until the
// owner says so, because buying twice charges the owner twice and delivers twice.
//
//	REFUND_DUE: ABS failed the purchase, or the payment arrived after the order expired and nothing was bought.
//	            The customer's money is still held, so the order is bought again with it.
//	PENDING:    nobody knows whether the first purchase went through. confirm must be true: it says the owner
//	            has looked in the ABS dashboard and seen nothing delivered or is willing to risk it.
//
// The order moves to VENDING and the caller then runs Service.BuyAgain (in the background: ABS can take up to
// 90 seconds). Only one caller wins a double click: the move is compare-and-set. The outcome is recorded like
// any purchase's: SUCCESS (the customer gets their receipt), REFUND_DUE again, or PENDING again.
// by is who decided, for the event.
func (s *Service) Retry(ctx context.Context, id, note, by string, confirm bool) (Order, error) {
	o, err := s.Store.ByID(ctx, id)
	if err != nil {
		return Order{}, err
	}
	switch o.Status {
	case StatusRefundDue:
	case StatusPending:
		if !confirm {
			return o, ErrConfirmRequired
		}
	default:
		return o, ErrNotRetryable
	}
	from := o.Status
	moved, err := s.Store.RetryMove(ctx, id, from)
	if err != nil {
		return o, err
	}
	if !moved {
		return s.currentRetry(ctx, id)
	}
	o, err = s.Store.ByID(ctx, id)
	if err != nil {
		return o, err
	}
	msg := fmt.Sprintf("%s: bought again (attempt %d) after it was %s, by %s", describe(o), o.Attempts, from, by)
	if note = strings.TrimSpace(note); note != "" {
		msg += ": " + note
	}
	s.event(LevelWarn, "order.retried", id, msg, map[string]any{"attempt": o.Attempts, "was": string(from)})
	return o, nil
}

// BuyAgain makes the purchase for an order Retry has moved to VENDING and records the outcome.
func (s *Service) BuyAgain(ctx context.Context, o Order) error { return s.vendNow(ctx, o) }

// currentRetry returns the order as it is now together with ErrNotRetryable: someone else moved it first.
func (s *Service) currentRetry(ctx context.Context, id string) (Order, error) {
	o, err := s.Store.ByID(ctx, id)
	if err != nil {
		return Order{}, err
	}
	return o, ErrNotRetryable
}

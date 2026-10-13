package order

import (
	"fmt"
)

type Status string

const (
	StatusInitiated       Status = "INITIATED"
	StatusAwaitingPayment Status = "AWAITING_PAYMENT"
	StatusPaid            Status = "PAID"
	StatusPartiallyPaid   Status = "PARTIALLY_PAID" // less than the price arrived; not vended
	StatusOverpaid        Status = "OVERPAID"       // more than the price arrived; not vended
	StatusVending         Status = "VENDING"
	StatusSuccess         Status = "SUCCESS"
	StatusPending         Status = "PENDING" // vendor outcome unknown; reconciler resolves
	StatusFailed          Status = "FAILED"
	StatusRefundDue       Status = "REFUND_DUE"
	StatusRefunded        Status = "REFUNDED"
	StatusExpired         Status = "EXPIRED"
	StatusCancelled       Status = "CANCELLED"
)

var allStatuses = []Status{
	StatusInitiated, StatusAwaitingPayment, StatusPaid, StatusPartiallyPaid, StatusOverpaid, StatusVending, StatusSuccess, StatusPending,
	StatusFailed, StatusRefundDue, StatusRefunded, StatusExpired, StatusCancelled,
}

var transitions = map[Status][]Status{
	StatusInitiated:       {StatusAwaitingPayment, StatusCancelled},
	StatusAwaitingPayment: {StatusPaid, StatusExpired, StatusCancelled, StatusPartiallyPaid, StatusOverpaid},
	// A short payment can still be topped up to the exact price.
	StatusPartiallyPaid: {StatusPaid, StatusOverpaid},
	StatusPaid:          {StatusVending},
	StatusVending:       {StatusSuccess, StatusFailed, StatusPending},
	StatusPending:       {StatusSuccess, StatusFailed},
	StatusFailed:        {StatusRefundDue},
	StatusRefundDue:     {StatusRefunded},
	// An expired order is never vended. Monnify should refuse a late transfer, but if
	// money does arrive on one, it is refunded instead.
	StatusExpired: {StatusRefundDue},
}

func (s Status) CanTransitionTo(to Status) bool {
	for _, t := range transitions[s] {
		if t == to {
			return true
		}
	}
	return false
}

func (s Status) IsTerminal() bool {
	switch s {
	case StatusSuccess, StatusRefunded, StatusCancelled:
		return true
	}
	return false
}

// Check returns an error for an illegal transition. The store layer should
// still enforce it in SQL (UPDATE ... WHERE status = $from).
func Check(from, to Status) error {
	if !from.CanTransitionTo(to) {
		return fmt.Errorf("order: illegal transition %s -> %s", from, to)
	}
	return nil
}

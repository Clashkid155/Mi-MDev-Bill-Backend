package order

import (
	"fmt"
)

type Status string

const (
	StatusInitiated       Status = "INITIATED"
	StatusAwaitingPayment Status = "AWAITING_PAYMENT"
	StatusPaid            Status = "PAID"
	StatusVending         Status = "VENDING"
	StatusSuccess         Status = "SUCCESS"
	StatusPending         Status = "PENDING" // vendor outcome unknown; reconciler resolves
	StatusFailed          Status = "FAILED"
	StatusRefundDue       Status = "REFUND_DUE"
	StatusRefunded        Status = "REFUNDED"
	StatusExpired         Status = "EXPIRED"
	StatusCancelled       Status = "CANCELLED"
)

var transitions = map[Status][]Status{
	StatusInitiated:       {StatusAwaitingPayment, StatusCancelled},
	StatusAwaitingPayment: {StatusPaid, StatusExpired, StatusCancelled},
	StatusPaid:            {StatusVending},
	StatusVending:         {StatusSuccess, StatusFailed, StatusPending},
	StatusPending:         {StatusSuccess, StatusFailed},
	StatusFailed:          {StatusRefundDue},
	StatusRefundDue:       {StatusRefunded},
	// A payment can land on an invoice we already expired (late transfer).
	StatusExpired: {StatusPaid},
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

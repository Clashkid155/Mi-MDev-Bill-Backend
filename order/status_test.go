package order

import "testing"

func TestTransitions(t *testing.T) {
	ok := [][2]Status{
		{StatusInitiated, StatusAwaitingPayment},
		{StatusAwaitingPayment, StatusPaid},
		{StatusPaid, StatusVending},
		{StatusVending, StatusPending},
		{StatusPending, StatusSuccess},
		{StatusPending, StatusFailed},
		{StatusFailed, StatusRefundDue},
		{StatusAwaitingPayment, StatusExpired},
		{StatusAwaitingPayment, StatusPartiallyPaid},
		{StatusAwaitingPayment, StatusOverpaid},
		{StatusPartiallyPaid, StatusPaid},
		{StatusExpired, StatusRefundDue},
	}
	for _, c := range ok {
		if err := Check(c[0], c[1]); err != nil {
			t.Errorf("%s -> %s should be allowed: %v", c[0], c[1], err)
		}
	}
	bad := [][2]Status{
		{StatusSuccess, StatusFailed},
		{StatusPaid, StatusSuccess}, // must pass through VENDING
		{StatusPending, StatusRefundDue},
		{StatusAwaitingPayment, StatusVending},
		{StatusExpired, StatusPaid}, // an expired order is never vended
		{StatusExpired, StatusVending},
		{StatusOverpaid, StatusVending}, // wrong amounts are never vended
		{StatusPartiallyPaid, StatusVending},
		{StatusPartiallyPaid, StatusExpired}, // the customer did pay something
	}
	for _, c := range bad {
		if err := Check(c[0], c[1]); err == nil {
			t.Errorf("%s -> %s should be rejected", c[0], c[1])
		}
	}
}

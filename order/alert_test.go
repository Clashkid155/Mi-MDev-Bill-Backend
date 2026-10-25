package order

import (
	"context"
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"
)

const ownerEmail = "owner@example.com"

// alertsOn turns the alerts on for a test and returns a way to read what was sent
// to the owner (waiting for sends in flight first).
func alertsOn(t *testing.T, f *fakes) (*EmailAlerts, func() []string) {
	t.Helper()
	alerts := NewEmailAlerts(f.svc.Mailer, ownerEmail, slog.New(slog.NewTextHandler(io.Discard, nil)))
	f.svc.Alerts = alerts
	return alerts, func() []string {
		alerts.Wait()
		f.mu.Lock()
		defer f.mu.Unlock()
		var subjects []string
		for _, m := range f.allEmails {
			if m.To == ownerEmail {
				subjects = append(subjects, m.Subject)
			}
		}
		return subjects
	}
}

func (f *fakes) absFails(body string, status int32) {
	f.absStatus.Store(status)
	f.absBody.Store(body)
}

const insufficient = `{"status":false,"text":"Insufficient balance. Please fund your wallet and try again.","data":[],"error":{"code":141}}`

func orderWithKey(f *fakes, t *testing.T, key string) Order {
	t.Helper()
	req := airtimeRequest
	req.IdempotencyKey = key
	o, err := f.svc.Create(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	return o
}

func TestAnEmptyABSWalletAlertsTheOwnerOnceAndEveryRefundOnce(t *testing.T) {
	f := newFakes(t)
	_, sent := alertsOn(t, f)
	f.absFails(insufficient, 400)

	first := f.payAndVend(t, orderWithKey(f, t, "a1"))
	if first.Status != StatusRefundDue {
		t.Fatalf("status %s", first.Status)
	}
	subjects := sent()
	if len(subjects) != 2 {
		t.Fatalf("a refund and an empty wallet, got %q", subjects)
	}
	joined := strings.Join(subjects, " | ")
	if !strings.Contains(joined, "Refund due: ₦100.00, order "+first.ID) || !strings.Contains(joined, "ABS wallet is empty") {
		t.Errorf("subjects: %q", subjects)
	}
	f.mu.Lock()
	var refund string
	for _, m := range f.allEmails {
		if strings.Contains(m.Subject, "Refund due") {
			refund = m.Text
		}
	}
	f.mu.Unlock()
	for _, want := range []string{first.ID, "08012345678", "a@b.co", "Insufficient balance", "POST /admin/orders/{id}/refunded", "Kora dashboard"} {
		if !strings.Contains(refund, want) {
			t.Errorf("the refund alert is missing %q:\n%s", want, refund)
		}
	}

	// A second failing order: its own refund alert, but the wallet warning is not repeated within the hour.
	second := f.payAndVend(t, orderWithKey(f, t, "a2"))
	subjects = sent()
	if len(subjects) != 3 || !strings.Contains(subjects[2], second.ID) {
		t.Errorf("one more refund alert and no second wallet alert, got %q", subjects)
	}
}

func TestAnOrdinaryFailureAlertsAboutTheRefundOnly(t *testing.T) {
	f := newFakes(t)
	_, sent := alertsOn(t, f)
	f.absFails(`{"status":false,"text":"Invalid phone number","data":[]}`, 400)
	f.payAndVend(t, orderWithKey(f, t, "b1"))
	if subjects := sent(); len(subjects) != 1 || !strings.HasPrefix(subjects[0], "[Mi MDev] Refund due") {
		t.Errorf("%q", subjects)
	}
}

func TestAnUnknownOutcomeAlertsTheOwner(t *testing.T) {
	f := newFakes(t)
	_, sent := alertsOn(t, f)
	f.absFails(`gateway timeout`, 504)
	o := f.payAndVend(t, orderWithKey(f, t, "c1"))
	if o.Status != StatusPending {
		t.Fatalf("status %s", o.Status)
	}
	subjects := sent()
	if len(subjects) != 1 || !strings.Contains(subjects[0], "Outcome unknown") || !strings.Contains(subjects[0], o.ID) {
		t.Errorf("%q", subjects)
	}
}

func TestADeliveredOrderSendsTheOwnerNothing(t *testing.T) {
	f := newFakes(t)
	_, sent := alertsOn(t, f)
	if o := f.payAndVend(t, orderWithKey(f, t, "d1")); o.Status != StatusSuccess {
		t.Fatalf("status %s", o.Status)
	}
	if subjects := sent(); len(subjects) != 0 {
		t.Errorf("a normal sale needs no alert, got %q", subjects)
	}
}

func TestMoneyThatArrivesAfterExpiryAlertsTheOwner(t *testing.T) {
	f := newFakes(t)
	_, sent := alertsOn(t, f)
	ctx := context.Background()
	o := orderWithKey(f, t, "e1")
	f.svc.Settle(ctx, f.expire(t, o))
	f.invoiceState.Store("PAID")
	f.svc.ConfirmPayment(ctx, o.ID, o.TxnRef)
	subjects := sent()
	if len(subjects) != 1 || !strings.Contains(subjects[0], "Refund due") {
		t.Fatalf("%q", subjects)
	}
	f.mu.Lock()
	text := f.allEmails[len(f.allEmails)-1].Text
	f.mu.Unlock()
	if !strings.Contains(text, "after the order had expired") {
		t.Errorf("the reason should say so:\n%s", text)
	}
}

func TestAFailureToEmailNeverBreaksThePurchaseAndIsRetriedNextTime(t *testing.T) {
	f := newFakes(t)
	_, sent := alertsOn(t, f)
	f.absFails(`{"status":false,"text":"Invalid phone number"}`, 400)
	f.emailOK.Store(false) // the mail service is down
	o := f.payAndVend(t, orderWithKey(f, t, "f1"))
	if o.Status != StatusRefundDue {
		t.Fatalf("the order must still reach REFUND_DUE: %s", o.Status)
	}
	if subjects := sent(); len(subjects) != 0 {
		t.Fatalf("nothing could be delivered: %q", subjects)
	}
	// Not delivered means not marked as sent: the same alert goes out when mail is back.
	f.emailOK.Store(true)
	f.svc.alertRefundDue(o, "Invalid phone number")
	if subjects := sent(); len(subjects) != 1 {
		t.Errorf("the alert should be retried once mail works, got %q", subjects)
	}
}

func TestAlertsAreThrottledPerKeyAndComeBackLater(t *testing.T) {
	f := newFakes(t)
	alerts, sent := alertsOn(t, f)
	clock := time.Now()
	alerts.now = func() time.Time { return clock }
	alerts.Alert("k", time.Hour, "first", "body")
	alerts.Alert("k", time.Hour, "second", "body")
	alerts.Alert("other", time.Hour, "third", "body")
	if got := sent(); len(got) != 2 {
		t.Fatalf("the same key within the hour is one email: %q", got)
	}
	clock = clock.Add(61 * time.Minute)
	alerts.Alert("k", time.Hour, "fourth", "body")
	if got := sent(); len(got) != 3 {
		t.Errorf("after the hour it may be sent again: %q", got)
	}
}

func TestWithoutAlertsNothingIsSentAndNothingBreaks(t *testing.T) {
	f := newFakes(t) // no Alerts set
	f.absFails(insufficient, 400)
	if o := f.payAndVend(t, orderWithKey(f, t, "g1")); o.Status != StatusRefundDue {
		t.Fatalf("status %s", o.Status)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.allEmails) != 0 {
		t.Errorf("no alerts configured, yet %d emails were sent", len(f.allEmails))
	}
}

func TestAHighPaymentFeeAlertsTheOwner(t *testing.T) {
	f := newFakes(t)
	k := newKoraFake(t, f)
	_, sent := alertsOn(t, f)
	f.svc.FeeWarnPercent = 1.7
	ctx := context.Background()
	o := orderWithKey(f, t, "h1") // ₦100
	k.status.Store("success")
	k.acceptedKobo.Store(o.Due())
	k.feeKobo.Store(3_00) // 3%
	if paid, err := f.svc.ConfirmPayment(ctx, o.ID, o.TxnRef); err != nil || paid == nil {
		t.Fatalf("%v %v", paid, err)
	}
	subjects := sent()
	if len(subjects) != 1 || !strings.Contains(subjects[0], "Payment fee is 3.00%") {
		t.Errorf("%q", subjects)
	}
}

func TestALowABSWalletAlertsTheOwnerBeforeItRunsOut(t *testing.T) {
	f := newFakes(t) // the fake wallet holds ₦726
	_, sent := alertsOn(t, f)
	api := &API{Service: f.svc}
	ctx := context.Background()

	api.checkBalance(ctx, 100_00) // threshold ₦100: fine
	if got := sent(); len(got) != 0 {
		t.Fatalf("a healthy wallet needs no alert: %q", got)
	}
	api.checkBalance(ctx, 5_000_00) // threshold ₦5,000: low
	api.checkBalance(ctx, 5_000_00) // checked again a moment later: still one email
	got := sent()
	if len(got) != 1 || !strings.Contains(got[0], "ABS wallet is low: ₦726.00") {
		t.Errorf("%q", got)
	}
	f.profileDown.Store(true) // ABS cannot say: logged, no alert, no panic
	api.checkBalance(ctx, 5_000_00)
	if got := sent(); len(got) != 1 {
		t.Errorf("%q", got)
	}
}

func TestIsInsufficientBalance(t *testing.T) {
	for msg, want := range map[string]bool{
		"code 141: Insufficient balance. Please fund your wallet and try again.": true,
		"insufficient funds":   true,
		"Invalid phone number": false,
		"":                     false,
	} {
		if isInsufficientBalance(msg) != want {
			t.Errorf("%q: got %t", msg, !want)
		}
	}
}

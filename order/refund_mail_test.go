package order

import (
	"net/http/httptest"
	"strings"
	"testing"

	"Bills-Backend/platform"
)

// refundEmails returns the emails the customer was sent about a refund.
func (f *fakes) refundEmails(addr string) []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []string
	for _, m := range f.allEmails {
		if m.To == addr && strings.Contains(m.Subject, "refund") {
			out = append(out, m.Text+"\n"+m.HTML)
		}
	}
	return out
}

func TestTheCustomerIsEmailedWhenTheirRefundIsRecorded(t *testing.T) {
	f := newFakes(t)
	srv := httptest.NewServer((&API{Service: f.svc}).AdminRoutes())
	defer srv.Close()
	o := refundDue(t, f, "mail-full")
	f.svc.Store.db.Exec(`UPDATE orders SET customer_name = 'Ada', network_name = 'MTN', plan_name = '' WHERE id = ?`, o.ID)

	if f.refundEmails("a@b.co") != nil {
		t.Fatal("no refund email may go out before the refund is recorded")
	}
	if code, _ := postRefunded(t, srv, o.ID, `{"note":"sent from Kora"}`); code != 200 {
		t.Fatal(code)
	}
	mails := f.refundEmails("a@b.co")
	if len(mails) != 1 {
		t.Fatalf("%d refund emails, want 1", len(mails))
	}
	for _, want := range []string{"Hi Ada", "₦10,000.00", "Refunded", o.ID, "08012345678", "MTN", "bank account you paid from", "up to 7 days", "https://mi-mdev.dizztech.com/support"} {
		if !strings.Contains(mails[0], want) {
			t.Errorf("the email does not say %q:\n%s", want, mails[0])
		}
	}
	for _, leak := range []string{"sent from Kora", "Kept for transaction costs", "9,900", "161.25"} {
		if strings.Contains(mails[0], leak) {
			t.Errorf("a full refund's email mentions %q", leak)
		}
	}
	// Recording it again is refused and sends nothing more.
	if code, _ := postRefunded(t, srv, o.ID, `{"note":"again"}`); code != 409 {
		t.Errorf("a second record answered %d", code)
	}
	if n := len(f.refundEmails("a@b.co")); n != 1 {
		t.Errorf("%d refund emails after a repeat, want 1", n)
	}
}

func TestAPartialRefundEmailSaysWhatWasKept(t *testing.T) {
	f := newFakes(t)
	srv := httptest.NewServer((&API{Service: f.svc}).AdminRoutes())
	defer srv.Close()
	o := refundDue(t, f, "mail-partial")

	if code, _ := postRefunded(t, srv, o.ID, `{"refund_amount_kobo": 983875, "refund_fee_kobo": 5000}`); code != 200 {
		t.Fatal(code)
	}
	mails := f.refundEmails("a@b.co")
	if len(mails) != 1 {
		t.Fatalf("%d refund emails", len(mails))
	}
	for _, want := range []string{"₦9,838.75", "You paid", "₦10,000.00", "Kept for transaction costs", "₦161.25"} {
		if !strings.Contains(mails[0], want) {
			t.Errorf("the email does not say %q:\n%s", want, mails[0])
		}
	}
}

func TestARefundEmailEscapesTheCustomersName(t *testing.T) {
	f := newFakes(t)
	srv := httptest.NewServer((&API{Service: f.svc}).AdminRoutes())
	defer srv.Close()
	o := refundDue(t, f, "mail-name")
	f.svc.Store.db.Exec(`UPDATE orders SET customer_name = '<script>alert(1)</script>' WHERE id = ?`, o.ID)
	postRefunded(t, srv, o.ID, `{}`)
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, m := range f.allEmails {
		if strings.Contains(m.Subject, "refund") && strings.Contains(m.HTML, "<script>") {
			t.Errorf("the name was put into the page unescaped")
		}
	}
}

// A mail service that is down must not stop the refund being recorded: the owner is told instead.
func TestARefundIsRecordedEvenIfTheEmailCannotBeSent(t *testing.T) {
	f, read := eventsFixture(t)
	srv := httptest.NewServer((&API{Service: f.svc}).AdminRoutes())
	defer srv.Close()
	o := refundDue(t, f, "mail-down")
	f.emailOK.Store(false)

	if code, _ := postRefunded(t, srv, o.ID, `{}`); code != 200 {
		t.Fatalf("the refund answered %d", code)
	}
	if got := orderNow(t, f, o.ID); got.Status != StatusRefunded {
		t.Errorf("status %s", got.Status)
	}
	if ev := read(platform.EventFilter{OrderID: o.ID, Kind: "refund.notice_failed"}); len(ev) != 1 || ev[0].Level != LevelWarn {
		t.Errorf("events %+v", ev)
	}
}

func TestNoRefundEmailWithoutAnAddressOrAMailer(t *testing.T) {
	f := newFakes(t)
	srv := httptest.NewServer((&API{Service: f.svc}).AdminRoutes())
	defer srv.Close()
	o := refundDue(t, f, "mail-none")
	f.svc.Store.db.Exec(`UPDATE orders SET customer_email = '' WHERE id = ?`, o.ID)
	f.svc.Mailer = nil
	if code, _ := postRefunded(t, srv, o.ID, `{}`); code != 200 {
		t.Fatal(code)
	}
	if f.emailsSent.Load() != 0 {
		t.Errorf("%d emails sent", f.emailsSent.Load())
	}
}

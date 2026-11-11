package order

import (
	"bytes"
	"context"
	_ "embed"
	"fmt"
	"html/template"
	"strings"
	"time"

	"Bills-Backend/abs"
	"Bills-Backend/email"
)

// The refund-sent email: the customer is told when the owner records that their money has been sent back.
// refund.html is the HTML version (it shares its look with receipt.html); text() is the plain version.

//go:embed refund.html
var refundHTML string

var refundTemplate = template.Must(template.New("refund").Parse(refundHTML))

type refundMail struct {
	Name, Reference, Date string
	KindLower             string
	NetworkLabel, Network string
	Plan                  string
	RecipientLabel        string
	Recipient             string
	Refunded              string
	Paid, Deducted        string // set only when part of the payment was kept for transaction costs
	Preheader             string

	SupportURL, XHandle, XURL string
	Year                      int
}

func buildRefundMail(o Order) refundMail {
	at := o.RefundedAt
	if at.IsZero() {
		at = time.Now()
	}
	at = at.In(lagos)
	m := refundMail{
		Name: o.CustomerName, Reference: o.ID, Date: at.Format("2 Jan 2006, 3:04 PM MST"),
		Network: o.NetworkName, Plan: o.PlanName, Recipient: o.Recipient, Refunded: formatNaira(o.Refunded()),
		SupportURL: supportURL, XHandle: xHandle, XURL: xURL, Year: at.Year(),
	}
	if kept := o.Due() - o.Refunded(); kept > 0 {
		m.Paid, m.Deducted = formatNaira(o.Due()), formatNaira(kept)
	}
	switch o.Kind {
	case KindElectricity:
		m.KindLower, m.NetworkLabel, m.RecipientLabel = "electricity", "Service", "Meter number"
	case KindData:
		m.KindLower, m.NetworkLabel, m.RecipientLabel = "data", "Network", "Phone number"
		if abs.ValidRecipient("fn", o.Recipient) {
			m.RecipientLabel = "FibreX number"
		}
	default:
		m.KindLower, m.NetworkLabel, m.RecipientLabel = "airtime", "Network", "Phone number"
	}
	m.Preheader = fmt.Sprintf("%s refunded for your %s purchase", m.Refunded, m.KindLower)
	return m
}

func (m refundMail) html() (string, error) {
	var b bytes.Buffer
	if err := refundTemplate.Execute(&b, m); err != nil {
		return "", err
	}
	return b.String(), nil
}

func (m refundMail) text() string {
	var b strings.Builder
	fmt.Fprintf(&b, "Hi %s,\n\nWe could not complete your %s purchase, so we have refunded your payment.\n\nRefunded: %s\n", m.Name, m.KindLower, m.Refunded)
	if m.Deducted != "" {
		fmt.Fprintf(&b, "You paid: %s\nKept for transaction costs: %s\n", m.Paid, m.Deducted)
	}
	if m.Network != "" {
		fmt.Fprintf(&b, "%s: %s\n", m.NetworkLabel, m.Network)
	}
	if m.Plan != "" {
		fmt.Fprintf(&b, "Plan: %s\n", m.Plan)
	}
	fmt.Fprintf(&b, "%s: %s\nRefunded on: %s\nReference: %s\n", m.RecipientLabel, m.Recipient, m.Date, m.Reference)
	b.WriteString("\nThe money was sent back to the bank account you paid from. It can take up to 7 days to show in your account. If it has not arrived after 7 days, contact us and quote the reference above.\n")
	fmt.Fprintf(&b, "\nNeed help? Visit %s or find us on X, %s (%s). Please quote the reference above.\n", m.SupportURL, m.XHandle, m.XURL)
	b.WriteString("\nThis email can't receive replies. For help, visit our support page.\n")
	b.WriteString("\nThis is an automated message from Mi MDev.")
	return b.String()
}

// sendRefundNotice emails the customer that their refund was sent. Like the receipt it never fails the action
// that triggered it: a broken or unconfigured mailer is only logged.
func (s *Service) sendRefundNotice(ctx context.Context, o Order) {
	if s.Mailer == nil || o.CustomerEmail == "" {
		return
	}
	m := buildRefundMail(o)
	page, err := m.html()
	if err != nil {
		s.Log.Warn("SendRefundNotice - Order", "detail", fmt.Sprintf("order %s: html template: %v", o.ID, err))
	}
	msg := email.Message{
		To: o.CustomerEmail, ToName: o.CustomerName,
		Subject: "Your Mi MDev refund has been sent",
		Text:    m.text(), HTML: page,
	}
	if err := s.Mailer.Send(ctx, msg); err != nil {
		s.Log.Warn("SendRefundNotice - Order", "detail", fmt.Sprintf("order %s: %v", o.ID, err))
		s.event(LevelWarn, "refund.notice_failed", o.ID, "the customer's refund email could not be sent; tell them the refund was sent", nil)
	}
}

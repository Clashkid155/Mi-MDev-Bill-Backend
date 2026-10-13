package order

import (
	"bytes"
	_ "embed"
	"encoding/json"
	"fmt"
	"html/template"
	"strconv"
	"strings"
	"time"

	"Bills-Backend/abs"
)

// The purchase-successful email. receipt.html is the HTML version (edit it to
// change the design); text() is the plain version sent alongside it for mail
// clients that do not show HTML.

//go:embed receipt.html
var receiptHTML string

// html/template escapes every value, which matters: customer_name is typed by
// the user and ends up in this page.
var receiptTemplate = template.Must(template.New("receipt").Parse(receiptHTML))

const (
	supportURL = "https://mi-mdev.dizztech.com/support"
	xHandle    = "@dizzinc"
	xURL       = "https://x.com/dizzinc"
)

// lagos is Nigerian time (UTC+1, no daylight saving), the zone customers read dates in.
var lagos = time.FixedZone("WAT", 3600)

type receipt struct {
	Name, Reference, Date string
	Kind, KindLower       string
	NetworkLabel, Network string // Network is empty for orders from before names were stored
	Plan                  string
	RecipientLabel        string
	Recipient             string
	Amount                string
	Preheader             string

	// Electricity only. Token and Units come from ABS's reply; RawDelivery is
	// the whole reply block, shown only when no token could be read from it so
	// the customer is never left without what they paid for.
	Token, Units, RawDelivery string

	SupportURL, XHandle, XURL string
	Year                      int
}

func buildReceipt(o Order, delivery json.RawMessage, now time.Time) receipt {
	now = now.In(lagos)
	r := receipt{
		Name: o.CustomerName, Reference: o.ID, Date: now.Format("2 Jan 2006, 3:04 PM MST"),
		Network: o.NetworkName, Plan: o.PlanName, Recipient: o.Recipient, Amount: formatNaira(o.AmountKobo),
		SupportURL: supportURL, XHandle: xHandle, XURL: xURL, Year: now.Year(),
	}
	switch o.Kind {
	case KindElectricity:
		r.Kind, r.NetworkLabel, r.RecipientLabel = "Electricity", "Service", "Meter number"
		r.Token, r.Units, r.RawDelivery = readToken(delivery)
	case KindData:
		r.Kind, r.NetworkLabel, r.RecipientLabel = "Data", "Network", "Phone number"
		if abs.ValidRecipient("fn", o.Recipient) {
			r.RecipientLabel = "FibreX number"
		}
	default:
		r.Kind, r.NetworkLabel, r.RecipientLabel = "Airtime", "Network", "Phone number"
	}
	r.KindLower = strings.ToLower(r.Kind)
	r.Preheader = fmt.Sprintf("%s %s for %s", r.Amount, r.KindLower, r.Recipient)
	return r
}

func (r receipt) html() (string, error) {
	var b bytes.Buffer
	if err := receiptTemplate.Execute(&b, r); err != nil {
		return "", err
	}
	return b.String(), nil
}

func (r receipt) text() string {
	var b strings.Builder
	fmt.Fprintf(&b, "Hi %s,\n\nYour %s purchase was successful.\n\n%s\n", r.Name, r.KindLower, r.Amount)
	if r.Network != "" {
		fmt.Fprintf(&b, "%s: %s\n", r.NetworkLabel, r.Network)
	}
	if r.Plan != "" {
		fmt.Fprintf(&b, "Plan: %s\n", r.Plan)
	}
	fmt.Fprintf(&b, "%s: %s\nDate: %s\nReference: %s\n", r.RecipientLabel, r.Recipient, r.Date, r.Reference)
	switch {
	case r.Token != "":
		fmt.Fprintf(&b, "\nYour electricity token: %s\n", r.Token)
		if r.Units != "" {
			fmt.Fprintf(&b, "Units: %s\n", r.Units)
		}
	case r.RawDelivery != "":
		fmt.Fprintf(&b, "\nYour token details: %s\n", r.RawDelivery)
	}
	fmt.Fprintf(&b, "\nNeed help? Visit %s or find us on X, %s (%s). Please quote the reference above.\n", r.SupportURL, r.XHandle, r.XURL)
	b.WriteString("\nThis email can't receive replies. For help, visit our support page.\n")
	b.WriteString("\nThis is an automated receipt from Mi MDev.")
	return b.String()
}

// readToken pulls the token and units out of ABS's electricity reply, e.g.
// {"token": "5296-7725-2448-4408-1616", "unit": 4.4}. Anything unrecognised
// comes back as raw instead, never dropped.
func readToken(delivery json.RawMessage) (token, units, raw string) {
	if len(delivery) == 0 {
		return "", "", ""
	}
	var d struct {
		Token any `json:"token"`
		Unit  any `json:"unit"`
	}
	if json.Unmarshal(delivery, &d) == nil {
		token, units = scalar(d.Token), scalar(d.Unit)
	}
	if token == "" {
		return "", "", string(delivery)
	}
	return token, units, ""
}

// scalar renders a JSON string or number for display. ABS sends "unit" with
// float noise (4.4000000000000003552713678800500929355621337890625); parsing it
// as a float and printing the shortest form gives back 4.4.
func scalar(v any) string {
	switch x := v.(type) {
	case string:
		return strings.TrimSpace(x)
	case float64:
		return strconv.FormatFloat(x, 'f', -1, 64)
	}
	return ""
}

// formatNaira renders kobo as ₦1,234.50.
func formatNaira(kobo int64) string {
	whole := strconv.FormatInt(kobo/100, 10)
	var groups []string
	for len(whole) > 3 {
		groups = append([]string{whole[len(whole)-3:]}, groups...)
		whole = whole[:len(whole)-3]
	}
	groups = append([]string{whole}, groups...)
	return fmt.Sprintf("₦%s.%02d", strings.Join(groups, ","), kobo%100)
}
